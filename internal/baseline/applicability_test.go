package baseline_test

import (
	"slices"
	"testing"

	"github.com/imkerbos/Distill/internal/baseline"
	"github.com/imkerbos/Distill/internal/fixture"
	"github.com/imkerbos/Distill/internal/snapshot"
)

func assetsOf(t *testing.T, clusterID string) snapshot.Assets {
	t.Helper()
	c, ok := fixture.Load().Cluster(clusterID)
	if !ok {
		t.Fatalf("cluster %s missing", clusterID)
	}
	return c.Assets
}

// batch 没有入口暴露对象、也没有 Pod 声明自己要被抓，因此这两类
// **不适用**，而不是缺失。九条误报里的两条就是它（spec §1）。
func TestAnUnexposedNamespaceNeedsNoLBOrMetricsBaseline(t *testing.T) {
	set := baseline.Derive(assetsOf(t, "prod-asia-1"), "batch", nil)

	for _, k := range []baseline.Kind{baseline.KindLBHealth, baseline.KindMetrics} {
		if !slices.Contains(set.NotApplicable, k) {
			t.Errorf("%s not reported inapplicable for batch, which has neither an exposure object nor a scrape declaration", k)
		}
		if slices.Contains(set.Missing(), k) {
			t.Errorf("%s still reported missing for batch — a namespace with nothing to allow cannot be missing an allowance", k)
		}
	}
}

// 守 spec §4.1：暴露面不只是 Ingress。
//
// batch 加一个 type=LoadBalancer 的 Service（没有 Gateway）。健康检查打的
// 是它，因此这一类既**适用**、也**推得出** —— deriveLBHealth 直接从这个
// Service 推出健康检查放行。
//
// 一个只看 a.Gateways 的适用性实现会在这里答"不适用"，放行一次会打断入口
// 的下发；一个只从 a.Gateways 推导的实现会答"适用却缺失"，让写回 gate 把
// 这个 namespace 永久卡死（kind gateway ns 实测出的那个缺口）。
func TestALoadBalancerServiceMakesTheLBBaselineApplicable(t *testing.T) {
	a := assetsOf(t, "prod-asia-1")
	a.Services = append(slices.Clone(a.Services), snapshot.Service{
		ClusterID: "prod-asia-1", Namespace: "batch", Name: "batch-lb",
		Type: "LoadBalancer", Ports: []snapshot.ServicePort{{Port: 80, TargetPort: 8080, Protocol: "TCP"}},
	})

	set := baseline.Derive(a, "batch", nil)
	if slices.Contains(set.NotApplicable, baseline.KindLBHealth) {
		t.Error("LB_HEALTH_CHECK reported inapplicable for a namespace exposed by a LoadBalancer Service")
	}
	if slices.Contains(set.Missing(), baseline.KindLBHealth) {
		t.Error("LB_HEALTH_CHECK still missing for a LoadBalancer-exposed namespace; the rule must derive from the Service")
	}
}

// NodePort 同样算暴露面：它常作为外部 LB 的后端，一样被健康检查打。
func TestANodePortServiceAlsoMakesTheLBBaselineApplicable(t *testing.T) {
	a := assetsOf(t, "prod-asia-1")
	a.Services = append(slices.Clone(a.Services), snapshot.Service{
		ClusterID: "prod-asia-1", Namespace: "batch", Name: "batch-np",
		Type: "NodePort", Ports: []snapshot.ServicePort{{Port: 80, TargetPort: 8080, Protocol: "TCP"}},
	})
	if slices.Contains(baseline.Derive(a, "batch", nil).NotApplicable, baseline.KindLBHealth) {
		t.Error("LB_HEALTH_CHECK reported inapplicable for a namespace exposed by a NodePort Service")
	}
}

// 守 spec §4.2：判据取被抓端的声明，不取 ScrapeTargets。
//
// batch 里放一个声明了 prometheus.io/scrape 的 Pod，但集群没有登记任何
// 抓取端 —— ScrapeTargets 因此仍然是空的。这一类必须变回**适用且缺失**：
// 有东西要被抓、平台推不出规则，正是该挡住的那一次。
//
// 一个拿 ScrapeTargets 当判据的实现会在这里答"不适用"，于是一个尚未登记
// 抓取端的集群每个 namespace 都能过门，而真正的 Prometheus 会被挡。
func TestAScrapeDeclarationMakesTheMetricsBaselineApplicableWithoutAnyScraper(t *testing.T) {
	a := assetsOf(t, "prod-asia-1")
	a.ScrapeTargets = nil
	a.ScrapeDeclarations = []snapshot.ScrapeDeclaration{{
		ClusterID: "prod-asia-1", Namespace: "batch", PodName: "batch-worker-0",
	}}

	set := baseline.Derive(a, "batch", nil)
	if slices.Contains(set.NotApplicable, baseline.KindMetrics) {
		t.Error("METRICS_SCRAPE reported inapplicable while a pod in batch declares prometheus.io/scrape")
	}
	if !slices.Contains(set.Missing(), baseline.KindMetrics) {
		t.Error("METRICS_SCRAPE not reported missing: something wants to be scraped and no rule was derived")
	}
}

// DNS 与 control plane 对每个 Pod 都成立，永远不许落进"不适用"。
// NODE_AGENT 的不适用是一次人工声明（node-agent spec §3），不由这里推断。
func TestTheAlwaysApplicableKindsAreNeverInapplicable(t *testing.T) {
	a := assetsOf(t, "prod-asia-1")
	for _, ns := range []string{"gateway", "payment", "batch", "legacy", "kube-system"} {
		for _, k := range []baseline.Kind{baseline.KindDNS, baseline.KindControlPlane, baseline.KindNodeAgent} {
			if slices.Contains(baseline.Derive(a, ns, nil).NotApplicable, k) {
				t.Errorf("%s: %s reported inapplicable; it holds for every pod in every namespace", ns, k)
			}
		}
	}
}

// 齐备的 namespace 一个字节都不变：既不缺，也没有任何一类被判成不适用。
func TestACompleteNamespaceReportsNeitherMissingNorInapplicable(t *testing.T) {
	set := baseline.Derive(assetsOf(t, "prod-asia-1"), "gateway", nil)
	if m := set.Missing(); len(m) != 0 {
		t.Errorf("Missing() = %v, want none for an exposed namespace", m)
	}
	if len(set.NotApplicable) != 0 {
		t.Errorf("NotApplicable = %v, want none: every kind has a derivation source here", set.NotApplicable)
	}
}

// **没采回依据的那一类不许被判成"不适用"。**
//
// 这是本轮唯一真正危险的地方：Service 那次采集被 403 拒掉时 a.Services 是
// 空的，而"没有暴露对象"与"没看过有没有暴露对象"在资产里长得一模一样。
// 把后者读成"这个 namespace 不需要健康检查放行"，就是把一次采集失败变成
// 一次放行 —— 而它随后会从缺失清单里消失，连带绕过 Enforcing 门禁。
//
// 失败方向必须朝"不知道"：这一类照旧留在缺失里，由 NotAssessedBaselines
// 标注（design doc 2026-08-18-baseline-applicability §3）。
func TestAnUnassessedKindIsNeverJudgedInapplicable(t *testing.T) {
	a := assetsOf(t, "prod-asia-1")
	// 依据这次没采回来：两类的资产都是空的，与"集群里就是没有"无法区分。
	a.Gateways = nil
	a.Services = nil
	a.ScrapeDeclarations = nil

	set := baseline.Derive(a, "batch", []baseline.Kind{baseline.KindLBHealth, baseline.KindMetrics})

	for _, k := range []baseline.Kind{baseline.KindLBHealth, baseline.KindMetrics} {
		if slices.Contains(set.NotApplicable, k) {
			t.Errorf("%s judged inapplicable while its evidence was never assessed; "+
				"an empty asset list means \"we did not look\" here, not \"the cluster has none\"", k)
		}
		if !slices.Contains(set.Missing(), k) {
			t.Errorf("%s left the missing list although nobody assessed its evidence", k)
		}
	}
}
