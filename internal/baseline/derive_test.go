package baseline_test

import (
	"testing"

	"github.com/imkerbos/Distill/internal/baseline"
	"github.com/imkerbos/Distill/internal/fixture"
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
