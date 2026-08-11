package registry_test

import (
	"testing"

	"github.com/imkerbos/Distill/internal/registry"
)

func TestOnboardStateEnumIsClosed(t *testing.T) {
	states := registry.AllOnboardStates()
	if len(states) != 3 {
		t.Fatalf("AllOnboardStates() = %d entries, want 3", len(states))
	}
	for _, s := range states {
		if !s.Valid() {
			t.Errorf("registered state %q reported invalid", s)
		}
	}
	if registry.OnboardState("ENFORCED").Valid() {
		t.Error("unregistered state reported valid")
	}
}

func TestImportRoleEnumIsClosed(t *testing.T) {
	roles := registry.AllImportRoles()
	if len(roles) != 2 {
		t.Fatalf("AllImportRoles() = %d entries, want 2", len(roles))
	}
	for _, r := range roles {
		if !r.Valid() {
			t.Errorf("registered role %q reported invalid", r)
		}
	}
	if registry.ImportRole("CURRENT").Valid() {
		t.Error("unregistered role reported valid")
	}
}

func TestImportSourceEnumIsClosed(t *testing.T) {
	sources := registry.AllImportSources()
	if len(sources) != 3 {
		t.Fatalf("AllImportSources() = %d entries, want 3", len(sources))
	}
	for _, s := range sources {
		if !s.Valid() {
			t.Errorf("registered source %q reported invalid", s)
		}
	}
	if registry.ImportSource("UPLOAD").Valid() {
		t.Error("unregistered source reported valid")
	}
}

// ToSnapshot 是 registry 与既有 Baseline 推导之间的唯一桥梁。
// 字段漏映射不会有编译错误，只会让某类 Baseline 悄悄推导不出来。
func TestToSnapshotCarriesEveryDerivationSource(t *testing.T) {
	c := registry.Cluster{
		ID: "c1", PodCIDR: "10.4.0.0/14", NodeCIDR: "10.128.0.0/20",
		APIServers:         []registry.APIServer{{Host: "10.9.0.2", CIDR: "10.9.0.0/28", Port: 443}},
		HealthCheckSources: []string{"35.191.0.0/16", "130.211.0.0/22"},
	}
	got := c.ToSnapshot()
	if got.ClusterID != "c1" {
		t.Errorf("ClusterID = %q, want c1", got.ClusterID)
	}
	if got.PodCIDR != "10.4.0.0/14" || got.NodeCIDR != "10.128.0.0/20" {
		t.Errorf("CIDRs = %q / %q, want the registered values", got.PodCIDR, got.NodeCIDR)
	}
	if len(got.HealthCheckSources) != 2 {
		t.Errorf("HealthCheckSources = %v, want 2 entries", got.HealthCheckSources)
	}
}
