package main

import (
	"slices"
	"testing"

	"github.com/imkerbos/Distill/internal/cluster"
	"github.com/imkerbos/Distill/internal/registry"
)

// registeredFleet 是两个网段都登记齐全的集群。
func registeredFleet() []registry.Cluster {
	return []registry.Cluster{
		{ID: "prod", PodCIDR: "10.4.0.0/16", NodeCIDR: "192.168.0.0/24"},
		{ID: "staging", PodCIDR: "10.8.0.0/16", NodeCIDR: "192.168.1.0/24"},
	}
}

// classify 是一次判定，失败即致命 —— 判定本身出错与判定结果不对是两件事。
func classify(t *testing.T, reg *cluster.Registry, ip string) cluster.Classification {
	t.Helper()
	got, err := reg.Classify(ip)
	if err != nil {
		t.Fatalf("Classify(%q) error = %v", ip, err)
	}
	return got
}

func TestFleetRegistryKeepsEachClusterOwnCIDRs(t *testing.T) {
	reg, unusable := fleetRegistry(registeredFleet())
	if len(unusable) != 0 {
		t.Fatalf("unusable = %v, want none for a fully registered fleet", unusable)
	}

	if got := classify(t, reg, "10.8.1.7"); got.Scope != cluster.ScopePod || got.ClusterID != "staging" {
		t.Errorf("Classify(10.8.1.7) = %s/%s, want POD/staging — a pod IP must resolve to the cluster that registered its CIDR",
			got.Scope, got.ClusterID)
	}
	if got := classify(t, reg, "192.168.0.10"); got.Scope != cluster.ScopeNode || got.ClusterID != "prod" {
		t.Errorf("Classify(192.168.0.10) = %s/%s, want NODE/prod", got.Scope, got.ClusterID)
	}
}

// Service ClusterIP 判成 UNKNOWN 是 spec §2.3 的直接后果，不是缺陷：
// registry.Cluster 没有 Service 网段字段，而"没登记"不得被说成"在集群外"。
// 这条用例钉住那个后果 —— 哪天有人给适配器补上 ServiceCIDRs 却忘了改
// registry 的表结构，它会当场变红。
func TestServiceIPsAreUnknownRatherThanExternal(t *testing.T) {
	reg, _ := fleetRegistry(registeredFleet())

	got := classify(t, reg, "10.96.0.10")
	if got.Scope == cluster.ScopeExternal {
		t.Fatal("Classify(10.96.0.10) = EXTERNAL, want UNKNOWN — calling an unregistered service CIDR 'outside the fleet' is a conclusion we have no basis for")
	}
	if got.Scope != cluster.ScopeUnknown {
		t.Fatalf("Classify(10.96.0.10) scope = %s, want UNKNOWN", got.Scope)
	}
	if got.Reason != cluster.ReasonServiceCIDRUnregistered {
		t.Errorf("reason = %q, want %q — the reason must name what is missing",
			got.Reason, cluster.ReasonServiceCIDRUnregistered)
	}
}

func TestFleetRegistryReportsUnusableRegistration(t *testing.T) {
	cases := []struct {
		name    string
		cluster registry.Cluster
	}{
		{"empty pod cidr", registry.Cluster{ID: "broken", NodeCIDR: "192.168.2.0/24"}},
		{"unparsable pod cidr", registry.Cluster{ID: "broken", PodCIDR: "10.4.0.0/33", NodeCIDR: "192.168.2.0/24"}},
		{"unparsable node cidr", registry.Cluster{ID: "broken", PodCIDR: "10.4.0.0/16", NodeCIDR: "not-a-cidr"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, unusable := fleetRegistry(append(registeredFleet(), c.cluster))
			if !slices.Contains(unusable, "broken") {
				t.Fatalf("unusable = %v, want it to name %q — an operator cannot fix a registration nobody reported",
					unusable, "broken")
			}
		})
	}
}

// 登记坏掉的集群必须留在判定表里。删掉它，它的 Pod IP 会被判成 EXTERNAL
// 或者归到别的集群名下 —— 一个下游会当作事实使用的错误结论。留下来，
// 判定退化成 UNKNOWN 并说得出缺什么。
func TestAClusterWithBrokenRegistrationStillDegradesTheAnswer(t *testing.T) {
	reg, _ := fleetRegistry(append(registeredFleet(), registry.Cluster{ID: "broken", NodeCIDR: "192.168.2.0/24"}))

	got := classify(t, reg, "10.99.0.1")
	if got.Scope != cluster.ScopeUnknown {
		t.Fatalf("Classify(10.99.0.1) scope = %s, want UNKNOWN while some cluster has no pod CIDR registered", got.Scope)
	}
	if got.Reason != cluster.ReasonPodCIDRUnregistered {
		t.Errorf("reason = %q, want %q", got.Reason, cluster.ReasonPodCIDRUnregistered)
	}
}
