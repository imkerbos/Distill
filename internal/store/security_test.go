package store_test

import (
	"context"
	"testing"

	"github.com/imkerbos/Distill/internal/fixture"
	"github.com/imkerbos/Distill/internal/store"
)

func newSecurityReport(t *testing.T, cluster string) store.SecurityReport {
	t.Helper()
	r := store.NewFixtureReader(fixture.Load())
	rep, err := r.Security(context.Background(), cluster, r.DataWindow())
	if err != nil {
		t.Fatalf("Security(%s): %v", cluster, err)
	}
	return rep
}

// 同一个端口在不同位置风险不同，报告必须能分开表达。合成一个分数后，
// 使用者无法判断该去找谁 —— 出公网的 SSH 要找网络，跨 namespace 的
// 数据库直连要找应用团队。
func TestSecurityReportSeparatesCategoryFromPosition(t *testing.T) {
	rep := newSecurityReport(t, "prod-asia-1")

	byPosition := map[store.RiskPosition]int{}
	byCategory := map[store.RiskCategory]int{}
	for _, f := range rep.RiskyFlows {
		byPosition[f.Position]++
		byCategory[f.Category]++
	}

	if byPosition[store.PositionEgressInternet] == 0 {
		t.Error("没有出公网的风险连接，报告失去最高危的一类")
	}
	if byPosition[store.PositionCrossNamespace] == 0 {
		t.Error("没有跨 namespace 的风险连接")
	}
	if byCategory[store.RiskAdminPlaintext] == 0 {
		t.Error("没有明文管理端口的风险连接")
	}
	if byCategory[store.RiskDatabase] == 0 {
		t.Error("没有数据库直连的风险连接")
	}
}

// 被策略挡住的高风险连接仍然要出现在报告里：DENY 说明这次没通，
// 不说明没有人在尝试。把它们过滤掉等于把"有人在连数据库"这条信号丢了。
func TestSecurityReportKeepsDeniedRiskyFlows(t *testing.T) {
	rep := newSecurityReport(t, "prod-asia-1")

	var denied int
	for _, f := range rep.RiskyFlows {
		if f.Verdict == "DENY" {
			denied++
		}
	}
	if denied == 0 {
		t.Error("报告里没有被 DENY 的风险连接 —— 它们不应被过滤掉")
	}
}

// 正常端口不得进报告。为了让页面有内容而给 Prometheus 端口贴风险标签，
// 与伪造判定没有区别。
func TestSecurityReportExcludesBenignPorts(t *testing.T) {
	rep := newSecurityReport(t, "prod-asia-1")
	for _, f := range rep.RiskyFlows {
		switch f.Port {
		case 9090, 9100, 8080, 443, 8443:
			t.Errorf("端口 %d 被判为高风险，但它是正常业务或监控端口", f.Port)
		}
	}
	for _, p := range rep.RiskPortCatalog {
		if p.Port == 9090 || p.Port == 9100 {
			t.Errorf("端口清单里不应包含 %d", p.Port)
		}
	}
}

// 清单必须随报告返回：报告为空时，使用者要能看到"我们查了哪些端口"，
// 否则一份空报告与一次根本没做的检查在界面上无法区分。
func TestSecurityReportAlwaysCarriesPortCatalog(t *testing.T) {
	// eu 集群没有任何风险流量，正是需要清单的场景。
	rep := newSecurityReport(t, "prod-eu-1")
	if len(rep.RiskyFlows) != 0 {
		t.Logf("prod-eu-1 有 %d 条风险流量", len(rep.RiskyFlows))
	}
	if len(rep.RiskPortCatalog) == 0 {
		t.Fatal("端口清单为空 —— 空报告将无法与未检查区分")
	}
}

// 出公网目标必须分列放行与未知条数：一条畅通的外联与一条已被挡住的
// 外联，只报总数会长得一样。
func TestEgressTargetsSeparateAllowedFromTotal(t *testing.T) {
	rep := newSecurityReport(t, "prod-asia-1")
	if len(rep.EgressTargets) == 0 {
		t.Fatal("没有公网出向目标")
	}
	var totalFlows, totalAllowed int
	for _, tg := range rep.EgressTargets {
		if tg.FlowCount == 0 {
			t.Errorf("目标 %s 的条数为 0", tg.Address)
		}
		if len(tg.Ports) == 0 {
			t.Errorf("目标 %s 没有端口", tg.Address)
		}
		if tg.AllowedCount > tg.FlowCount {
			t.Errorf("目标 %s: 放行 %d 条 > 总数 %d 条", tg.Address, tg.AllowedCount, tg.FlowCount)
		}
		totalFlows += tg.FlowCount
		totalAllowed += tg.AllowedCount
	}
	if totalAllowed == 0 {
		t.Error("没有任何被放行的出公网流量，出向清单失去意义")
	}
	t.Logf("公网出向 %d 条，其中放行 %d 条", totalFlows, totalAllowed)
}

// 裸奔 Pod 清单不得混入 hostNetwork Pod：前者是"没被策略选中"，
// 后者是"NetworkPolicy 根本管不到"，处置手段完全不同。
func TestNakedPodsExcludeHostNetwork(t *testing.T) {
	f := fixture.Load()
	rep := newSecurityReport(t, "prod-asia-1")
	if len(rep.NakedPods) == 0 {
		t.Fatal("没有裸奔 Pod，清单失去意义")
	}

	host := map[string]bool{}
	for _, c := range f.Clusters {
		for _, p := range c.Pods {
			if p.HostNetwork {
				host[p.Namespace+"/"+p.Name] = true
			}
		}
	}
	for _, np := range rep.NakedPods {
		if host[np.Namespace+"/"+np.Name] {
			t.Errorf("hostNetwork Pod %s/%s 出现在裸奔清单里", np.Namespace, np.Name)
		}
	}
}

func TestSecurityRejectsMissingWindow(t *testing.T) {
	r := store.NewFixtureReader(fixture.Load())
	if _, err := r.Security(context.Background(), "prod-asia-1", store.TimeWindow{}); err == nil {
		t.Fatal("缺时间窗时应返回错误")
	}
}

func TestSecurityUnknownClusterIsNotFound(t *testing.T) {
	r := store.NewFixtureReader(fixture.Load())
	if _, err := r.Security(context.Background(), "no-such", r.DataWindow()); err == nil {
		t.Fatal("集群不存在时应返回错误")
	}
}
