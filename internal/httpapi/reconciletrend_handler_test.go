package httpapi_test

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/httpapi"
	"github.com/imkerbos/Distill/internal/reconcile"
	"github.com/imkerbos/Distill/internal/snapshotstore"
)

const trendPath = "/api/v1/clusters/prod-asia-1/reconciliation/trend"

// trendFixture 是趋势用例的装配：一个登记了集群的注册表，加一个趋势读取端。
type trendFixture struct {
	h      http.Handler
	cookie *http.Cookie
}

// newTrendFixture 装一个只带对账历史读取端的路由器。
//
// trend 为 nil 时装配的是"本部署不记录对账历史"那一形态 —— 那是部署上真实
// 存在的一种（没有采集链路的实例），也是一条必须被测到的分支。
func newTrendFixture(t *testing.T, trend httpapi.ReconciliationTrendReader) trendFixture {
	t.Helper()
	reg := newMemRegistry()
	for _, c := range fixtureClusters() {
		reg.clusters[c.ID] = c
	}
	// 接口值里裹着一个 nil 指针与接口本身为 nil 是两回事，而 handler 判的是
	// 后者：显式传 nil 接口，才真的走到"没接通"那条分支。
	var dep httpapi.ReconciliationTrendReader
	if trend != nil {
		dep = trend
	}
	h, _, cookie := buildTestRouterWithTrend(t, reg, dep)
	return trendFixture{h: h, cookie: cookie}
}

// fetchTrend 取一次趋势，要求成功，返回点的原始 JSON。
//
// 断言打在**真的传给前端的那份 JSON** 上，不是被测类型自己：拿被测类型去解
// 自己的输出，一个字段被改成不序列化也照样绿（同 planView 的理由）。
func fetchTrend(t *testing.T, f trendFixture) []map[string]any {
	t.Helper()
	rec := authedGet(t, f.h, f.cookie, trendPath)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := bodyOf(t, rec)
	if body["code"] != float64(0) {
		t.Fatalf("code = %v, want 0: %s", body["code"], rec.Body.String())
	}
	data, _ := body["data"].(map[string]any)
	raw, _ := data["points"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, p := range raw {
		m, _ := p.(map[string]any)
		out = append(out, m)
	}
	return out
}

// readSource 读一份源码，供"两处常量必须一致"那类断言使用。
func readSource(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // G304: 测试内的固定相对路径
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// containsAll 报告 haystack 是否含全部片段。
func containsAll(haystack string, needles []string) bool {
	for _, n := range needles {
		if !strings.Contains(haystack, n) {
			return false
		}
	}
	return true
}

// stubTrend 是趋势端点要的那一点点历史。
type stubTrend struct {
	runs []snapshotstore.ReconciliationRun
	// askedLimit 记下 handler 要了多少个点，用来守"上限两处一致"。
	askedLimit int
	err        error
}

func (s *stubTrend) ReconciliationTrend(
	_ context.Context, _ string, limit int,
) ([]snapshotstore.ReconciliationRun, error) {
	s.askedLimit = limit
	return s.runs, s.err
}

func trendRun(at time.Time, agree, under, over, unknown int, reports bool) snapshotstore.ReconciliationRun {
	return snapshotstore.ReconciliationRun{
		ClusterID: "prod-asia-1", RunID: "r",
		WindowFrom: at, WindowTo: at.Add(time.Minute), ComputedAt: at,
		SourceReports: reports,
		Report: reconcile.Report{Overall: reconcile.Counts{
			reconcile.ClassAgree:           agree,
			reconcile.ClassUnderPermissive: under,
			reconcile.ClassOverPermissive:  over,
			reconcile.ClassPlatformUnknown: unknown,
		}},
	}
}

// **算不出一致率的窗口必须是 null，不是 0。**
//
// 把"算不出"画成 0，趋势图上就会出现一个触底的点，读起来是"那天全错了"，
// 而事实是那天没有可比对的连接。一条会说谎的曲线比没有曲线更糟。
func TestTrendReportsAnUncomputableRateAsNullNotZero(t *testing.T) {
	at := time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC)
	trend := &stubTrend{runs: []snapshotstore.ReconciliationRun{
		// 来源不报判定：这条接入方式对不了账。
		trendRun(at, 0, 0, 0, 0, false),
		// 报了判定，但这个窗口一条可比对的都没有（全是平台答不出）。
		trendRun(at.Add(-time.Hour), 0, 0, 0, 7, true),
		// 正常的一轮。
		trendRun(at.Add(-2*time.Hour), 9, 1, 0, 3, true),
	}}
	f := newTrendFixture(t, trend)

	points := fetchTrend(t, f)
	if len(points) != 3 {
		t.Fatalf("拿到 %d 个点, want 3", len(points))
	}
	for i, p := range points[:2] {
		if p["rate"] != nil {
			t.Errorf("第 %d 个点的 rate = %v，算不出的窗口必须是 null —— "+
				"0 在图上是一个触底的点，读起来是「那天全错了」", i, p["rate"])
		}
	}
	if points[2]["rate"] == nil {
		t.Fatal("第 3 个点有 9 条一致、1 条分歧，一致率算得出来")
	}
	if got := points[2]["rate"].(float64); got != 0.9 {
		t.Errorf("rate = %v, want 0.9", got)
	}
	// 分母必须跟着走：基于 3 条的 100% 与基于 3 万条的 100% 在图上是同一个
	// 点，含义差着数量级。
	if got := points[2]["comparable"].(float64); got != 10 {
		t.Errorf("comparable = %v, want 10", got)
	}
	// 平台答不出**不是分歧**，不能混进分母，但要单独报出来。
	if got := points[2]["platformUnknown"].(float64); got != 3 {
		t.Errorf("platformUnknown = %v, want 3", got)
	}
}

// rate 为 null 时，要能区分"来源不报判定"与"报了但没有可比对的连接"。
//
// 两者的处置完全不同：前者要换流量来源，后者等下一轮就好。
func TestTrendDistinguishesWhyARateIsMissing(t *testing.T) {
	at := time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC)
	f := newTrendFixture(t, &stubTrend{runs: []snapshotstore.ReconciliationRun{
		trendRun(at, 0, 0, 0, 0, false),
		trendRun(at.Add(-time.Hour), 0, 0, 0, 7, true),
	}})

	points := fetchTrend(t, f)
	if points[0]["sourceReports"] != false || points[1]["sourceReports"] != true {
		t.Errorf("sourceReports 没能区分两种「算不出」：%v", points)
	}
}

// 上限两处一致：handler 要的点数必须就是存储层肯给的点数。
//
// 不一致时，一个「要 200 个点」的请求会拿到 50 个，而界面上那条线看起来
// 只是短一点，没有任何迹象说明它被截断了。
func TestTrendAsksForTheSameLimitTheStoreHonours(t *testing.T) {
	trend := &stubTrend{}
	f := newTrendFixture(t, trend)
	fetchTrend(t, f)

	if trend.askedLimit != httpapi.MaxTrendPointsForTest {
		t.Fatalf("handler 要了 %d 个点，与自己声明的上限 %d 不一致",
			trend.askedLimit, httpapi.MaxTrendPointsForTest)
	}
	store := readSource(t, "../snapshotstore/reconciliation.go")
	if !containsAll(store, []string{"limit > 200"}) {
		t.Errorf("存储层的上限变了，而 handler 那个常量没跟上：" +
			"一个要满量的请求会拿到被截断的一段，且界面上看不出来")
	}
}

// 本部署不记录对账历史时，答"这条路没接通"，不答空数组。
//
// 空数组读起来是"这个集群还没对过账"，会让操作者去等一份永远不会出现的趋势。
func TestTrendWithoutAStoreIsNotAnEmptyTrend(t *testing.T) {
	f := newTrendFixture(t, nil)
	rec := authedGet(t, f.h, f.cookie, trendPath)
	body := bodyOf(t, rec)
	if body["code"] == float64(0) {
		t.Fatalf("没有对账历史读取端，却回了一份趋势：%s", rec.Body.String())
	}
	if body["code"] != float64(20007) {
		t.Errorf("code = %v, want 20007（这条读路径没接通）", body["code"])
	}
}
