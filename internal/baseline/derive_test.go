package baseline_test

import (
	"testing"

	"github.com/imkerbos/Distill/internal/baseline"
	"github.com/imkerbos/Distill/internal/fixture"
	"github.com/imkerbos/Distill/internal/snapshot"
)

// asia 的 gateway namespace 对外暴露，各类应当齐备。
func TestDeriveOnGatewayNamespaceIsComplete(t *testing.T) {
	c, ok := fixture.Load().Cluster("prod-asia-1")
	if !ok {
		t.Fatal("cluster prod-asia-1 missing")
	}
	set := baseline.Derive(c.Assets, "gateway", nil)
	if missing := set.Missing(); len(missing) != 0 {
		t.Errorf("Missing() = %v, want none for an exposed namespace", missing)
	}
}

// Missing() 必须能非空 —— 恒返回空就是个摆设。
//
// 用 DNS 来守这条，而不再用 batch 的 LB/METRICS：那两类在 batch 上
// 根本没有推导对象，如实报出来是「不适用」而不是「缺失」
// （design doc 2026-08-18-baseline-applicability §1，用例在
// applicability_test.go）。
//
// DNS 恒适用，因此它推不出来时只可能是缺失 —— 这是这条守卫要的那种数据。
func TestMissingIsNotEmptyWhenAnAlwaysApplicableSourceIsGone(t *testing.T) {
	c, _ := fixture.Load().Cluster("prod-asia-1")
	a := c.Assets
	// 抽掉 kube-dns 的后端地址，DNS 就推不出规则了。
	var kept []snapshot.Endpoints
	for _, e := range a.Endpoints {
		if e.Namespace == "kube-system" && e.Name == "kube-dns" {
			continue
		}
		kept = append(kept, e)
	}
	a.Endpoints = kept

	set := baseline.Derive(a, "batch", nil)
	var sawDNS bool
	for _, k := range set.Missing() {
		if k == baseline.KindDNS {
			sawDNS = true
		}
	}
	if !sawDNS {
		t.Error("DNS not reported missing after its derivation source was removed; Missing() would be a no-op")
	}
	for _, k := range set.NotApplicable {
		if k == baseline.KindDNS {
			t.Error("DNS reported inapplicable; every pod resolves names, so it can only ever be missing")
		}
	}
}

// DNS 与 control plane 与 namespace 无关，每个 namespace 都该有。
func TestDeriveAlwaysIncludesDNSAndControlPlane(t *testing.T) {
	c, _ := fixture.Load().Cluster("prod-asia-1")
	for _, ns := range []string{"gateway", "payment", "batch", "legacy"} {
		kinds := map[baseline.Kind]bool{}
		for _, k := range baseline.Derive(c.Assets, ns, nil).Kinds() {
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
			for _, k := range baseline.Derive(c.Assets, ns.Name, nil).Kinds() {
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
	if !containsKind(baseline.Derive(a, "payment", nil).Missing(), baseline.KindNodeAgent) {
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
	if !containsKind(baseline.Derive(a, "payment", nil).Missing(), baseline.KindNodeAgent) {
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
			for _, r := range baseline.Derive(c.Assets, ns.Name, nil).Rules {
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
