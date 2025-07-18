package networkqos

import (
	"fmt"
	"reflect"
	"sync"
	"time"

	nadinformerv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/informers/externalversions/k8s.cni.cncf.io/v1"
	nadlisterv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/listers/k8s.cni.cncf.io/v1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	corev1informers "k8s.io/client-go/informers/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	networkqosapi "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1"
	networkqosclientset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1/apis/clientset/versioned"
	networkqosinformer "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1/apis/informers/externalversions/networkqos/v1alpha1"
	networkqoslister "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/networkqos/v1alpha1/apis/listers/networkqos/v1alpha1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/syncmap"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

const (
	// maxRetries is the number of times a object will be retried before it is dropped out of the queue.
	// With the current rate-limiter in use (5ms*2^(maxRetries-1)) the following numbers represent the
	// sequence of delays between successive queuings of an object.
	//
	// 5ms, 10ms, 20ms, 40ms, 80ms, 160ms, 320ms, 640ms, 1.3s, 2.6s, 5.1s, 10.2s, 20.4s, 41s, 82s
	maxRetries = 15
)

// Controller holds the fields required for NQOS controller
// taken from k8s controller guidelines
type Controller struct {
	// name of the controller that starts the NQOS controller
	// (values are default-network-controller, secondary-network-controller etc..)
	controllerName string
	util.NetInfo
	nqosClientSet networkqosclientset.Interface

	// libovsdb northbound client interface
	nbClient      libovsdbclient.Client
	eventRecorder record.EventRecorder
	// An address set factory that creates address sets
	addressSetFactory addressset.AddressSetFactory
	// pass in the isPodScheduledinLocalZone util from bnc - used only to determine
	// what zones the pods are in.
	// isPodScheduledinLocalZone returns whether the provided pod is in a zone local to the zone controller
	// So if pod is not scheduled yet it is considered remote. Also if we can't fetch node from kapi and determing the zone,
	// we consider it remote - this is ok for this controller as this variable is only used to
	// determine if we need to add pod's port to port group or not - future updates should
	// take care of reconciling the state of the cluster
	isPodScheduledinLocalZone func(*corev1.Pod) bool
	// store's the name of the zone that this controller belongs to
	zone string

	// namespace+name -> cloned value of NetworkQoS
	nqosCache *syncmap.SyncMap[*networkQoSState]

	// queues for the CRDs where incoming work is placed to de-dup
	nqosQueue workqueue.TypedRateLimitingInterface[string]
	// cached access to nqos objects
	nqosLister      networkqoslister.NetworkQoSLister
	nqosCacheSynced cache.InformerSynced
	// namespace queue, cache, lister
	nqosNamespaceLister corev1listers.NamespaceLister
	nqosNamespaceSynced cache.InformerSynced
	nqosNamespaceQueue  workqueue.TypedRateLimitingInterface[*eventData[*corev1.Namespace]]
	// pod queue, cache, lister
	nqosPodLister corev1listers.PodLister
	nqosPodSynced cache.InformerSynced
	nqosPodQueue  workqueue.TypedRateLimitingInterface[*eventData[*corev1.Pod]]
	// node queue, cache, lister
	nqosNodeLister corev1listers.NodeLister
	nqosNodeSynced cache.InformerSynced
	nqosNodeQueue  workqueue.TypedRateLimitingInterface[string]

	// nad lister, only valid for default network controller when multi-network is enabled
	nadLister nadlisterv1.NetworkAttachmentDefinitionLister
	nadSynced cache.InformerSynced
}

type eventData[T metav1.Object] struct {
	old T
	new T
}

func newEventData[T metav1.Object](old T, new T) *eventData[T] {
	return &eventData[T]{
		old: old,
		new: new,
	}
}

func (e *eventData[T]) name() string {
	if !reflect.ValueOf(e.old).IsNil() {
		return e.old.GetName()
	} else if !reflect.ValueOf(e.new).IsNil() {
		return e.new.GetName()
	}
	return ""
}

func (e *eventData[T]) namespace() string {
	if !reflect.ValueOf(e.old).IsNil() {
		return e.old.GetNamespace()
	} else if !reflect.ValueOf(e.new).IsNil() {
		return e.new.GetNamespace()
	}
	return ""
}

// NewController returns a new *Controller.
func NewController(
	controllerName string,
	netInfo util.NetInfo,
	nbClient libovsdbclient.Client,
	recorder record.EventRecorder,
	nqosClient networkqosclientset.Interface,
	nqosInformer networkqosinformer.NetworkQoSInformer,
	namespaceInformer corev1informers.NamespaceInformer,
	podInformer corev1informers.PodInformer,
	nodeInformer corev1informers.NodeInformer,
	nadInformer nadinformerv1.NetworkAttachmentDefinitionInformer,
	addressSetFactory addressset.AddressSetFactory,
	isPodScheduledinLocalZone func(*corev1.Pod) bool,
	zone string) (*Controller, error) {

	c := &Controller{
		controllerName:            controllerName,
		NetInfo:                   netInfo,
		nbClient:                  nbClient,
		nqosClientSet:             nqosClient,
		addressSetFactory:         addressSetFactory,
		isPodScheduledinLocalZone: isPodScheduledinLocalZone,
		zone:                      zone,
		nqosCache:                 syncmap.NewSyncMap[*networkQoSState](),
	}

	klog.V(5).Infof("Setting up event handlers for Network QoS controller %s", controllerName)
	// setup nqos informers, listers, queue
	c.nqosLister = nqosInformer.Lister()
	c.nqosCacheSynced = nqosInformer.Informer().HasSynced
	c.nqosQueue = workqueue.NewTypedRateLimitingQueueWithConfig(
		workqueue.NewTypedItemFastSlowRateLimiter[string](1*time.Second, 5*time.Second, 5),
		workqueue.TypedRateLimitingQueueConfig[string]{Name: "networkQoS"},
	)
	_, err := nqosInformer.Informer().AddEventHandler(factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onNQOSAdd,
		UpdateFunc: c.onNQOSUpdate,
		DeleteFunc: c.onNQOSDelete,
	}))
	if err != nil {
		return nil, fmt.Errorf("could not add Event Handler for nqosInformer during network qos controller initialization, %w", err)
	}

	klog.V(5).Info("Setting up event handlers for Namespaces in Network QoS controller")
	c.nqosNamespaceLister = namespaceInformer.Lister()
	c.nqosNamespaceSynced = namespaceInformer.Informer().HasSynced
	c.nqosNamespaceQueue = workqueue.NewTypedRateLimitingQueueWithConfig(
		workqueue.NewTypedItemFastSlowRateLimiter[*eventData[*corev1.Namespace]](1*time.Second, 5*time.Second, 5),
		workqueue.TypedRateLimitingQueueConfig[*eventData[*corev1.Namespace]]{Name: "nqosNamespaces"},
	)
	_, err = namespaceInformer.Informer().AddEventHandler(factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onNQOSNamespaceAdd,
		UpdateFunc: c.onNQOSNamespaceUpdate,
		DeleteFunc: c.onNQOSNamespaceDelete,
	}))
	if err != nil {
		return nil, fmt.Errorf("could not add Event Handler for namespace Informer during network qos controller initialization, %w", err)
	}

	klog.V(5).Info("Setting up event handlers for Pods in Network QoS controller")
	c.nqosPodLister = podInformer.Lister()
	c.nqosPodSynced = podInformer.Informer().HasSynced
	c.nqosPodQueue = workqueue.NewTypedRateLimitingQueueWithConfig(
		workqueue.NewTypedItemFastSlowRateLimiter[*eventData[*corev1.Pod]](1*time.Second, 5*time.Second, 5),
		workqueue.TypedRateLimitingQueueConfig[*eventData[*corev1.Pod]]{Name: "nqosPods"},
	)
	_, err = podInformer.Informer().AddEventHandler(factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onNQOSPodAdd,
		UpdateFunc: c.onNQOSPodUpdate,
		DeleteFunc: c.onNQOSPodDelete,
	}))
	if err != nil {
		return nil, fmt.Errorf("could not add Event Handler for pod Informer during network qos controller initialization, %w", err)
	}

	klog.V(5).Info("Setting up event handlers for Nodes in Network QoS controller")
	c.nqosNodeLister = nodeInformer.Lister()
	c.nqosNodeSynced = nodeInformer.Informer().HasSynced
	c.nqosNodeQueue = workqueue.NewTypedRateLimitingQueueWithConfig(
		workqueue.NewTypedItemFastSlowRateLimiter[string](1*time.Second, 5*time.Second, 5),
		workqueue.TypedRateLimitingQueueConfig[string]{Name: "nqosNodes"},
	)
	_, err = nodeInformer.Informer().AddEventHandler(factory.WithUpdateHandlingForObjReplace(cache.ResourceEventHandlerFuncs{
		UpdateFunc: c.onNQOSNodeUpdate,
	}))
	if err != nil {
		return nil, fmt.Errorf("could not add Event Handler for node Informer during network qos controller initialization, %w", err)
	}

	if nadInformer != nil {
		c.nadLister = nadInformer.Lister()
		c.nadSynced = nadInformer.Informer().HasSynced
	}

	c.eventRecorder = recorder
	return c, nil
}

// Run will not return until stopCh is closed. workers determines how many
// objects (pods, namespaces, nqoses) will be handled in parallel.
func (c *Controller) Run(threadiness int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()

	klog.Infof("Starting controller %s", c.controllerName)

	// Wait for the caches to be synced
	klog.V(5).Info("Waiting for informer caches (networkqos,namespace,pod,node) to sync")
	if !util.WaitForInformerCacheSyncWithTimeout(c.controllerName, stopCh, c.nqosCacheSynced, c.nqosNamespaceSynced, c.nqosPodSynced, c.nqosNodeSynced) {
		utilruntime.HandleError(fmt.Errorf("timed out waiting for informer caches (networkqos,namespace,pod,node) to sync"))
		return
	}
	if c.nadSynced != nil {
		klog.V(5).Info("Waiting for net-attach-def informer cache to sync")
		if !util.WaitForInformerCacheSyncWithTimeout(c.controllerName, stopCh, c.nadSynced) {
			utilruntime.HandleError(fmt.Errorf("timed out waiting for net-attach-def informer cache to sync"))
			return
		}
	}

	klog.Infof("Repairing Network QoSes")
	// Run the repair function at startup so that we synchronize KAPI and OVNDBs
	err := c.repairNetworkQoSes()
	if err != nil {
		klog.Errorf("Failed to repair Network QoS: %v", err)
	}

	wg := &sync.WaitGroup{}
	// Start the workers after the repair loop to avoid races
	klog.V(5).Info("Starting Network QoS workers")
	for i := 0; i < threadiness; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wait.Until(func() {
				c.runNQOSWorker(wg)
			}, time.Second, stopCh)
		}()
	}

	klog.V(5).Info("Starting Namespace Network QoS workers")
	for i := 0; i < threadiness; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wait.Until(func() {
				c.runNQOSNamespaceWorker(wg)
			}, time.Second, stopCh)
		}()
	}

	klog.V(5).Info("Starting Pod Network QoS workers")
	for i := 0; i < threadiness; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wait.Until(func() {
				c.runNQOSPodWorker(wg)
			}, time.Second, stopCh)
		}()
	}

	klog.V(5).Info("Starting Node Network QoS workers")
	for i := 0; i < threadiness; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wait.Until(func() {
				c.runNQOSNodeWorker(wg)
			}, time.Second, stopCh)
		}()
	}

	<-stopCh

	klog.Infof("Shutting down controller %s", c.controllerName)
	c.nqosQueue.ShutDown()
	c.nqosNamespaceQueue.ShutDown()
	c.nqosPodQueue.ShutDown()
	c.nqosNodeQueue.ShutDown()
	c.teardownMetricsCollector()
	wg.Wait()
}

// worker runs a worker thread that just dequeues items, processes them, and
// marks them done. You may run as many of these in parallel as you wish; the
// workqueue guarantees that they will not end up processing the same object
// at the same time.
func (c *Controller) runNQOSWorker(wg *sync.WaitGroup) {
	for c.processNextNQOSWorkItem(wg) {
	}
}

func (c *Controller) runNQOSNamespaceWorker(wg *sync.WaitGroup) {
	for c.processNextNQOSNamespaceWorkItem(wg) {
	}
}

func (c *Controller) runNQOSPodWorker(wg *sync.WaitGroup) {
	for c.processNextNQOSPodWorkItem(wg) {
	}
}

func (c *Controller) runNQOSNodeWorker(wg *sync.WaitGroup) {
	for c.processNextNQOSNodeWorkItem(wg) {
	}
}

// handlers

// onNQOSAdd queues the NQOS for processing.
func (c *Controller) onNQOSAdd(obj any) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	c.nqosQueue.Add(key)
}

// onNQOSUpdate updates the NQOS Selector in the cache and queues the NQOS for processing.
func (c *Controller) onNQOSUpdate(oldObj, newObj any) {
	oldNQOS, ok := oldObj.(*networkqosapi.NetworkQoS)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("expecting NetworkQoS but received %T", oldObj))
		return
	}
	newNQOS, ok := newObj.(*networkqosapi.NetworkQoS)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("expecting NetworkQoS but received %T", newObj))
		return
	}
	// don't process resync or objects that are marked for deletion
	if oldNQOS.ResourceVersion == newNQOS.ResourceVersion ||
		!newNQOS.GetDeletionTimestamp().IsZero() {
		return
	}
	if reflect.DeepEqual(oldNQOS.Spec, newNQOS.Spec) {
		return
	}
	key, err := cache.MetaNamespaceKeyFunc(newObj)
	if err == nil {
		// updates to NQOS object should be very rare, once put in place they usually stay the same
		klog.V(4).Infof("Updating Network QoS %s: nqosSpec %v",
			key, newNQOS.Spec)
		c.nqosQueue.Add(key)
	}
}

// onNQOSDelete queues the NQOS for processing.
func (c *Controller) onNQOSDelete(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	c.nqosQueue.Add(key)
}

// onNQOSNamespaceAdd queues the namespace for processing.
func (c *Controller) onNQOSNamespaceAdd(obj interface{}) {
	ns, ok := obj.(*corev1.Namespace)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("expecting Namespace but received %T", obj))
		return
	}
	if ns == nil {
		utilruntime.HandleError(fmt.Errorf("empty namespace"))
		return
	}
	c.nqosNamespaceQueue.Add(newEventData(nil, ns))
}

// onNQOSNamespaceUpdate queues the namespace for processing.
func (c *Controller) onNQOSNamespaceUpdate(oldObj, newObj interface{}) {
	oldNamespace, ok := oldObj.(*corev1.Namespace)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("expecting Namespace but received %T", oldObj))
		return
	}
	newNamespace, ok := newObj.(*corev1.Namespace)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("expecting Namespace but received %T", newObj))
		return
	}
	if oldNamespace == nil || newNamespace == nil {
		utilruntime.HandleError(fmt.Errorf("empty namespace"))
		return
	}
	if oldNamespace.ResourceVersion == newNamespace.ResourceVersion || !newNamespace.GetDeletionTimestamp().IsZero() {
		return
	}
	// If the labels have not changed, then there's no change that we care about: return.
	oldNamespaceLabels := labels.Set(oldNamespace.Labels)
	newNamespaceLabels := labels.Set(newNamespace.Labels)
	if labels.Equals(oldNamespaceLabels, newNamespaceLabels) {
		return
	}
	klog.V(5).Infof("Namespace %s labels have changed: %v", newNamespace.Name, newNamespaceLabels)
	c.nqosNamespaceQueue.Add(newEventData(oldNamespace, newNamespace))
}

// onNQOSNamespaceDelete queues the namespace for processing.
func (c *Controller) onNQOSNamespaceDelete(obj interface{}) {
	ns, ok := obj.(*corev1.Namespace)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("couldn't get object from tombstone %#v", obj))
			return
		}
		ns, ok = tombstone.Obj.(*corev1.Namespace)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("tombstone contained object that is not a Namespace: %#v", tombstone.Obj))
			return
		}
	}
	if ns != nil {
		c.nqosNamespaceQueue.Add(newEventData(ns, nil))
	}
}

// onNQOSPodAdd queues the pod for processing.
func (c *Controller) onNQOSPodAdd(obj interface{}) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("expecting Pod but received %T", obj))
		return
	}
	if pod == nil {
		utilruntime.HandleError(fmt.Errorf("empty pod"))
		return
	}
	c.nqosPodQueue.Add(newEventData(nil, pod))
}

// onNQOSPodUpdate queues the pod for processing.
func (c *Controller) onNQOSPodUpdate(oldObj, newObj interface{}) {
	oldPod, ok := oldObj.(*corev1.Pod)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("expecting Pod but received %T", oldObj))
		return
	}
	newPod, ok := newObj.(*corev1.Pod)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("expecting Pod but received %T", newObj))
		return
	}
	if oldPod == nil || newPod == nil {
		utilruntime.HandleError(fmt.Errorf("empty pod"))
		return
	}
	// don't process resync or objects that are marked for deletion
	if oldPod.ResourceVersion == newPod.ResourceVersion ||
		!newPod.GetDeletionTimestamp().IsZero() {
		return
	}
	// We only care about pod's label changes, pod's IP changes
	// pod going into completed state and pod getting scheduled and switching
	// zones. Rest of the cases we may return
	oldPodLabels := labels.Set(oldPod.Labels)
	newPodLabels := labels.Set(newPod.Labels)
	oldPodIPs, _ := util.GetPodIPsOfNetwork(oldPod, c.NetInfo)
	newPodIPs, _ := util.GetPodIPsOfNetwork(newPod, c.NetInfo)
	oldPodCompleted := util.PodCompleted(oldPod)
	newPodCompleted := util.PodCompleted(newPod)
	if labels.Equals(oldPodLabels, newPodLabels) &&
		// check for podIP changes (in case we allocate and deallocate) or for dualstack conversion
		// it will also catch the pod update that will come when LSPAdd and IPAM allocation are done
		len(oldPodIPs) == len(newPodIPs) &&
		oldPodCompleted == newPodCompleted {
		return
	}
	klog.V(5).Infof("Handling update event for pod %s/%s, labels %v, podIPs: %v, PodCompleted?: %v", newPod.Namespace, newPod.Name, newPodLabels, newPodIPs, newPodCompleted)
	c.nqosPodQueue.Add(newEventData(oldPod, newPod))
}

// onNQOSPodDelete queues the pod for processing.
func (c *Controller) onNQOSPodDelete(obj interface{}) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("couldn't get object from tombstone %#v", obj))
			return
		}
		pod, ok = tombstone.Obj.(*corev1.Pod)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("tombstone contained object that is not a Pod: %#v", tombstone.Obj))
			return
		}
	}
	if pod != nil {
		c.nqosPodQueue.Add(newEventData(pod, nil))
	}
}

// onNQOSNodeUpdate queues the node for processing.
func (c *Controller) onNQOSNodeUpdate(oldObj, newObj interface{}) {
	oldNode, ok := oldObj.(*corev1.Node)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("expecting Node but received %T", oldObj))
		return
	}
	newNode, ok := newObj.(*corev1.Node)
	if !ok {
		utilruntime.HandleError(fmt.Errorf("expecting Node but received %T", newObj))
		return
	}
	// don't process resync or objects that are marked for deletion
	if oldNode.ResourceVersion == newNode.ResourceVersion ||
		!newNode.GetDeletionTimestamp().IsZero() {
		return
	}
	// node not in local zone, no need to process
	if !c.isNodeInLocalZone(oldNode) && !c.isNodeInLocalZone(newNode) {
		return
	}
	// only care about node's zone name changes
	if !util.NodeZoneAnnotationChanged(oldNode, newNode) {
		return
	}
	klog.V(4).Infof("Node %s zone changed from %s to %s", newNode.Name, oldNode.Annotations[util.OvnNodeZoneName], newNode.Annotations[util.OvnNodeZoneName])
	key, err := cache.MetaNamespaceKeyFunc(newObj)
	if err == nil {
		c.nqosNodeQueue.Add(key)
	}
}
