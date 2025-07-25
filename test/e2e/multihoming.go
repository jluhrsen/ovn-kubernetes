package e2e

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/feature"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"

	mnpapi "github.com/k8snetworkplumbingwg/multi-networkpolicy/pkg/apis/k8s.cni.cncf.io/v1beta1"
	mnpclient "github.com/k8snetworkplumbingwg/multi-networkpolicy/pkg/client/clientset/versioned/typed/k8s.cni.cncf.io/v1beta1"
	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadclient "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned/typed/k8s.cni.cncf.io/v1"

	ipgenerator "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/generator/ip"
	util "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/deploymentconfig"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/images"
	"github.com/ovn-org/ovn-kubernetes/test/e2e/infraprovider"
	infraapi "github.com/ovn-org/ovn-kubernetes/test/e2e/infraprovider/api"
)

const (
	PolicyForAnnotation = "k8s.v1.cni.cncf.io/policy-for"
	nodeHostnameKey     = "kubernetes.io/hostname"
)

var _ = Describe("Multi Homing", feature.MultiHoming, func() {
	const (
		podName                      = "tinypod"
		secondaryNetworkCIDR         = "10.128.0.0/16"
		secondaryNetworkName         = "tenant-blue"
		secondaryFlatL2IgnoreCIDR    = "10.128.0.0/29"
		secondaryFlatL2NetworkCIDR   = "10.128.0.0/24"
		secondaryLocalnetIgnoreCIDR  = "60.128.0.0/29"
		secondaryLocalnetNetworkCIDR = "60.128.0.0/24"
		netPrefixLengthPerNode       = 24
		localnetVLANID               = 10
		secondaryIPv6CIDR            = "2010:100:200::0/60"
		netPrefixLengthIPv6PerNode   = 64
	)
	f := wrappedTestFramework("multi-homing")

	var (
		cs          clientset.Interface
		nadClient   nadclient.K8sCniCncfIoV1Interface
		mnpClient   mnpclient.K8sCniCncfIoV1beta1Interface
		providerCtx infraapi.Context
	)

	BeforeEach(func() {
		cs = f.ClientSet

		var err error
		nadClient, err = nadclient.NewForConfig(f.ClientConfig())
		Expect(err).NotTo(HaveOccurred())
		mnpClient, err = mnpclient.NewForConfig(f.ClientConfig())
		Expect(err).NotTo(HaveOccurred())
		providerCtx = infraprovider.Get().NewTestContext()
	})

	Context("A single pod with an OVN-K secondary network", func() {
		ginkgo.DescribeTable("is able to get to the Running phase", func(netConfigParams networkAttachmentConfigParams, podConfig podConfiguration) {
			netConfig := newNetworkAttachmentConfig(netConfigParams)

			netConfig.namespace = f.Namespace.Name
			podConfig.namespace = f.Namespace.Name

			if netConfig.topology == "localnet" {
				By("applying ovs bridge mapping")
				Expect(providerCtx.SetupUnderlay(f, infraapi.Underlay{
					LogicalNetworkName: netConfig.networkName,
					VlanID:             netConfig.vlanID,
				})).To(Succeed())
			}

			By("creating the attachment configuration")
			_, err := nadClient.NetworkAttachmentDefinitions(netConfig.namespace).Create(
				context.Background(),
				generateNAD(netConfig),
				metav1.CreateOptions{},
			)
			Expect(err).NotTo(HaveOccurred())

			By("creating the pod using a secondary network")
			pod, err := cs.CoreV1().Pods(podConfig.namespace).Create(
				context.Background(),
				generatePodSpec(podConfig),
				metav1.CreateOptions{},
			)
			Expect(err).NotTo(HaveOccurred())

			By("asserting the pod gets to the `Ready` phase")
			Eventually(func() v1.PodPhase {
				updatedPod, err := cs.CoreV1().Pods(podConfig.namespace).Get(context.Background(), pod.GetName(), metav1.GetOptions{})
				if err != nil {
					return v1.PodFailed
				}
				return updatedPod.Status.Phase
			}, 2*time.Minute, 6*time.Second).Should(Equal(v1.PodRunning))

			if netConfig.excludeCIDRs != nil {
				podIP, err := podIPForAttachment(cs, pod.GetNamespace(), pod.GetName(), secondaryNetworkName, 0)
				Expect(err).NotTo(HaveOccurred())
				subnet, err := getNetCIDRSubnet(netConfig.cidr)
				Expect(err).NotTo(HaveOccurred())
				Expect(inRange(subnet, podIP)).To(Succeed())
				for _, excludedRange := range netConfig.excludeCIDRs {
					Expect(inRange(excludedRange, podIP)).To(
						MatchError(fmt.Errorf("ip [%s] is NOT in range %s", podIP, excludedRange)))
				}
			}
		},
			ginkgo.Entry(
				"when attaching to an L3 - routed - network",
				networkAttachmentConfigParams{
					cidr:     netCIDR(secondaryNetworkCIDR, netPrefixLengthPerNode),
					name:     secondaryNetworkName,
					topology: "layer3",
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        podName,
				},
			),
			ginkgo.Entry(
				"when attaching to an L3 - routed - network with IPv6 network",
				networkAttachmentConfigParams{
					cidr:     netCIDR(secondaryIPv6CIDR, netPrefixLengthIPv6PerNode),
					name:     secondaryNetworkName,
					topology: "layer3",
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        podName,
				},
			),
			ginkgo.Entry(
				"when attaching to an L2 - switched - network",
				networkAttachmentConfigParams{
					cidr:     secondaryFlatL2NetworkCIDR,
					name:     secondaryNetworkName,
					topology: "layer2",
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        podName,
				},
			),
			ginkgo.Entry(
				"when attaching to an L2 - switched - network featuring `excludeCIDR`s",
				networkAttachmentConfigParams{
					cidr:         secondaryFlatL2NetworkCIDR,
					name:         secondaryNetworkName,
					topology:     "layer2",
					excludeCIDRs: []string{secondaryFlatL2IgnoreCIDR},
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        podName,
				},
			),
			ginkgo.Entry(
				"when attaching to an L2 - switched - network without IPAM",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "layer2",
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        podName,
				},
			),
			ginkgo.Entry(
				"when attaching to an L2 - switched - network with an IPv6 subnet",
				networkAttachmentConfigParams{
					cidr:     secondaryIPv6CIDR,
					name:     secondaryNetworkName,
					topology: "layer2",
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        podName,
				},
			),
			ginkgo.Entry(
				"when attaching to an L2 - switched - network with a dual stack configuration",
				networkAttachmentConfigParams{
					cidr:     strings.Join([]string{secondaryFlatL2NetworkCIDR, secondaryIPv6CIDR}, ","),
					name:     secondaryNetworkName,
					topology: "layer2",
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        podName,
				},
			),
			ginkgo.Entry(
				"when attaching to a localnet - switched - network",
				networkAttachmentConfigParams{
					cidr:     secondaryLocalnetNetworkCIDR,
					name:     secondaryNetworkName,
					topology: "localnet",
					vlanID:   localnetVLANID,
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        podName,
				},
			),
			ginkgo.Entry(
				"when attaching to a localnet - switched - network featuring `excludeCIDR`s",
				networkAttachmentConfigParams{
					cidr:         secondaryLocalnetNetworkCIDR,
					name:         secondaryNetworkName,
					topology:     "localnet",
					excludeCIDRs: []string{secondaryLocalnetIgnoreCIDR},
					vlanID:       localnetVLANID,
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        podName,
				},
			),
			ginkgo.Entry(
				"when attaching to a localnet - switched - network without IPAM",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "localnet",
					vlanID:   localnetVLANID,
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        podName,
				},
			),
			ginkgo.Entry(
				"when attaching to a localnet - switched - network with an IPv6 subnet",
				networkAttachmentConfigParams{
					cidr:     secondaryIPv6CIDR,
					name:     secondaryNetworkName,
					topology: "localnet",
					vlanID:   localnetVLANID,
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        podName,
				},
			),
			ginkgo.Entry(
				"when attaching to an L2 - switched - network with a dual stack configuration",
				networkAttachmentConfigParams{
					cidr:     strings.Join([]string{secondaryLocalnetNetworkCIDR, secondaryIPv6CIDR}, ","),
					name:     secondaryNetworkName,
					topology: "localnet",
					vlanID:   localnetVLANID,
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        podName,
				},
			),
		)

		const (
			clientPodName  = "client-pod"
			clientIPOffset = 100
			serverIPOffset = 102
			port           = 9000
		)

		ginkgo.DescribeTable("attached to a localnet network mapped to external primary interface bridge", //nolint:lll

			func(netConfigParams networkAttachmentConfigParams, clientPodConfig, serverPodConfig podConfiguration, isCollocatedPods bool) {
				By("Get two scheduable nodes and ensure client and server are located on distinct Nodes")
				nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.Background(), f.ClientSet, 2)
				framework.ExpectNoError(err, "2 scheduable nodes are required")
				Expect(len(nodes.Items)).To(BeNumerically(">=", 1), "cluster should have at least 2 nodes")
				if isCollocatedPods {
					clientPodConfig.nodeSelector = map[string]string{nodeHostnameKey: nodes.Items[0].GetName()}
					serverPodConfig.nodeSelector = map[string]string{nodeHostnameKey: nodes.Items[0].GetName()}
				} else {
					clientPodConfig.nodeSelector = map[string]string{nodeHostnameKey: nodes.Items[0].GetName()}
					serverPodConfig.nodeSelector = map[string]string{nodeHostnameKey: nodes.Items[1].GetName()}
				}
				netConfig := newNetworkAttachmentConfig(networkAttachmentConfigParams{
					name:      secondaryNetworkName,
					namespace: f.Namespace.Name,
					topology:  "localnet",
				})
				if clientPodConfig.namespace == "" {
					clientPodConfig.namespace = f.Namespace.Name
				}
				if serverPodConfig.namespace == "" {
					serverPodConfig.namespace = f.Namespace.Name
				}

				By("setting up the localnet underlay")
				Expect(providerCtx.SetupUnderlay(f, infraapi.Underlay{
					BridgeName:         deploymentconfig.Get().ExternalBridgeName(),
					LogicalNetworkName: netConfig.networkName,
				})).To(Succeed())

				nad := generateNAD(netConfig)
				By(fmt.Sprintf("creating the attachment configuration: %v\n", nad))
				_, err = nadClient.NetworkAttachmentDefinitions(f.Namespace.Name).Create(
					context.Background(),
					nad,
					metav1.CreateOptions{},
				)
				Expect(err).NotTo(HaveOccurred())

				if serverPodConfig.attachments != nil && serverPodConfig.needsIPRequestFromHostSubnet {
					By("finalizing the server pod IP configuration")
					err = addIPRequestToPodConfig(cs, &serverPodConfig, serverIPOffset)
					Expect(err).NotTo(HaveOccurred())
				}

				if clientPodConfig.attachments != nil && clientPodConfig.needsIPRequestFromHostSubnet {
					By("finalizing the client pod IP configuration")
					err = addIPRequestToPodConfig(cs, &clientPodConfig, clientIPOffset)
					Expect(err).NotTo(HaveOccurred())
				}

				By("instantiating the server pod")
				serverPod := kickstartPod(cs, serverPodConfig)

				By("instantiating the client pod")
				kickstartPod(cs, clientPodConfig)

				// Check that the client pod can reach the server pod on the server localnet interface
				var serverIPs []string
				if serverPodConfig.hostNetwork {
					serverIPs, err = podIPsFromStatus(cs, serverPodConfig.namespace, serverPodConfig.name)
				} else {
					serverIPs, err = podIPsForAttachment(cs, serverPod.Namespace, serverPod.Name, netConfig.name)

				}
				Expect(err).NotTo(HaveOccurred())

				for _, serverIP := range serverIPs {
					By(fmt.Sprintf("asserting the *client* can contact the server pod exposed endpoint: %q on port %q", serverIP, port))
					curlArgs := []string{}
					pingArgs := []string{}
					if clientPodConfig.attachments != nil {
						// When the client is attached to a localnet, send probes from the localnet interface
						curlArgs = []string{"--interface", "net1"}
						pingArgs = []string{"-I", "net1"}
					}
					Eventually(func() error {
						return reachServerPodFromClient(cs, serverPodConfig, clientPodConfig, serverIP, port, curlArgs...)
					}, 2*time.Minute, 6*time.Second).Should(Succeed())

					By(fmt.Sprintf("asserting the *client* can ping the server pod exposed endpoint: %q", serverIP))
					Eventually(func() error {
						return pingServerPodFromClient(cs, serverPodConfig, clientPodConfig, serverIP, pingArgs...)
					}, 2*time.Minute, 6*time.Second).Should(Succeed())
				}
			},
			ginkgo.Entry(
				"can be reached by a client pod in the default network on a different node",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "localnet",
				},
				podConfiguration{ // client on default network
					name:         clientPodName,
					isPrivileged: true,
				},
				podConfiguration{ // server attached to localnet secondary network
					attachments: []nadapi.NetworkSelectionElement{{
						Name: secondaryNetworkName,
					}},
					name:                         podName,
					containerCmd:                 httpServerContainerCmd(port),
					needsIPRequestFromHostSubnet: true, // will override attachments above with an IPRequest
				},
				false, // scheduled on distinct Nodes
				Label("BUG", "OCPBUGS-43004"),
			),
			ginkgo.Entry(
				"can be reached by a client pod in the default network on the same node",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "localnet",
				},
				podConfiguration{ // client on default network
					name:         clientPodName + "-same-node",
					isPrivileged: true,
				},
				podConfiguration{ // server attached to localnet secondary network
					attachments: []nadapi.NetworkSelectionElement{{
						Name: secondaryNetworkName,
					}},
					name:                         podName,
					containerCmd:                 httpServerContainerCmd(port),
					needsIPRequestFromHostSubnet: true,
				},
				true, // collocated on same Node
				Label("BUG", "OCPBUGS-43004"),
			),
			ginkgo.Entry(
				"can reach a host-networked pod on a different node",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "localnet",
				},
				podConfiguration{ // client on localnet
					attachments: []nadapi.NetworkSelectionElement{{
						Name: secondaryNetworkName,
					}},
					name:                         clientPodName,
					isPrivileged:                 true,
					needsIPRequestFromHostSubnet: true,
				},
				podConfiguration{ // server on default network, pod is host-networked
					name:         podName,
					containerCmd: httpServerContainerCmd(port),
					hostNetwork:  true,
				},
				false, // not collocated on same node
				Label("STORY", "SDN-5345"),
			),
			ginkgo.Entry(
				"can reach a host-networked pod on the same node",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "localnet",
				},
				podConfiguration{ // client on localnet
					attachments: []nadapi.NetworkSelectionElement{{
						Name: secondaryNetworkName,
					}},
					name:                         clientPodName,
					isPrivileged:                 true,
					needsIPRequestFromHostSubnet: true,
				},
				podConfiguration{ // server on default network, pod is host-networked
					name:         podName,
					containerCmd: httpServerContainerCmd(port),
					hostNetwork:  true,
				},
				true, // collocated on same node
				Label("STORY", "SDN-5345"),
			),
		)
	})

	Context("multiple pods connected to the same OVN-K secondary network", func() {
		const (
			clientPodName   = "client-pod"
			nodeHostnameKey = "kubernetes.io/hostname"
			port            = 9000
			clientIP        = "192.168.200.10/24"
			staticServerIP  = "192.168.200.20/24"
		)

		ginkgo.It("eventually configures pods that were added to an already existing network before the nad", func() {
			netConfig := newNetworkAttachmentConfig(networkAttachmentConfigParams{
				name:      secondaryNetworkName,
				namespace: f.Namespace.Name,
				topology:  "layer2",
				cidr:      secondaryNetworkCIDR,
			})

			By("creating the attachment configuration")
			_, err := nadClient.NetworkAttachmentDefinitions(f.Namespace.Name).Create(
				context.Background(),
				generateNAD(netConfig),
				metav1.CreateOptions{},
			)
			Expect(err).NotTo(HaveOccurred())

			By("creating a new namespace")

			createdNamespace, err := f.CreateNamespace(context.Background(), "multi-nad-namespace", nil)
			Expect(err).NotTo(HaveOccurred())

			By("creating the pod in the new namespace")
			pod, err := cs.CoreV1().Pods(createdNamespace.Name).Create(
				context.Background(),
				generatePodSpec(podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        clientPodName,
				}),
				metav1.CreateOptions{},
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(pod).NotTo(BeNil())

			By("asserting the pod is trying to start")
			Eventually(func() bool {
				updatedPod, err := cs.CoreV1().Pods(createdNamespace.Name).Get(context.Background(), pod.GetName(), metav1.GetOptions{})
				if err != nil {
					return false
				}
				if updatedPod.Status.Phase == v1.PodPending {
					for _, containerStatus := range updatedPod.Status.ContainerStatuses {
						// ensure that the container is trying to start
						if containerStatus.State.Waiting != nil {
							return true
						}
					}
				}
				return false
			}, 2*time.Minute, 6*time.Second).Should(BeTrue())

			By("creating the attachment configuration in the new namespace")
			netConfig.namespace = createdNamespace.Name
			_, err = nadClient.NetworkAttachmentDefinitions(createdNamespace.Name).Create(
				context.Background(),
				generateNAD(netConfig),
				metav1.CreateOptions{},
			)
			Expect(err).NotTo(HaveOccurred())

			By("asserting the pod reaches the `Ready` state")
			Eventually(func() v1.PodPhase {
				updatedPod, err := cs.CoreV1().Pods(createdNamespace.Name).Get(context.Background(), pod.GetName(), metav1.GetOptions{})
				if err != nil {
					return v1.PodFailed
				}
				return updatedPod.Status.Phase
			}, 2*time.Minute, 6*time.Second).Should(Equal(v1.PodRunning))

		})
		ginkgo.DescribeTable(
			"can communicate over the secondary network",
			func(netConfigParams networkAttachmentConfigParams, clientPodConfig podConfiguration, serverPodConfig podConfiguration) {
				netConfig := newNetworkAttachmentConfig(netConfigParams)

				netConfig.namespace = f.Namespace.Name
				clientPodConfig.namespace = f.Namespace.Name
				serverPodConfig.namespace = f.Namespace.Name

				if netConfig.topology == "localnet" {
					Expect(providerCtx.SetupUnderlay(f, infraapi.Underlay{
						LogicalNetworkName: netConfig.networkName,
						VlanID:             netConfig.vlanID,
					})).To(Succeed())
				}

				By("creating the attachment configuration")
				_, err := nadClient.NetworkAttachmentDefinitions(f.Namespace.Name).Create(
					context.Background(),
					generateNAD(netConfig),
					metav1.CreateOptions{},
				)
				Expect(err).NotTo(HaveOccurred())

				By("Get two scheduable nodes and schedule client and server to be on distinct Nodes")
				nodes, err := e2enode.GetBoundedReadySchedulableNodes(context.Background(), f.ClientSet, 2)
				framework.ExpectNoError(err, "2 scheduable nodes are required")
				Expect(len(nodes.Items)).To(BeNumerically(">=", 1), "cluster should have at least 2 nodes")
				clientPodConfig.nodeSelector = map[string]string{nodeHostnameKey: nodes.Items[0].GetName()}
				serverPodConfig.nodeSelector = map[string]string{nodeHostnameKey: nodes.Items[1].GetName()}

				By("instantiating the server pod")
				serverPod, err := cs.CoreV1().Pods(serverPodConfig.namespace).Create(
					context.Background(),
					generatePodSpec(serverPodConfig),
					metav1.CreateOptions{},
				)
				Expect(err).NotTo(HaveOccurred())
				Expect(serverPod).NotTo(BeNil())

				By("asserting the server pod reaches the `Ready` state")
				Eventually(func() v1.PodPhase {
					updatedPod, err := cs.CoreV1().Pods(f.Namespace.Name).Get(context.Background(), serverPod.GetName(), metav1.GetOptions{})
					if err != nil {
						return v1.PodFailed
					}
					return updatedPod.Status.Phase
				}, 2*time.Minute, 6*time.Second).Should(Equal(v1.PodRunning))

				By("instantiating the *client* pod")
				clientPod, err := cs.CoreV1().Pods(clientPodConfig.namespace).Create(
					context.Background(),
					generatePodSpec(clientPodConfig),
					metav1.CreateOptions{},
				)
				Expect(err).NotTo(HaveOccurred())

				By("asserting the client pod reaches the `Ready` state")
				Eventually(func() v1.PodPhase {
					updatedPod, err := cs.CoreV1().Pods(f.Namespace.Name).Get(context.Background(), clientPod.GetName(), metav1.GetOptions{})
					if err != nil {
						return v1.PodFailed
					}
					return updatedPod.Status.Phase
				}, 2*time.Minute, 6*time.Second).Should(Equal(v1.PodRunning))

				serverIP := ""
				if netConfig.cidr == "" {
					By("configuring static IP addresses in the pods")
					const (
						clientIP       = "192.168.200.10/24"
						staticServerIP = "192.168.200.20/24"
					)
					if !areStaticIPsConfiguredViaCNI(clientPodConfig) {
						Expect(configurePodStaticIP(clientPodConfig.namespace, clientPodName, clientIP)).To(Succeed())
					}
					if !areStaticIPsConfiguredViaCNI(serverPodConfig) {
						Expect(configurePodStaticIP(serverPodConfig.namespace, serverPod.GetName(), staticServerIP)).To(Succeed())
					}
					serverIP = strings.ReplaceAll(staticServerIP, "/24", "")
				}

				for i, cidr := range strings.Split(netConfig.cidr, ",") {
					if cidr != "" {
						By("asserting the server pod has an IP from the configured range")
						serverIP, err = podIPForAttachment(cs, f.Namespace.Name, serverPod.GetName(), netConfig.name, i)
						Expect(err).NotTo(HaveOccurred())
						By(fmt.Sprintf("asserting the server pod IP %v is from the configured range %v/%v", serverIP, cidr, netPrefixLengthPerNode))
						subnet, err := getNetCIDRSubnet(cidr)
						Expect(err).NotTo(HaveOccurred())
						Expect(inRange(subnet, serverIP)).To(Succeed())
					}

					By("asserting the *client* pod can contact the server pod exposed endpoint")
					Eventually(func() error {
						return reachServerPodFromClient(cs, serverPodConfig, clientPodConfig, serverIP, port)
					}, 2*time.Minute, 6*time.Second).Should(Succeed())
				}
			},
			ginkgo.Entry(
				"can communicate over an L2 secondary network when the pods are scheduled in different nodes",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "layer2",
					cidr:     secondaryNetworkCIDR,
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        clientPodName,
				},
				podConfiguration{
					attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:         podName,
					containerCmd: httpServerContainerCmd(port),
				},
			),
			ginkgo.Entry(
				"can communicate over an L2 - switched - secondary network with `excludeCIDR`s",
				networkAttachmentConfigParams{
					name:         secondaryNetworkName,
					topology:     "layer2",
					cidr:         secondaryNetworkCIDR,
					excludeCIDRs: []string{secondaryFlatL2IgnoreCIDR},
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        clientPodName,
				},
				podConfiguration{
					attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:         podName,
					containerCmd: httpServerContainerCmd(port),
				},
			),
			ginkgo.Entry(
				"can communicate over an L3 - routed - secondary network",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "layer3",
					cidr:     netCIDR(secondaryNetworkCIDR, netPrefixLengthPerNode),
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        clientPodName,
				},
				podConfiguration{
					attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:         podName,
					containerCmd: httpServerContainerCmd(port),
				},
			),
			ginkgo.Entry(
				"can communicate over an L3 - routed - secondary network with IPv6 subnet",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "layer3",
					cidr:     netCIDR(secondaryIPv6CIDR, netPrefixLengthIPv6PerNode),
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        clientPodName,
				},
				podConfiguration{
					attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:         podName,
					containerCmd: httpServerContainerCmd(port),
				},
			),
			ginkgo.Entry(
				"can communicate over an L3 - routed - secondary network with a dual stack configuration",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "layer3",
					cidr:     strings.Join([]string{netCIDR(secondaryNetworkCIDR, netPrefixLengthPerNode), netCIDR(secondaryIPv6CIDR, netPrefixLengthIPv6PerNode)}, ","),
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        clientPodName,
				},
				podConfiguration{
					attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:         podName,
					containerCmd: httpServerContainerCmd(port),
				},
			),
			ginkgo.Entry(
				"can communicate over an L2 - switched - secondary network without IPAM",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "layer2",
				},
				podConfiguration{
					attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:         clientPodName,
					isPrivileged: true,
				},
				podConfiguration{
					attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:         podName,
					containerCmd: httpServerContainerCmd(port),
					isPrivileged: true,
				},
			),
			ginkgo.Entry(
				"can communicate over an L2 secondary network without IPAM, with static IPs configured via network selection elements",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "layer2",
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{
						Name:      secondaryNetworkName,
						IPRequest: []string{clientIP},
					}},
					name: clientPodName,
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{
						Name:      secondaryNetworkName,
						IPRequest: []string{staticServerIP},
					}},
					name:         podName,
					containerCmd: httpServerContainerCmd(port),
				},
			),
			ginkgo.Entry(
				"can communicate over an L2 secondary network with an IPv6 subnet when pods are scheduled in different nodes",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "layer2",
					cidr:     secondaryIPv6CIDR,
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        clientPodName,
				},
				podConfiguration{
					attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:         podName,
					containerCmd: httpServerContainerCmd(port),
				},
			),
			ginkgo.Entry(
				"can communicate over an L2 secondary network with a dual stack configuration",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "layer2",
					cidr:     strings.Join([]string{secondaryFlatL2NetworkCIDR, secondaryIPv6CIDR}, ","),
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        clientPodName,
				},
				podConfiguration{
					attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:         podName,
					containerCmd: httpServerContainerCmd(port),
				},
			),
			ginkgo.Entry(
				"can communicate over a localnet secondary network when the pods are scheduled on different nodes",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "localnet",
					cidr:     secondaryLocalnetNetworkCIDR,
					vlanID:   localnetVLANID,
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        clientPodName,
				},
				podConfiguration{
					attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:         podName,
					containerCmd: httpServerContainerCmd(port),
				},
			),
			ginkgo.Entry(
				"can communicate over a localnet secondary network without IPAM when the pods are scheduled on different nodes",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "localnet",
					vlanID:   localnetVLANID,
				},
				podConfiguration{
					attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:         clientPodName,
					isPrivileged: true,
				},
				podConfiguration{
					attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:         podName,
					containerCmd: httpServerContainerCmd(port),
					isPrivileged: true,
				},
			),
			ginkgo.Entry(
				"can communicate over a localnet secondary network without IPAM when the pods are scheduled on different nodes, with static IPs configured via network selection elements",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "localnet",
					vlanID:   localnetVLANID,
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{
						Name:      secondaryNetworkName,
						IPRequest: []string{clientIP},
					}},
					name: clientPodName,
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{
						Name:      secondaryNetworkName,
						IPRequest: []string{staticServerIP},
					}},
					name:         podName,
					containerCmd: httpServerContainerCmd(port),
				},
			),
			ginkgo.Entry(
				"can communicate over a localnet secondary network with an IPv6 subnet when pods are scheduled on different nodes",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "localnet",
					cidr:     secondaryIPv6CIDR,
					vlanID:   localnetVLANID,
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        clientPodName,
				},
				podConfiguration{
					attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:         podName,
					containerCmd: httpServerContainerCmd(port),
				},
			),
			ginkgo.Entry(
				"can communicate over a localnet secondary network with a dual stack configuration when pods are scheduled on different nodes",
				networkAttachmentConfigParams{
					name:     secondaryNetworkName,
					topology: "localnet",
					cidr:     strings.Join([]string{secondaryLocalnetNetworkCIDR, secondaryIPv6CIDR}, ","),
					vlanID:   localnetVLANID,
				},
				podConfiguration{
					attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:        clientPodName,
				},
				podConfiguration{
					attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					name:         podName,
					containerCmd: httpServerContainerCmd(port),
				},
			),
		)

		Context("localnet OVN-K secondary network", func() {
			const (
				clientPodName              = "client-pod"
				nodeHostnameKey            = "kubernetes.io/hostname"
				servicePort         uint16 = 9000
				dockerNetworkName          = "underlay"
				underlayServiceIP          = "60.128.0.1"
				expectedOriginalMTU        = 1200
			)

			var (
				netConfig networkAttachmentConfig
			)

			underlayIP := underlayServiceIP + "/24"
			Context("with a service running on the underlay", func() {
				BeforeEach(func() {
					netConfig = newNetworkAttachmentConfig(
						networkAttachmentConfigParams{
							name:         secondaryNetworkName,
							namespace:    f.Namespace.Name,
							vlanID:       localnetVLANID,
							topology:     "localnet",
							cidr:         secondaryLocalnetNetworkCIDR,
							excludeCIDRs: []string{underlayServiceIP + "/32"},
							mtu:          expectedOriginalMTU,
						})

					By("setting up the localnet underlay")
					Expect(providerCtx.SetupUnderlay(f, infraapi.Underlay{
						LogicalNetworkName: netConfig.networkName,
						VlanID:             netConfig.vlanID,
					})).To(Succeed())
				})

				BeforeEach(func() {
					By("starting a service, connected to the underlay")
					providerCtx = infraprovider.Get().NewTestContext()

					underlayNetwork, err := infraprovider.Get().GetNetwork(dockerNetworkName)
					Expect(err).NotTo(HaveOccurred(), "must get underlay network")
					externalContainerName := f.Namespace.Name + "-web-server"
					serviceContainerSpec := infraapi.ExternalContainer{
						Name:       externalContainerName,
						Image:      images.AgnHost(),
						Network:    underlayNetwork,
						Entrypoint: "bash",
						Args:       []string{"-c", fmt.Sprintf("ip a add %s/24 dev eth0 && ./agnhost netexec --http-port=%d", underlayServiceIP, servicePort)},
						ExtPort:    servicePort,
					}
					_, err = providerCtx.CreateExternalContainer(serviceContainerSpec)
					Expect(err).NotTo(HaveOccurred(), "must create external container 1")
				})

				BeforeEach(func() {
					By("creating the attachment configuration")
					_, err := nadClient.NetworkAttachmentDefinitions(netConfig.namespace).Create(
						context.Background(),
						generateNAD(netConfig),
						metav1.CreateOptions{},
					)
					Expect(err).NotTo(HaveOccurred())
				})

				It("correctly sets the MTU on the pod", func() {
					Eventually(func() error {
						clientPodConfig := podConfiguration{
							name:        clientPodName + randStr(10),
							namespace:   f.Namespace.Name,
							attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						}
						kickstartPod(cs, clientPodConfig)
						mtu, err := getSecondaryInterfaceMTU(clientPodConfig)
						if err != nil {
							return fmt.Errorf("failed to get MTU: %w", err)
						}

						if mtu != expectedOriginalMTU {
							return fmt.Errorf("pod MTU is %d, but expected %d", mtu, expectedOriginalMTU)
						}
						return nil
					}).Should(Succeed(), "pod MTU should be properly configured")
				})

				It("can communicate over a localnet secondary network from pod to the underlay service", func() {
					clientPodConfig := podConfiguration{
						name:        clientPodName,
						namespace:   f.Namespace.Name,
						attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
					}
					kickstartPod(cs, clientPodConfig)

					By("asserting the *client* pod can contact the underlay service")
					Expect(connectToServer(clientPodConfig, underlayServiceIP, servicePort)).To(Succeed())

				})

				Context("and networkAttachmentDefinition is modified", func() {
					const (
						expectedChangedMTU        = 1600
						newDesiredRange           = "60.128.0.192/28" // Desired IPs from 60.128.0.192 to 60.128.0.207
						excludedSubnetLowerRange1 = "60.128.0.0/25"   // Excludes IPs from 60.128.0.0 to 60.128.0.127
						excludedSubnetLowerRange2 = "60.128.0.128/26" // Excludes IPs from 60.128.0.128 to 60.128.0.191
						excludedSubnetUpperRange1 = "60.128.0.208/28" // Excludes IPs from 60.128.0.208 to 60.128.0.223
						excludedSubnetUpperRange2 = "60.128.0.224/27" // Excludes IPs from 60.128.0.224 to 60.128.0.255
						newLocalnetVLANID         = 30
					)
					BeforeEach(func() {
						By("setting new MTU")
						netConfig.mtu = expectedChangedMTU
						By("setting new subnets to leave a smaller range")
						netConfig.excludeCIDRs = []string{excludedSubnetLowerRange1, excludedSubnetLowerRange2, excludedSubnetUpperRange1, excludedSubnetUpperRange2}
						By("setting new VLAN-ID")
						netConfig.vlanID = newLocalnetVLANID
						p := []byte(fmt.Sprintf(`[{"op":"replace","path":"/spec/config","value":%q}]`, generateNADSpec(netConfig)))
						Expect(patchNADSpec(nadClient, netConfig.name, netConfig.namespace, p)).To(Succeed())
					})

					It("sets the new MTU on the pod after NetworkAttachmentDefinition reconcile", func() {
						Eventually(func() error {
							clientPodConfig := podConfiguration{
								name:        clientPodName + randStr(10),
								namespace:   f.Namespace.Name,
								attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
							}
							kickstartPod(cs, clientPodConfig)
							mtu, err := getSecondaryInterfaceMTU(clientPodConfig)
							if err != nil {
								return fmt.Errorf("failed to get MTU: %w", err)
							}
							if mtu != expectedChangedMTU {
								err := fmt.Errorf("pod MTU is %d, but expected %d", mtu, expectedChangedMTU)
								if delErr := cs.CoreV1().Pods(clientPodConfig.namespace).Delete(context.Background(), clientPodConfig.name, metav1.DeleteOptions{}); delErr != nil {
									err = errors.Join(err, fmt.Errorf("pod delete failed: %w", delErr))
								}
								return err
							}
							return nil
						}).Should(Succeed(), "pod MTU should be properly configured")
					})

					It("allocates the pod's secondary interface IP in the new range after NetworkAttachmentDefinition reconcile", func() {
						By("asserting the pod's secondary interface IP is properly configured")
						Eventually(func() error {
							clientPodConfig := podConfiguration{
								name:        clientPodName + "-" + randStr(10),
								namespace:   f.Namespace.Name,
								attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
							}
							kickstartPod(cs, clientPodConfig)

							clientIP, err := podIPForAttachment(cs, clientPodConfig.namespace, clientPodConfig.name, netConfig.name, 0)
							if err != nil {
								return err
							}

							// In order to prevent the pod from interfering with the test, deleting it before retrying
							if err := inRange(newDesiredRange, clientIP); err != nil {
								if delErr := cs.CoreV1().Pods(clientPodConfig.namespace).Delete(context.Background(), clientPodConfig.name, metav1.DeleteOptions{}); delErr != nil {
									err = errors.Join(err, fmt.Errorf("pod delete failed: %w", delErr))
								}
								return err
							}
							return nil
						}).Should(Succeed(), "pod's secondary NIC is not allocated in the desired range")
					})

					It("can no longer communicate over a localnet secondary network from pod to the underlay service", func() {
						Eventually(func() error {
							clientPodConfig := podConfiguration{
								name:        clientPodName,
								namespace:   f.Namespace.Name,
								attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
							}
							kickstartPod(cs, clientPodConfig)

							By("asserting the *client* pod can no longer contact the underlay service")
							var err error
							if err = connectToServer(clientPodConfig, underlayServiceIP, servicePort); err != nil && strings.Contains(err.Error(), "exit code 28") {
								return nil
							}
							err = fmt.Errorf("expected exit code 28 from underlay service, got err %w", err)

							if delErr := cs.CoreV1().Pods(clientPodConfig.namespace).Delete(context.Background(), clientPodConfig.name, metav1.DeleteOptions{}); delErr != nil {
								err = errors.Join(err, fmt.Errorf("pod delete failed: %w", delErr))
							}
							return err
						}).Should(Succeed(), "pod should be disconnected from underlay")
					})

					Context("and the service connected to the underlay is reconfigured to connect to the new VLAN-ID", func() {
						BeforeEach(func() {
							Expect(providerCtx.SetupUnderlay(f, infraapi.Underlay{
								LogicalNetworkName: netConfig.networkName,
								VlanID:             newLocalnetVLANID,
							})).To(Succeed(), "configuring the OVS bridge with new localnet vlan id")
						})

						It("can now communicate over a localnet secondary network from pod to the underlay service", func() {
							Eventually(func() error {
								clientPodConfig := podConfiguration{
									name:        clientPodName,
									namespace:   f.Namespace.Name,
									attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
								}
								kickstartPod(cs, clientPodConfig)

								By("asserting the *client* pod can contact the underlay service")
								if err := connectToServer(clientPodConfig, underlayServiceIP, servicePort); err != nil {
									if delErr := cs.CoreV1().Pods(clientPodConfig.namespace).Delete(context.Background(), clientPodConfig.name, metav1.DeleteOptions{}); delErr != nil {
										err = errors.Join(err, fmt.Errorf("pod delete failed: %w", delErr))
									}
									return err
								}
								return nil
							}).Should(Succeed(), "pod should be connected to underlay")
						})
					})
				})

				Context("with multi network policy blocking the traffic", func() {
					var clientPodConfig podConfiguration
					labels := map[string]string{"name": "access-control"}

					const policyName = "allow-egress-ipblock"

					BeforeEach(func() {
						clientPodConfig = podConfiguration{
							name:        clientPodName,
							namespace:   f.Namespace.Name,
							attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
							labels:      labels,
						}
						kickstartPod(cs, clientPodConfig)

						By("asserting the *client* pod can contact the underlay service before creating the policy")
						Expect(connectToServer(clientPodConfig, underlayServiceIP, servicePort)).To(Succeed())
					})

					AfterEach(func() {
						By("deleting the multi-network policy")
						err := mnpClient.MultiNetworkPolicies(clientPodConfig.namespace).Delete(
							context.Background(),
							policyName,
							metav1.DeleteOptions{},
						)
						Expect(err).NotTo(HaveOccurred())

						By("asserting the *client* pod can contact the underlay service after deleting the policy")
						Expect(connectToServer(clientPodConfig, underlayServiceIP, servicePort)).To(Succeed())
					})

					It("can not communicate over a localnet secondary network from pod to the underlay service", func() {
						By("creating the multi-network policy")
						_, err := mnpClient.MultiNetworkPolicies(clientPodConfig.namespace).Create(
							context.Background(),
							multiNetEgressLimitingIPBlockPolicy(
								policyName,
								secondaryNetworkName,
								metav1.LabelSelector{
									MatchLabels: labels,
								},
								mnpapi.IPBlock{
									CIDR:   secondaryLocalnetNetworkCIDR,
									Except: []string{underlayServiceIP},
								},
							),
							metav1.CreateOptions{},
						)
						Expect(err).NotTo(HaveOccurred())

						By("asserting the *client* pod cannot contact the underlay service after creating the policy")
						Expect(connectToServer(clientPodConfig, underlayServiceIP, servicePort)).To(MatchError(ContainSubstring("exit code 28")))
					})
				})

				When("a policy is provisioned", func() {
					var clientPodConfig podConfiguration
					allPodsSelector := map[string]string{}

					const (
						denyAllIngress = "deny-all-ingress"
						allowAllEgress = "allow-all-egress"
						denyAllEgress  = "deny-all-egress"
					)

					BeforeEach(func() {
						clientPodConfig = podConfiguration{
							name:        clientPodName,
							namespace:   f.Namespace.Name,
							attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
							labels:      allPodsSelector,
						}
						kickstartPod(cs, clientPodConfig)
					})

					DescribeTable("can communicate over a localnet secondary network from pod to gw", func(mnp *mnpapi.MultiNetworkPolicy) {
						By("provisioning the multi-network policy")
						_, err := mnpClient.MultiNetworkPolicies(clientPodConfig.namespace).Create(
							context.Background(),
							mnp,
							metav1.CreateOptions{},
						)
						Expect(err).NotTo(HaveOccurred())

						if mnp.Name != denyAllEgress {
							Expect(connectToServer(clientPodConfig, underlayServiceIP, servicePort)).To(Succeed())
						} else {
							Expect(connectToServer(clientPodConfig, underlayServiceIP, servicePort)).To(Not(Succeed()))
						}

						By("deleting the multi-network policy")
						Expect(mnpClient.MultiNetworkPolicies(clientPodConfig.namespace).Delete(
							context.Background(),
							mnp.Name,
							metav1.DeleteOptions{},
						)).To(Succeed())

						By("asserting the *client* pod can contact the underlay service after deleting the policy")
						Expect(connectToServer(clientPodConfig, underlayServiceIP, servicePort)).To(Succeed())
					},
						XEntry(
							"ingress denyall, ingress policy should have no impact on egress",
							multiNetPolicy(
								denyAllIngress,
								secondaryNetworkName,
								metav1.LabelSelector{
									MatchLabels: allPodsSelector,
								},
								[]mnpapi.MultiPolicyType{mnpapi.PolicyTypeIngress},
								nil,
								nil,
							),
							Label("BUG", "OCPBUGS-25928"),
						),
						Entry(
							"ingress denyall, egress allow all, ingress policy should have no impact on egress",
							multiNetPolicy(
								denyAllIngress,
								secondaryNetworkName,
								metav1.LabelSelector{
									MatchLabels: allPodsSelector,
								},
								[]mnpapi.MultiPolicyType{mnpapi.PolicyTypeIngress, mnpapi.PolicyTypeEgress},
								nil,
								[]mnpapi.MultiNetworkPolicyEgressRule{
									{},
								},
							),
						),
						Entry(
							"egress allow all",
							multiNetPolicy(
								allowAllEgress,
								secondaryNetworkName,
								metav1.LabelSelector{
									MatchLabels: allPodsSelector,
								},
								[]mnpapi.MultiPolicyType{mnpapi.PolicyTypeEgress},
								nil,
								[]mnpapi.MultiNetworkPolicyEgressRule{
									{},
								},
							),
						),
						Entry(
							"egress deny all",
							multiNetPolicy(
								denyAllEgress,
								secondaryNetworkName,
								metav1.LabelSelector{
									MatchLabels: allPodsSelector,
								},
								[]mnpapi.MultiPolicyType{mnpapi.PolicyTypeEgress},
								nil,
								nil,
							),
						),
					)
				})
			})

			Context("with a trunked configuration", func() {
				const vlanID = 20
				BeforeEach(func() {
					// we are setting up the bridge in trunked mode by not
					// specifying a particular VLAN ID on the network conf
					netConfig = newNetworkAttachmentConfig(
						networkAttachmentConfigParams{
							name:         secondaryNetworkName,
							namespace:    f.Namespace.Name,
							topology:     "localnet",
							cidr:         secondaryLocalnetNetworkCIDR,
							excludeCIDRs: []string{underlayServiceIP + "/32"},
						})

					By("setting up the localnet underlay with a trunked configuration")
					Expect(providerCtx.SetupUnderlay(f, infraapi.Underlay{
						LogicalNetworkName: netConfig.networkName,
						VlanID:             netConfig.vlanID,
					})).To(Succeed(), "configuring the OVS bridge")

					By("starting a service, connected to the underlay over a VLAN")
					providerCtx = infraprovider.Get().NewTestContext()

					ifName := "eth0"
					vlanName := fmt.Sprintf("%s.%d", ifName, vlanID)
					underlayNetwork, err := infraprovider.Get().GetNetwork(dockerNetworkName)
					Expect(err).NotTo(HaveOccurred(), "must get underlay network")
					externalContainerName := f.Namespace.Name + "-web-server"
					serviceContainerSpec := infraapi.ExternalContainer{
						Name:       externalContainerName,
						Image:      images.AgnHost(),
						Network:    underlayNetwork,
						Entrypoint: "bash",
						ExtPort:    servicePort,
						Args: []string{"-c", fmt.Sprintf(`
ip link add link %[1]s name %[2]s type vlan id %[3]d
ip link set dev %[2]s up
ip a add %[4]s/24 dev %[2]s
./agnhost netexec --http-port=%[5]d
`, ifName, vlanName, vlanID, underlayServiceIP, servicePort)},
					}
					_, err = providerCtx.CreateExternalContainer(serviceContainerSpec)
					Expect(err).NotTo(HaveOccurred(), "must create external container 1")

				})

				It("the same bridge mapping can be shared by a separate VLAN by using the physical network name attribute", func() {
					const otherNetworkName = "different-network"
					vlan20NetConfig := newNetworkAttachmentConfig(
						networkAttachmentConfigParams{
							name:                otherNetworkName,
							physicalNetworkName: netConfig.networkName,
							namespace:           f.Namespace.Name,
							vlanID:              vlanID,
							topology:            "localnet",
							cidr:                secondaryLocalnetNetworkCIDR,
							excludeCIDRs:        []string{underlayServiceIP + "/32"},
						})

					By("creating the attachment configuration for a separate VLAN")
					_, err := nadClient.NetworkAttachmentDefinitions(f.Namespace.Name).Create(
						context.Background(),
						generateNAD(vlan20NetConfig),
						metav1.CreateOptions{},
					)
					Expect(err).NotTo(HaveOccurred())

					clientPodConfig := podConfiguration{
						name:        clientPodName,
						namespace:   f.Namespace.Name,
						attachments: []nadapi.NetworkSelectionElement{{Name: otherNetworkName}},
					}
					kickstartPod(cs, clientPodConfig)

					By(fmt.Sprintf("asserting the *client* pod can contact the underlay service with IP %q on the separate vlan", underlayIP))
					Expect(connectToServer(clientPodConfig, underlayServiceIP, servicePort)).To(Succeed())

				})
			})
		})

		Context("with multi-network policies that", func() {
			const (
				generatedNamespaceNamePrefix = "pepe"
				blockedServerStaticIP        = "192.168.200.30"
			)
			var extraNamespace *v1.Namespace

			BeforeEach(func() {
				createdNamespace, err := cs.CoreV1().Namespaces().Create(
					context.Background(),
					&v1.Namespace{
						ObjectMeta: metav1.ObjectMeta{
							Labels:       map[string]string{"role": "trusted"},
							GenerateName: generatedNamespaceNamePrefix,
						},
					},
					metav1.CreateOptions{},
				)
				Expect(err).NotTo(HaveOccurred())
				extraNamespace = createdNamespace
			})

			AfterEach(func() {
				blockUntilEverythingIsGone := metav1.DeletePropagationForeground
				Expect(cs.CoreV1().Namespaces().Delete(
					context.Background(),
					extraNamespace.Name,
					metav1.DeleteOptions{PropagationPolicy: &blockUntilEverythingIsGone},
				)).To(Succeed())
				Eventually(func() bool {
					_, err := cs.CoreV1().Namespaces().Get(context.Background(), extraNamespace.Name, metav1.GetOptions{})
					nsPods, podCatchErr := cs.CoreV1().Pods(extraNamespace.Name).List(context.Background(), metav1.ListOptions{})
					return podCatchErr == nil && apierrors.IsNotFound(err) && len(nsPods.Items) == 0
				}, 2*time.Minute, 5*time.Second).Should(BeTrue())
			})

			ginkgo.DescribeTable(
				"configure traffic allow lists",
				func(netConfigParams networkAttachmentConfigParams, allowedClientPodConfig podConfiguration, blockedClientPodConfig podConfiguration, serverPodConfig podConfiguration, policy *mnpapi.MultiNetworkPolicy) {
					netConfig := newNetworkAttachmentConfig(netConfigParams)

					if netConfig.topology == "localnet" {
						By("setting up the localnet underlay")
						Expect(providerCtx.SetupUnderlay(f, infraapi.Underlay{
							LogicalNetworkName: netConfig.networkName,
							VlanID:             netConfig.vlanID,
						})).To(Succeed())
					}

					Expect(createNads(f, nadClient, extraNamespace, netConfig)).NotTo(HaveOccurred())

					kickstartPodInNamespace(cs, &allowedClientPodConfig, f.Namespace.Name, extraNamespace.Name)
					kickstartPodInNamespace(cs, &blockedClientPodConfig, f.Namespace.Name, extraNamespace.Name)
					kickstartPodInNamespace(cs, &serverPodConfig, f.Namespace.Name, extraNamespace.Name)

					serverIP, err := podIPForAttachment(cs, serverPodConfig.namespace, serverPodConfig.name, netConfig.name, 0)
					Expect(err).NotTo(HaveOccurred())
					if netConfig.cidr != "" {
						assertServerPodIPInRange(netConfig.cidr, serverIP, netPrefixLengthPerNode)
					}

					if doesPolicyFeatAnIPBlock(policy) {
						blockedIP, err := podIPForAttachment(cs, f.Namespace.Name, blockedClientPodConfig.name, netConfig.name, 0)
						Expect(err).NotTo(HaveOccurred())
						setBlockedClientIPInPolicyIPBlockExcludedRanges(policy, blockedIP)
					}

					Expect(createMultiNetworkPolicy(mnpClient, f.Namespace.Name, policy)).To(Succeed())

					By("asserting the *allowed-client* pod can contact the server pod exposed endpoint")
					Eventually(func() error {
						return reachServerPodFromClient(cs, serverPodConfig, allowedClientPodConfig, serverIP, port)
					}, 2*time.Minute, 6*time.Second).Should(Succeed())

					By("asserting the *blocked-client* pod **cannot** contact the server pod exposed endpoint")
					Expect(connectToServer(blockedClientPodConfig, serverIP, port)).To(MatchError(ContainSubstring("exit code 28")))
				},
				ginkgo.Entry(
					"using pod selectors for a pure L2 overlay",
					networkAttachmentConfigParams{
						name:     secondaryNetworkName,
						topology: "layer2",
						cidr:     secondaryFlatL2NetworkCIDR,
					},
					podConfiguration{
						attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:        allowedClient(clientPodName),
						labels: map[string]string{
							"app":  "client",
							"role": "trusted",
						},
					},
					podConfiguration{
						attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:        blockedClient(clientPodName),
						labels:      map[string]string{"app": "client"},
					},
					podConfiguration{
						attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:         podName,
						containerCmd: httpServerContainerCmd(port),
						labels:       map[string]string{"app": "stuff-doer"},
					},
					multiNetIngressLimitingPolicy(
						secondaryNetworkName,
						metav1.LabelSelector{
							MatchLabels: map[string]string{"app": "stuff-doer"},
						},
						metav1.LabelSelector{
							MatchLabels: map[string]string{"role": "trusted"},
						},
						multiNetPolicyPort(port),
					),
				),
				ginkgo.Entry(
					"using pod selectors and port range for a pure L2 overlay",
					networkAttachmentConfigParams{
						name:     secondaryNetworkName,
						topology: "layer2",
						cidr:     secondaryFlatL2NetworkCIDR,
					},
					podConfiguration{
						attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:        allowedClient(clientPodName),
						labels: map[string]string{
							"app":  "client",
							"role": "trusted",
						},
					},
					podConfiguration{
						attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:        blockedClient(clientPodName),
						labels:      map[string]string{"app": "client"},
					},
					podConfiguration{
						attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:         podName,
						containerCmd: httpServerContainerCmd(port),
						labels:       map[string]string{"app": "stuff-doer"},
					},
					multiNetIngressLimitingPolicy(
						secondaryNetworkName,
						metav1.LabelSelector{
							MatchLabels: map[string]string{"app": "stuff-doer"},
						},
						metav1.LabelSelector{
							MatchLabels: map[string]string{"role": "trusted"},
						},
						// build a random range around the port we are actually trying to allow without explicitly setting it
						multiNetPolicyPortRange(port-3, port+5),
					),
				),
				ginkgo.Entry(
					"using pod selectors for a routed topology",
					networkAttachmentConfigParams{
						name:     secondaryNetworkName,
						topology: "layer3",
						cidr:     netCIDR(secondaryNetworkCIDR, netPrefixLengthPerNode),
					},
					podConfiguration{
						attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:        allowedClient(clientPodName),
						labels: map[string]string{
							"app":  "client",
							"role": "trusted",
						},
					},
					podConfiguration{
						attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:        blockedClient(clientPodName),
						labels:      map[string]string{"app": "client"},
					},
					podConfiguration{
						attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:         podName,
						containerCmd: httpServerContainerCmd(port),
						labels:       map[string]string{"app": "stuff-doer"},
					},
					multiNetIngressLimitingPolicy(
						secondaryNetworkName,
						metav1.LabelSelector{
							MatchLabels: map[string]string{"app": "stuff-doer"},
						},
						metav1.LabelSelector{
							MatchLabels: map[string]string{"role": "trusted"},
						},
						multiNetPolicyPort(port),
					),
				),
				ginkgo.Entry(
					"using pod selectors for a localnet topology",
					networkAttachmentConfigParams{
						name:     secondaryNetworkName,
						topology: "localnet",
						cidr:     secondaryLocalnetNetworkCIDR,
					},
					podConfiguration{
						attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:        allowedClient(clientPodName),
						labels: map[string]string{
							"app":  "client",
							"role": "trusted",
						},
					},
					podConfiguration{
						attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:        blockedClient(clientPodName),
						labels:      map[string]string{"app": "client"},
					},
					podConfiguration{
						attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:         podName,
						containerCmd: httpServerContainerCmd(port),
						labels:       map[string]string{"app": "stuff-doer"},
					},
					multiNetIngressLimitingPolicy(
						secondaryNetworkName,
						metav1.LabelSelector{
							MatchLabels: map[string]string{"app": "stuff-doer"},
						},
						metav1.LabelSelector{
							MatchLabels: map[string]string{"role": "trusted"},
						},
						multiNetPolicyPort(port),
					),
				),
				ginkgo.Entry(
					"using IPBlock for a pure L2 overlay",
					networkAttachmentConfigParams{
						name:     secondaryNetworkName,
						topology: "layer2",
						cidr:     secondaryFlatL2NetworkCIDR,
					},
					podConfiguration{
						attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:        allowedClient(clientPodName),
					},
					podConfiguration{
						attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:        blockedClient(clientPodName),
					},
					podConfiguration{
						attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:         podName,
						containerCmd: httpServerContainerCmd(port),
						labels:       map[string]string{"app": "stuff-doer"},
					},
					multiNetIngressLimitingIPBlockPolicy(
						secondaryNetworkName,
						metav1.LabelSelector{
							MatchLabels: map[string]string{"app": "stuff-doer"},
						},
						mnpapi.IPBlock{ // the test will find out the IP address of the client and put it in the `exclude` list
							CIDR: secondaryFlatL2NetworkCIDR,
						},
						port,
					),
				),
				ginkgo.Entry(
					"using IPBlock for a routed topology",
					networkAttachmentConfigParams{
						name:     secondaryNetworkName,
						topology: "layer3",
						cidr:     netCIDR(secondaryNetworkCIDR, netPrefixLengthPerNode),
					},
					podConfiguration{
						attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:        allowedClient(clientPodName),
					},
					podConfiguration{
						attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:        blockedClient(clientPodName),
					},
					podConfiguration{
						attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:         podName,
						containerCmd: httpServerContainerCmd(port),
						labels:       map[string]string{"app": "stuff-doer"},
					},
					multiNetIngressLimitingIPBlockPolicy(
						secondaryNetworkName,
						metav1.LabelSelector{
							MatchLabels: map[string]string{"app": "stuff-doer"},
						},
						mnpapi.IPBlock{ // the test will find out the IP address of the client and put it in the `exclude` list
							CIDR: secondaryNetworkCIDR,
						},
						port,
					),
				),
				ginkgo.Entry(
					"using IPBlock for a localnet topology",
					networkAttachmentConfigParams{
						name:     secondaryNetworkName,
						topology: "localnet",
						cidr:     secondaryLocalnetNetworkCIDR,
					},
					podConfiguration{
						attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:        allowedClient(clientPodName),
					},
					podConfiguration{
						attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:        blockedClient(clientPodName),
					},
					podConfiguration{
						attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:         podName,
						containerCmd: httpServerContainerCmd(port),
						labels:       map[string]string{"app": "stuff-doer"},
					},
					multiNetIngressLimitingIPBlockPolicy(
						secondaryNetworkName,
						metav1.LabelSelector{
							MatchLabels: map[string]string{"app": "stuff-doer"},
						},
						mnpapi.IPBlock{ // the test will find out the IP address of the client and put it in the `exclude` list
							CIDR: secondaryLocalnetNetworkCIDR,
						},
						port,
					),
				),
				ginkgo.Entry(
					"using namespace selectors for a pure L2 overlay",
					networkAttachmentConfigParams{
						name:        secondaryNetworkName,
						topology:    "layer2",
						cidr:        secondaryFlatL2NetworkCIDR,
						networkName: uniqueNadName("spans-multiple-namespaces"),
					},
					podConfiguration{
						attachments:            []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:                   allowedClient(clientPodName),
						requiresExtraNamespace: true,
					},
					podConfiguration{
						attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:        blockedClient(clientPodName),
					},
					podConfiguration{
						attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:         podName,
						containerCmd: httpServerContainerCmd(port),
						labels:       map[string]string{"app": "stuff-doer"},
					},
					multiNetIngressLimitingPolicyAllowFromNamespace(
						secondaryNetworkName,
						metav1.LabelSelector{
							MatchLabels: map[string]string{"app": "stuff-doer"},
						},
						metav1.LabelSelector{
							MatchLabels: map[string]string{"role": "trusted"},
						},
						port,
					),
				),
				ginkgo.Entry(
					"using namespace selectors for a routed topology",
					networkAttachmentConfigParams{
						name:        secondaryNetworkName,
						topology:    "layer3",
						cidr:        netCIDR(secondaryNetworkCIDR, netPrefixLengthPerNode),
						networkName: uniqueNadName("spans-multiple-namespaces"),
					},
					podConfiguration{
						attachments:            []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:                   allowedClient(clientPodName),
						requiresExtraNamespace: true,
					},
					podConfiguration{
						attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:        blockedClient(clientPodName),
					},
					podConfiguration{
						attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:         podName,
						containerCmd: httpServerContainerCmd(port),
						labels:       map[string]string{"app": "stuff-doer"},
					},
					multiNetIngressLimitingPolicyAllowFromNamespace(
						secondaryNetworkName,
						metav1.LabelSelector{
							MatchLabels: map[string]string{"app": "stuff-doer"},
						},
						metav1.LabelSelector{
							MatchLabels: map[string]string{"role": "trusted"},
						},
						port,
					),
				),
				ginkgo.Entry(
					"using namespace selectors for a localnet topology",
					networkAttachmentConfigParams{
						name:        secondaryNetworkName,
						topology:    "localnet",
						cidr:        secondaryLocalnetNetworkCIDR,
						networkName: uniqueNadName("spans-multiple-namespaces"),
					},
					podConfiguration{
						attachments:            []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:                   allowedClient(clientPodName),
						requiresExtraNamespace: true,
					},
					podConfiguration{
						attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:        blockedClient(clientPodName),
					},
					podConfiguration{
						attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:         podName,
						containerCmd: httpServerContainerCmd(port),
						labels:       map[string]string{"app": "stuff-doer"},
					},
					multiNetIngressLimitingPolicyAllowFromNamespace(
						secondaryNetworkName,
						metav1.LabelSelector{
							MatchLabels: map[string]string{"app": "stuff-doer"},
						},
						metav1.LabelSelector{
							MatchLabels: map[string]string{"role": "trusted"},
						},
						port,
					),
				),

				ginkgo.Entry(
					"using IPBlock for an IPAMless pure L2 overlay",
					networkAttachmentConfigParams{
						name:     secondaryNetworkName,
						topology: "layer2",
					},
					podConfiguration{
						attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName, IPRequest: []string{clientIP}}},
						name:        allowedClient(clientPodName),
					},
					podConfiguration{
						attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName, IPRequest: []string{blockedServerStaticIP + "/24"}}},
						name:        blockedClient(clientPodName),
					},
					podConfiguration{
						attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName, IPRequest: []string{staticServerIP}}},
						name:         podName,
						containerCmd: httpServerContainerCmd(port),
						labels:       map[string]string{"app": "stuff-doer"},
					},
					multiNetIngressLimitingIPBlockPolicy(
						secondaryNetworkName,
						metav1.LabelSelector{
							MatchLabels: map[string]string{"app": "stuff-doer"},
						},
						mnpapi.IPBlock{
							CIDR:   "192.168.200.0/24",
							Except: []string{blockedServerStaticIP},
						},
						port,
					),
				),
			)

			ginkgo.DescribeTable(
				"allow all ingress",
				func(netConfigParams networkAttachmentConfigParams, clientPodConfig podConfiguration, serverPodConfig podConfiguration, policy *mnpapi.MultiNetworkPolicy) {
					netConfig := newNetworkAttachmentConfig(netConfigParams)

					By("setting up the localnet underlay")
					Expect(providerCtx.SetupUnderlay(f, infraapi.Underlay{
						LogicalNetworkName: netConfig.networkName,
						VlanID:             netConfig.vlanID,
					})).To(Succeed())

					Expect(createNads(f, nadClient, extraNamespace, netConfig)).NotTo(HaveOccurred())

					kickstartPodInNamespace(cs, &clientPodConfig, f.Namespace.Name, extraNamespace.Name)
					kickstartPodInNamespace(cs, &serverPodConfig, f.Namespace.Name, extraNamespace.Name)

					serverIP, err := podIPForAttachment(cs, serverPodConfig.namespace, serverPodConfig.name, netConfig.name, 0)
					Expect(err).NotTo(HaveOccurred())
					assertServerPodIPInRange(netConfig.cidr, serverIP, netPrefixLengthPerNode)

					Expect(createMultiNetworkPolicy(mnpClient, f.Namespace.Name, policy)).To(Succeed())

					By("asserting the *client* pod can contact the server pod exposed endpoint")
					Eventually(func() error {
						return reachServerPodFromClient(cs, serverPodConfig, clientPodConfig, serverIP, port)
					}, 2*time.Minute, 6*time.Second).Should(Succeed())
				},
				ginkgo.Entry(
					"using ingress allow-all for a localnet topology",
					networkAttachmentConfigParams{
						name:     secondaryNetworkName,
						topology: "localnet",
						cidr:     secondaryLocalnetNetworkCIDR,
					},
					podConfiguration{
						attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:        allowedClient(clientPodName),
						labels: map[string]string{
							"app":  "client",
							"role": "trusted",
						},
					},
					podConfiguration{
						attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:         podName,
						containerCmd: httpServerContainerCmd(port),
						labels:       map[string]string{"app": "stuff-doer"},
					},
					multiNetPolicy(
						"allow-all-ingress",
						secondaryNetworkName,
						metav1.LabelSelector{},
						[]mnpapi.MultiPolicyType{mnpapi.PolicyTypeIngress},
						[]mnpapi.MultiNetworkPolicyIngressRule{
							mnpapi.MultiNetworkPolicyIngressRule{},
						},
						nil,
					),
				),
				ginkgo.XEntry(
					"using egress deny-all for a localnet topology",
					networkAttachmentConfigParams{
						name:     secondaryNetworkName,
						topology: "localnet",
						cidr:     secondaryLocalnetNetworkCIDR,
					},
					podConfiguration{
						attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:        allowedClient(clientPodName),
						labels: map[string]string{
							"app":  "client",
							"role": "trusted",
						},
					},
					podConfiguration{
						attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:         podName,
						containerCmd: httpServerContainerCmd(port),
						labels:       map[string]string{"app": "stuff-doer"},
					},
					multiNetPolicy(
						"deny-all-egress",
						secondaryNetworkName,
						metav1.LabelSelector{
							MatchLabels: map[string]string{"app": "stuff-doer"},
						},
						[]mnpapi.MultiPolicyType{mnpapi.PolicyTypeEgress},
						nil,
						nil,
					),
					Label("BUG", "OCPBUGS-25928"),
				),
				ginkgo.Entry(
					"using egress deny-all, ingress allow-all for a localnet topology",
					networkAttachmentConfigParams{
						name:     secondaryNetworkName,
						topology: "localnet",
						cidr:     secondaryLocalnetNetworkCIDR,
					},
					podConfiguration{
						attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:        allowedClient(clientPodName),
						labels: map[string]string{
							"app":  "client",
							"role": "trusted",
						},
					},
					podConfiguration{
						attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:         podName,
						containerCmd: httpServerContainerCmd(port),
						labels:       map[string]string{"app": "stuff-doer"},
					},
					multiNetPolicy(
						"deny-all-egress-allow-all-ingress",
						secondaryNetworkName,
						metav1.LabelSelector{
							MatchLabels: map[string]string{"app": "stuff-doer"},
						},
						[]mnpapi.MultiPolicyType{mnpapi.PolicyTypeIngress, mnpapi.PolicyTypeEgress},
						[]mnpapi.MultiNetworkPolicyIngressRule{
							mnpapi.MultiNetworkPolicyIngressRule{},
						},
						nil,
					),
				),
			)

			ginkgo.DescribeTable(
				"deny traffic",
				func(netConfigParams networkAttachmentConfigParams, clientPodConfig podConfiguration, serverPodConfig podConfiguration, policy *mnpapi.MultiNetworkPolicy) {
					netConfig := newNetworkAttachmentConfig(netConfigParams)

					By("setting up the localnet underlay")
					Expect(providerCtx.SetupUnderlay(f, infraapi.Underlay{
						LogicalNetworkName: netConfig.networkName,
						VlanID:             netConfig.vlanID,
					})).To(Succeed())

					Expect(createNads(f, nadClient, extraNamespace, netConfig)).NotTo(HaveOccurred())

					kickstartPodInNamespace(cs, &clientPodConfig, f.Namespace.Name, extraNamespace.Name)
					kickstartPodInNamespace(cs, &serverPodConfig, f.Namespace.Name, extraNamespace.Name)

					serverIP, err := podIPForAttachment(cs, serverPodConfig.namespace, serverPodConfig.name, netConfig.name, 0)
					Expect(err).NotTo(HaveOccurred())
					assertServerPodIPInRange(netConfig.cidr, serverIP, netPrefixLengthPerNode)

					Expect(createMultiNetworkPolicy(mnpClient, f.Namespace.Name, policy)).To(Succeed())

					By("asserting the *client* pod can't contact the server pod exposed endpoint when using ingress deny-all")
					Eventually(func() error {
						return reachServerPodFromClient(cs, serverPodConfig, clientPodConfig, serverIP, port)
					}, 2*time.Minute, 6*time.Second).Should(Not(Succeed()))
				},
				ginkgo.Entry(
					"using ingress deny-all for a localnet topology",
					networkAttachmentConfigParams{
						name:     secondaryNetworkName,
						topology: "localnet",
						cidr:     secondaryLocalnetNetworkCIDR,
					},
					podConfiguration{
						attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:        allowedClient(clientPodName),
						labels: map[string]string{
							"app":  "client",
							"role": "trusted",
						},
					},
					podConfiguration{
						attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:         podName,
						containerCmd: httpServerContainerCmd(port),
						labels:       map[string]string{"app": "stuff-doer"},
					},
					multiNetPolicy(
						"deny-all-ingress",
						secondaryNetworkName,
						metav1.LabelSelector{
							MatchLabels: map[string]string{"app": "stuff-doer"},
						},
						[]mnpapi.MultiPolicyType{mnpapi.PolicyTypeIngress},
						nil,
						nil,
					),
				),
				ginkgo.Entry(
					"using pod selectors and wrong port range for a localnet topology",
					networkAttachmentConfigParams{
						name:     secondaryNetworkName,
						topology: "localnet",
						cidr:     secondaryLocalnetNetworkCIDR,
					},
					podConfiguration{
						attachments: []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:        allowedClient(clientPodName),
						labels: map[string]string{
							"app":  "client",
							"role": "trusted",
						},
					},
					podConfiguration{
						attachments:  []nadapi.NetworkSelectionElement{{Name: secondaryNetworkName}},
						name:         podName,
						containerCmd: httpServerContainerCmd(port),
						labels:       map[string]string{"app": "stuff-doer"},
					},
					multiNetIngressLimitingPolicy(
						secondaryNetworkName,
						metav1.LabelSelector{
							MatchLabels: map[string]string{"app": "stuff-doer"},
						},
						metav1.LabelSelector{
							MatchLabels: map[string]string{"role": "trusted"},
						},
						// build a port range that doesn't include server port
						multiNetPolicyPortRange(port-10, port-1),
					),
				),
			)
		})
	})

	Context("A pod with multiple attachments to the same OVN-K networks", func() {
		var pod *v1.Pod

		BeforeEach(func() {
			netAttachDefs := []networkAttachmentConfig{
				newAttachmentConfigWithOverriddenName(secondaryNetworkName, f.Namespace.Name, secondaryNetworkName, "layer2", secondaryFlatL2NetworkCIDR),
				newAttachmentConfigWithOverriddenName(secondaryNetworkName+"-alias", f.Namespace.Name, secondaryNetworkName, "layer2", secondaryFlatL2NetworkCIDR),
			}

			for i := range netAttachDefs {
				netConfig := netAttachDefs[i]
				By("creating the attachment configuration")
				_, err := nadClient.NetworkAttachmentDefinitions(netConfig.namespace).Create(
					context.Background(),
					generateNAD(netConfig),
					metav1.CreateOptions{},
				)
				Expect(err).NotTo(HaveOccurred())
			}

			By("sitting on our hands for a couple secs we give the controller time to sync all NADs before provisioning policies and pods")
			// TODO: this is temporary. We hope to eventually sync pods & multi-net policies on NAD C/U/D ops
			time.Sleep(3 * time.Second)

			podConfig := podConfiguration{
				attachments: []nadapi.NetworkSelectionElement{
					{Name: secondaryNetworkName},
					{Name: secondaryNetworkName + "-alias"},
				},
				name:      podName,
				namespace: f.Namespace.Name,
			}
			By("creating the pod using a secondary network")
			var err error
			pod, err = cs.CoreV1().Pods(podConfig.namespace).Create(
				context.Background(),
				generatePodSpec(podConfig),
				metav1.CreateOptions{},
			)
			Expect(err).NotTo(HaveOccurred())

			By("asserting the pod gets to the `Ready` phase")
			Eventually(func() v1.PodPhase {
				updatedPod, err := cs.CoreV1().Pods(podConfig.namespace).Get(context.Background(), pod.GetName(), metav1.GetOptions{})
				if err != nil {
					return v1.PodFailed
				}
				return updatedPod.Status.Phase
			}, 2*time.Minute, 6*time.Second).Should(Equal(v1.PodRunning))
		})

		It("features two different IPs from the same subnet", func() {
			var err error
			pod, err = cs.CoreV1().Pods(pod.GetNamespace()).Get(context.Background(), pod.GetName(), metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			netStatus, err := podNetworkStatus(pod, func(status nadapi.NetworkStatus) bool {
				return !status.Default
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(netStatus).To(HaveLen(2))

			Expect(netStatus[0].IPs).To(HaveLen(1))
			Expect(netStatus[1].IPs).To(HaveLen(1))
			Expect(netStatus[0].IPs[0]).NotTo(Equal(netStatus[1].IPs[0]))
			Expect(inRange(secondaryFlatL2NetworkCIDR, netStatus[0].IPs[0]))
			Expect(inRange(secondaryFlatL2NetworkCIDR, netStatus[1].IPs[0]))
		})
	})
})

func kickstartPod(cs clientset.Interface, configuration podConfiguration) *v1.Pod {
	podNamespacedName := fmt.Sprintf("%s/%s", configuration.namespace, configuration.name)
	By(fmt.Sprintf("instantiating pod %q", podNamespacedName))
	createdPod, err := cs.CoreV1().Pods(configuration.namespace).Create(
		context.Background(),
		generatePodSpec(configuration),
		metav1.CreateOptions{},
	)
	Expect(err).WithOffset(1).NotTo(HaveOccurred())

	By(fmt.Sprintf("asserting that pod %q reaches the `Ready` state", podNamespacedName))
	EventuallyWithOffset(1, func() v1.PodPhase {
		updatedPod, err := cs.CoreV1().Pods(configuration.namespace).Get(context.Background(), configuration.name, metav1.GetOptions{})
		if err != nil {
			return v1.PodFailed
		}
		return updatedPod.Status.Phase
	}, 2*time.Minute, 6*time.Second).Should(Equal(v1.PodRunning))
	return createdPod
}

func createNads(f *framework.Framework, nadClient nadclient.K8sCniCncfIoV1Interface, extraNamespace *v1.Namespace, netConfig networkAttachmentConfig) error {
	for _, ns := range []*v1.Namespace{f.Namespace, extraNamespace} {
		By(fmt.Sprintf("creating the nad for namespace %q", ns.Name))
		netConfig.namespace = ns.Name
		_, err := nadClient.NetworkAttachmentDefinitions(ns.Name).Create(
			context.Background(),
			generateNAD(netConfig),
			metav1.CreateOptions{},
		)
		if err != nil {
			return err
		}
	}

	By("sitting on our hands for a couple secs we give the controller time to sync all NADs before provisioning policies and pods")
	// TODO: this is temporary. We hope to eventually sync pods & multi-net policies on NAD C/U/D ops
	time.Sleep(3 * time.Second)

	return nil
}

func kickstartPodInNamespace(cs clientset.Interface, podConfig *podConfiguration, defaultNamespace string, extraNamespace string) *v1.Pod {
	if podConfig.requiresExtraNamespace {
		podConfig.namespace = extraNamespace
	} else {
		podConfig.namespace = defaultNamespace
	}

	return kickstartPod(cs, *podConfig)
}

func assertServerPodIPInRange(cidr string, serverIP string, netPrefixLengthPerNode int) {
	By(fmt.Sprintf("asserting the server pod IP %v is from the configured range %v/%v", serverIP, cidr, netPrefixLengthPerNode))
	subnet, err := getNetCIDRSubnet(cidr)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	ExpectWithOffset(1, inRange(subnet, serverIP)).To(Succeed())
}

func createMultiNetworkPolicy(mnpClient mnpclient.K8sCniCncfIoV1beta1Interface, namespace string, policy *mnpapi.MultiNetworkPolicy) error {
	By("provisioning the multi-network policy")
	_, err := mnpClient.MultiNetworkPolicies(namespace).Create(
		context.Background(),
		policy,
		metav1.CreateOptions{},
	)
	return err
}

func computeIPWithOffset(baseAddr string, increment int) (string, error) {
	addr, err := netip.ParsePrefix(baseAddr)
	if err != nil {
		return "", fmt.Errorf("Failed to parse CIDR %v", err)
	}

	ip := addr.Addr()

	for i := 0; i < increment; i++ {
		ip = ip.Next()
		if !ip.IsValid() {
			return "", fmt.Errorf("overflow: IP address exceeds bounds")
		}
	}

	return netip.PrefixFrom(ip, addr.Bits()).String(), nil
}

// Given a node name and an offset, generateIPsFromNodePrimaryIfAddr returns an IPv4 and an IPv6 address
// at the provided offset from the primary interface addresses found on the node.
func generateIPsFromNodePrimaryIfAddr(cs clientset.Interface, nodeName string, offset int) ([]string, error) {
	var newAddresses []string

	node, err := cs.CoreV1().Nodes().Get(context.Background(), nodeName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("Failed to get node %s: %v", nodeName, err)
	}

	nodeIfAddr, err := util.GetNodeIfAddrAnnotation(node)
	if err != nil {
		return nil, err
	}
	nodeAddresses := []string{}
	if nodeIfAddr.IPv4 != "" {
		nodeAddresses = append(nodeAddresses, nodeIfAddr.IPv4)
	}
	if nodeIfAddr.IPv6 != "" {
		nodeAddresses = append(nodeAddresses, nodeIfAddr.IPv6)
	}
	for _, nodeAddress := range nodeAddresses {
		ipGen, err := ipgenerator.NewIPGenerator(nodeAddress)
		if err != nil {
			return nil, err
		}
		newIP, err := ipGen.GenerateIP(offset)
		if err != nil {
			return nil, err
		}
		newAddresses = append(newAddresses, newIP.String())
	}
	return newAddresses, nil
}

func addIPRequestToPodConfig(cs clientset.Interface, podConfig *podConfiguration, offset int) error {
	nodeName, ok := podConfig.nodeSelector[nodeHostnameKey]
	if !ok {
		return fmt.Errorf("No node selector found on podConfig")
	}

	IPsToRequest, err := generateIPsFromNodePrimaryIfAddr(cs, nodeName, offset)
	if err != nil {
		return err
	}
	for i := range podConfig.attachments {
		podConfig.attachments[i].IPRequest = IPsToRequest
	}
	return nil
}
