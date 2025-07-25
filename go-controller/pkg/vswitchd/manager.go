// Code generated by "libovsdb.modelgen"
// DO NOT EDIT.

package vswitchd

import "github.com/ovn-kubernetes/libovsdb/model"

const ManagerTable = "Manager"

type (
	ManagerConnectionMode = string
)

var (
	ManagerConnectionModeInBand    ManagerConnectionMode = "in-band"
	ManagerConnectionModeOutOfBand ManagerConnectionMode = "out-of-band"
)

// Manager defines an object in Manager table
type Manager struct {
	UUID            string                 `ovsdb:"_uuid"`
	ConnectionMode  *ManagerConnectionMode `ovsdb:"connection_mode"`
	ExternalIDs     map[string]string      `ovsdb:"external_ids"`
	InactivityProbe *int                   `ovsdb:"inactivity_probe"`
	IsConnected     bool                   `ovsdb:"is_connected"`
	MaxBackoff      *int                   `ovsdb:"max_backoff"`
	OtherConfig     map[string]string      `ovsdb:"other_config"`
	Status          map[string]string      `ovsdb:"status"`
	Target          string                 `ovsdb:"target"`
}

func (a *Manager) GetUUID() string {
	return a.UUID
}

func (a *Manager) GetConnectionMode() *ManagerConnectionMode {
	return a.ConnectionMode
}

func copyManagerConnectionMode(a *ManagerConnectionMode) *ManagerConnectionMode {
	if a == nil {
		return nil
	}
	b := *a
	return &b
}

func equalManagerConnectionMode(a, b *ManagerConnectionMode) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	if a == b {
		return true
	}
	return *a == *b
}

func (a *Manager) GetExternalIDs() map[string]string {
	return a.ExternalIDs
}

func copyManagerExternalIDs(a map[string]string) map[string]string {
	if a == nil {
		return nil
	}
	b := make(map[string]string, len(a))
	for k, v := range a {
		b[k] = v
	}
	return b
}

func equalManagerExternalIDs(a, b map[string]string) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || v != w {
			return false
		}
	}
	return true
}

func (a *Manager) GetInactivityProbe() *int {
	return a.InactivityProbe
}

func copyManagerInactivityProbe(a *int) *int {
	if a == nil {
		return nil
	}
	b := *a
	return &b
}

func equalManagerInactivityProbe(a, b *int) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	if a == b {
		return true
	}
	return *a == *b
}

func (a *Manager) GetIsConnected() bool {
	return a.IsConnected
}

func (a *Manager) GetMaxBackoff() *int {
	return a.MaxBackoff
}

func copyManagerMaxBackoff(a *int) *int {
	if a == nil {
		return nil
	}
	b := *a
	return &b
}

func equalManagerMaxBackoff(a, b *int) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	if a == b {
		return true
	}
	return *a == *b
}

func (a *Manager) GetOtherConfig() map[string]string {
	return a.OtherConfig
}

func copyManagerOtherConfig(a map[string]string) map[string]string {
	if a == nil {
		return nil
	}
	b := make(map[string]string, len(a))
	for k, v := range a {
		b[k] = v
	}
	return b
}

func equalManagerOtherConfig(a, b map[string]string) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || v != w {
			return false
		}
	}
	return true
}

func (a *Manager) GetStatus() map[string]string {
	return a.Status
}

func copyManagerStatus(a map[string]string) map[string]string {
	if a == nil {
		return nil
	}
	b := make(map[string]string, len(a))
	for k, v := range a {
		b[k] = v
	}
	return b
}

func equalManagerStatus(a, b map[string]string) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || v != w {
			return false
		}
	}
	return true
}

func (a *Manager) GetTarget() string {
	return a.Target
}

func (a *Manager) DeepCopyInto(b *Manager) {
	*b = *a
	b.ConnectionMode = copyManagerConnectionMode(a.ConnectionMode)
	b.ExternalIDs = copyManagerExternalIDs(a.ExternalIDs)
	b.InactivityProbe = copyManagerInactivityProbe(a.InactivityProbe)
	b.MaxBackoff = copyManagerMaxBackoff(a.MaxBackoff)
	b.OtherConfig = copyManagerOtherConfig(a.OtherConfig)
	b.Status = copyManagerStatus(a.Status)
}

func (a *Manager) DeepCopy() *Manager {
	b := new(Manager)
	a.DeepCopyInto(b)
	return b
}

func (a *Manager) CloneModelInto(b model.Model) {
	c := b.(*Manager)
	a.DeepCopyInto(c)
}

func (a *Manager) CloneModel() model.Model {
	return a.DeepCopy()
}

func (a *Manager) Equals(b *Manager) bool {
	return a.UUID == b.UUID &&
		equalManagerConnectionMode(a.ConnectionMode, b.ConnectionMode) &&
		equalManagerExternalIDs(a.ExternalIDs, b.ExternalIDs) &&
		equalManagerInactivityProbe(a.InactivityProbe, b.InactivityProbe) &&
		a.IsConnected == b.IsConnected &&
		equalManagerMaxBackoff(a.MaxBackoff, b.MaxBackoff) &&
		equalManagerOtherConfig(a.OtherConfig, b.OtherConfig) &&
		equalManagerStatus(a.Status, b.Status) &&
		a.Target == b.Target
}

func (a *Manager) EqualsModel(b model.Model) bool {
	c := b.(*Manager)
	return a.Equals(c)
}

var _ model.CloneableModel = &Manager{}
var _ model.ComparableModel = &Manager{}
