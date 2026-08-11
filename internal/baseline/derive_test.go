package baseline_test

import (
	"testing"

	"github.com/imkerbos/Distill/internal/baseline"
	"github.com/imkerbos/Distill/internal/fixture"
	"github.com/imkerbos/Distill/internal/snapshot"
)

// asia 的 gateway namespace 对外暴露，五类应当齐备。
func TestDeriveOnGatewayNamespaceIsComplete(t *testing.T) {
	c, ok := fixture.Load().Cluster("prod-asia-1")
	if !ok {
		t.Fatal("cluster prod-asia-1 missing")
	}
	set := baseline.Derive(c.Assets, "gateway")
	if missing := set.Missing(); len(missing) != 0 {
		t.Errorf("Missing() = %v, want none for an exposed namespace", missing)
	}
}

// batch 没有暴露面、也不是抓取目标，必然缺两类。
// Missing() 恒返回空就是个摆设 —— 必须有数据能让它非空。
func TestDeriveOnUnexposedNamespaceReportsMissing(t *testing.T) {
	c, _ := fixture.Load().Cluster("prod-asia-1")
	missing := baseline.Derive(c.Assets, "batch").Missing()
	want := map[baseline.Kind]bool{
		baseline.KindLBHealth: false, baseline.KindMetrics: false,
	}
	for _, k := range missing {
		if _, expected := want[k]; !expected {
			t.Errorf("unexpected missing kind %q", k)
		}
		want[k] = true
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("kind %q not reported missing, but batch has no such derivation source", k)
		}
	}
}

// DNS 与 control plane 与 namespace 无关，每个 namespace 都该有。
func TestDeriveAlwaysIncludesDNSAndControlPlane(t *testing.T) {
	c, _ := fixture.Load().Cluster("prod-asia-1")
	for _, ns := range []string{"gateway", "payment", "batch", "legacy"} {
		kinds := map[baseline.Kind]bool{}
		for _, k := range baseline.Derive(c.Assets, ns).Kinds() {
			kinds[k] = true
		}
		if !kinds[baseline.KindDNS] {
			t.Errorf("%s: DNS baseline absent; every namespace needs DNS egress", ns)
		}
		if !kinds[baseline.KindControlPlane] {
			t.Errorf("%s: control-plane baseline absent", ns)
		}
	}
}

// NODE_AGENT 是集群级的：集群里登记了 agent，每个 namespace 都该有这条规则。
func TestNodeAgentIsClusterWide(t *testing.T) {
	for _, c := range fixture.Load().Clusters {
		for _, ns := range c.Namespaces {
			kinds := map[baseline.Kind]bool{}
			for _, k := range baseline.Derive(c.Assets, ns.Name).Kinds() {
				kinds[k] = true
			}
			if !kinds[baseline.KindNodeAgent] {
				t.Errorf("%s/%s: NODE_AGENT absent; node agents collect from the whole cluster",
					c.ID, ns.Name)
			}
		}
	}
}

// 没有登记任何 NodeAgent 时必须报缺失。
//
// fixture 两个集群都登记了 agent，这条分支在任何 fixture 数据上都走不到 ——
// 一项永远走不到的校验在测试里等于不存在（spec §3）。因此这里手造快照。
func TestNodeAgentMissingWhenNoneRegistered(t *testing.T) {
	a := snapshot.Assets{
		ClusterID: "c-no-agent",
		Registry:  snapshot.ClusterRegistry{ClusterID: "c-no-agent", NodeCIDR: "10.128.0.0/20"},
	}
	if !containsKind(baseline.Derive(a, "payment").Missing(), baseline.KindNodeAgent) {
		t.Error("Missing() omits NODE_AGENT although no NodeAgent is registered")
	}
}

// 登记了 agent 但没有 node CIDR 时同样必须报缺失。
//
// 缺 CIDR 只能靠硬编码常量表补齐，而常量表不会随网段变更更新
// （spec §7.2）；宁可报缺失，也不能编一个网段出来。
func TestNodeAgentMissingWhenNodeCIDREmpty(t *testing.T) {
	a := snapshot.Assets{
		ClusterID: "c-no-cidr",
		NodeAgents: []snapshot.NodeAgent{{
			ClusterID: "c-no-cidr", Namespace: "kube-system", App: "log-agent",
			HostNetwork: true, TargetPort: 9100,
		}},
		Registry: snapshot.ClusterRegistry{ClusterID: "c-no-cidr"},
	}
	if !containsKind(baseline.Derive(a, "payment").Missing(), baseline.KindNodeAgent) {
		t.Error("Missing() omits NODE_AGENT although Registry.NodeCIDR is empty")
	}
}

func containsKind(kinds []baseline.Kind, want baseline.Kind) bool {
	for _, k := range kinds {
		if k == want {
			return true
		}
	}
	return false
}

// 每条规则都必须带推导依据，一条都不能漏。
func TestEveryDerivedRuleCarriesDerivations(t *testing.T) {
	for _, c := range fixture.Load().Clusters {
		for _, ns := range c.Namespaces {
			for _, r := range baseline.Derive(c.Assets, ns.Name).Rules {
				if len(r.Derivations) == 0 {
					t.Errorf("%s/%s: %s rule has no derivation", c.ID, ns.Name, r.Kind)
				}
				if !r.Kind.Valid() {
					t.Errorf("%s/%s: unregistered kind %q", c.ID, ns.Name, r.Kind)
				}
			}
		}
	}
}
