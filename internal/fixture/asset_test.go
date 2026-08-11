package fixture_test

import (
	"testing"

	"github.com/imkerbos/Distill/internal/fixture"
)

// 五类必备 Baseline 各需要一个推导来源。任何一类缺了对应快照对象，
// 推导就只能靠硬编码常量表 —— 那正是 spec §7.2 禁止的东西。
func TestEveryClusterHasDerivationSourcesForAllFiveBaselines(t *testing.T) {
	for _, c := range fixture.Load().Clusters {
		a := c.Assets
		if a.ClusterID != c.ID {
			t.Errorf("%s: Assets.ClusterID = %q, want %q", c.ID, a.ClusterID, c.ID)
		}
		if _, ok := a.Service("kube-system", "kube-dns"); !ok {
			t.Errorf("%s: kube-dns Service missing; DNS baseline has no derivation source", c.ID)
		}
		if _, ok := a.EndpointsFor("kube-system", "kube-dns"); !ok {
			t.Errorf("%s: kube-dns Endpoints missing; cannot confirm DNS backends exist", c.ID)
		}
		if len(a.APIServers) == 0 {
			t.Errorf("%s: no APIServerEndpoint; control-plane baseline has no derivation source", c.ID)
		}
		if len(a.ScrapeTargets) == 0 {
			t.Errorf("%s: no ScrapeTarget; metrics baseline has no derivation source", c.ID)
		}
		if len(a.NodeAgents) == 0 {
			t.Errorf("%s: no NodeAgent; node-agent baseline has no derivation source", c.ID)
		}
		if a.Registry.NodeCIDR == "" {
			t.Errorf("%s: Registry.NodeCIDR empty; node-agent baseline would need a hardcoded CIDR", c.ID)
		}
		if len(a.Registry.HealthCheckSources) == 0 {
			t.Errorf("%s: Registry.HealthCheckSources empty; LB baseline would need a hardcoded CIDR", c.ID)
		}
	}
}

// 只有存在 LoadBalancer 暴露的集群才该有 Gateway 对象。
// asia 有 gateway namespace，eu 没有 —— 两种情况都要能走到，
// 否则「暴露面缺失时不生成 LB baseline」这条路径永远测不到。
func TestOnlyAsiaHasGatewayExposure(t *testing.T) {
	f := fixture.Load()
	asia, _ := f.Cluster("prod-asia-1")
	eu, _ := f.Cluster("prod-eu-1")

	if len(asia.Assets.Gateways) == 0 {
		t.Error("prod-asia-1 has no Gateway; LB baseline path is untestable")
	}
	if len(eu.Assets.Gateways) != 0 {
		t.Error("prod-eu-1 has Gateway; the missing-exposure path is no longer demonstrated")
	}
}

// kube-dns 的后端 Pod 必须真实存在于 Pod 快照里。
// Service 有 selector 但没有任何 Pod 匹配时，生成的规则指向空集：
// 看起来齐备、实际什么都没放行。
func TestKubeDNSSelectorMatchesRealPods(t *testing.T) {
	for _, c := range fixture.Load().Clusters {
		svc, ok := c.Assets.Service("kube-system", "kube-dns")
		if !ok {
			t.Fatalf("%s: kube-dns Service missing", c.ID)
		}
		matched := 0
		for _, p := range c.Pods {
			if p.Namespace != "kube-system" {
				continue
			}
			all := true
			for k, v := range svc.Selector {
				if p.Labels[k] != v {
					all = false
					break
				}
			}
			if all && len(svc.Selector) > 0 {
				matched++
			}
		}
		if matched == 0 {
			t.Errorf("%s: kube-dns selector %v matches no Pod", c.ID, svc.Selector)
		}
	}
}
