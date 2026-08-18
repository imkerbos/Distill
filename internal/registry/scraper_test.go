package registry_test

import (
	"errors"
	"testing"

	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/snapshot"
)

func validScraper() registry.MetricsScraper {
	return registry.MetricsScraper{
		Namespace: "monitoring",
		Labels:    map[string]string{"app.kubernetes.io/name": "prometheus"},
	}
}

func TestValidateMetricsScraperAcceptsAWellFormedOne(t *testing.T) {
	if err := registry.ValidateMetricsScraper(validScraper()); err != nil {
		t.Fatalf("ValidateMetricsScraper(valid) = %v, want nil", err)
	}
}

func TestValidateMetricsScraperRequiresBothHalves(t *testing.T) {
	// 少了命名空间，生成的 namespaceSelector 会匹配到所有命名空间；
	// 少了标签，podSelector 会匹配到那个命名空间里的每一个 Pod。
	// 两种情况下规则都比操作者以为的宽得多，而它照样"生成成功"。
	noNS := validScraper()
	noNS.Namespace = ""
	if err := registry.ValidateMetricsScraper(noNS); err == nil {
		t.Error("ValidateMetricsScraper(no namespace) = nil, want an error")
	}
	noLabels := validScraper()
	noLabels.Labels = nil
	err := registry.ValidateMetricsScraper(noLabels)
	if err == nil {
		t.Fatal("ValidateMetricsScraper(no labels) = nil — 一个空 podSelector 会" +
			"放行那个命名空间里的每一个 Pod")
	}
	if !errors.Is(err, registry.ErrInvalid) {
		t.Errorf("error = %v, want ErrInvalid", err)
	}
	empty := validScraper()
	empty.Labels = map[string]string{}
	if err := registry.ValidateMetricsScraper(empty); err == nil {
		t.Error("ValidateMetricsScraper(empty labels map) = nil, want an error")
	}
}

func TestLabelsKeyIsStableRegardlessOfMapOrder(t *testing.T) {
	// 主键靠它。同一组标签算出两个不同的键，会让同一个抓取端被登记两次，
	// 于是同一条 ingress 规则生成两遍。
	a := registry.MetricsScraper{Namespace: "monitoring", Labels: map[string]string{
		"app": "prometheus", "release": "kube-prometheus-stack", "tier": "monitoring",
	}}
	b := registry.MetricsScraper{Namespace: "monitoring", Labels: map[string]string{
		"tier": "monitoring", "app": "prometheus", "release": "kube-prometheus-stack",
	}}
	if a.LabelsKey() != b.LabelsKey() {
		t.Errorf("LabelsKey() = %q and %q for the same labels", a.LabelsKey(), b.LabelsKey())
	}
	if a.LabelsKey() == "" {
		t.Error("LabelsKey() is empty for a scraper that has labels")
	}
	// 不同的标签必须给出不同的键，否则两个抓取端会互相覆盖。
	c := registry.MetricsScraper{Namespace: "monitoring", Labels: map[string]string{"app": "other"}}
	if a.LabelsKey() == c.LabelsKey() {
		t.Error("two different label sets produced the same key")
	}
}

func scrapablePod(namespace, name, port string) snapshot.Pod {
	return snapshot.Pod{
		Namespace: namespace, Name: name,
		ScrapeAnnotations: map[string]string{
			"prometheus.io/scrape": "true",
			"prometheus.io/port":   port,
		},
	}
}

func TestScrapeTargetsJoinRegisteredScrapersWithObservedTargets(t *testing.T) {
	c := registry.Cluster{
		ID:              "prod-asia-1",
		MetricsScrapers: []registry.MetricsScraper{validScraper()},
	}
	pods := []snapshot.Pod{
		scrapablePod("shop", "web-1", "9102"),
		scrapablePod("payment", "api-1", "8080"),
	}

	got := c.ScrapeTargetSnapshots(pods)

	if len(got) != 2 {
		t.Fatalf("ScrapeTargetSnapshots() returned %d targets, want 2: %+v", len(got), got)
	}
	for _, target := range got {
		// 抓取端来自登记 —— 这一半观测不出来。
		if target.ScraperNamespace != "monitoring" {
			t.Errorf("ScraperNamespace = %q, want monitoring", target.ScraperNamespace)
		}
		if target.ScraperLabels["app.kubernetes.io/name"] != "prometheus" {
			t.Errorf("ScraperLabels = %v, want the registered ones", target.ScraperLabels)
		}
		if target.ClusterID != "prod-asia-1" {
			t.Errorf("ClusterID = %q, want prod-asia-1", target.ClusterID)
		}
	}
}

func TestScrapeTargetsSkipPodsThatDeclareNothingUsable(t *testing.T) {
	c := registry.Cluster{ID: "c", MetricsScrapers: []registry.MetricsScraper{validScraper()}}
	cases := map[string]snapshot.Pod{
		// 没声明过：绝大多数 Pod 是这一类。
		"no annotations": {Namespace: "shop", Name: "quiet"},
		// 明确说了不抓。
		"scrape=false": {Namespace: "shop", Name: "opted-out", ScrapeAnnotations: map[string]string{
			"prometheus.io/scrape": "false", "prometheus.io/port": "9102",
		}},
		// **说了要抓却没给端口：不许猜一个。** 一条放行到猜出来的端口的规则，
		// 看起来齐备、实际什么都没放行，而症状要到监控中断时才出现。
		"no port": {Namespace: "shop", Name: "portless", ScrapeAnnotations: map[string]string{
			"prometheus.io/scrape": "true",
		}},
		// 端口不是数字：同上，不猜。
		"bad port": {Namespace: "shop", Name: "broken", ScrapeAnnotations: map[string]string{
			"prometheus.io/scrape": "true", "prometheus.io/port": "metrics",
		}},
		// 端口越界。
		"port out of range": {Namespace: "shop", Name: "huge", ScrapeAnnotations: map[string]string{
			"prometheus.io/scrape": "true", "prometheus.io/port": "70000",
		}},
	}
	for name, pod := range cases {
		t.Run(name, func(t *testing.T) {
			if got := c.ScrapeTargetSnapshots([]snapshot.Pod{pod}); len(got) != 0 {
				t.Errorf("ScrapeTargetSnapshots() = %+v, want none", got)
			}
		})
	}
}

func TestScrapeTargetsAreEmptyWithoutARegisteredScraper(t *testing.T) {
	// **没有登记抓取端就不生成任何东西**，METRICS_SCRAPE 照旧进缺失清单。
	// 编一条占位规则会让齐备性校验通过，而真正的抓取仍被挡。
	c := registry.Cluster{ID: "c"}
	if got := c.ScrapeTargetSnapshots([]snapshot.Pod{scrapablePod("shop", "web-1", "9102")}); len(got) != 0 {
		t.Errorf("ScrapeTargetSnapshots() = %+v, want none without a registered scraper", got)
	}
}
