package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/fixture"
	"github.com/imkerbos/Distill/internal/response"
	"github.com/imkerbos/Distill/internal/store"
)

// flowListEnvelope 是流量列表的响应形状：分页信息与列表本身同级，
// 调用方据此知道自己拿到的是不是全部。
type flowListEnvelope struct {
	Code int `json:"code"`
	Data struct {
		Items    []store.FlowRecord `json:"items"`
		Total    int                `json:"total"`
		Returned int                `json:"returned"`
		Limit    int                `json:"limit"`
		Window   store.TimeWindow   `json:"window"`
	} `json:"data"`
}

// fixtureDataWindow 给出覆盖 fixture 全量数据的时间窗。
// fixtureReader 返回接口类型，而数据窗口只存在于 fixture 实现上。
func fixtureDataWindow() store.TimeWindow {
	return store.NewFixtureReader(fixture.Load(), fixtureSource()).DataWindow()
}

func decodeFlowList(t *testing.T, body []byte) flowListEnvelope {
	t.Helper()
	var env flowListEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode: %v (%q)", err, string(body))
	}
	return env
}

func TestFlowsEndpoint(t *testing.T) {
	h, _, cookie := newTestRouter(t, fixtureReader())
	rec := authedGet(t, h, cookie, "/api/v1/flows?limit=5")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	env := decodeFlowList(t, rec.Body.Bytes())
	if len(env.Data.Items) != 5 {
		t.Errorf("got %d flows, want 5", len(env.Data.Items))
	}
	for _, f := range env.Data.Items {
		if f.Verdict == "" || f.Confidence == "" {
			t.Errorf("flow %s missing verdict or confidence", f.ID)
		}
	}
}

// 被截断的列表必须自报总数，否则界面会把"只给你看了 5 条"说成"一共就这些"。
func TestFlowsResponseReportsTruncation(t *testing.T) {
	h, _, cookie := newTestRouter(t, fixtureReader())
	env := decodeFlowList(t, authedGet(t, h, cookie, "/api/v1/flows?limit=5").Body.Bytes())

	if env.Data.Total <= env.Data.Returned {
		t.Errorf("total = %d, returned = %d — truncation is invisible to the frontend",
			env.Data.Total, env.Data.Returned)
	}
	if env.Data.Returned != len(env.Data.Items) {
		t.Errorf("returned = %d but %d items present", env.Data.Returned, len(env.Data.Items))
	}
	if env.Data.Limit != 5 {
		t.Errorf("limit = %d, want 5", env.Data.Limit)
	}
}

// 默认视图（不带 limit）必须带上刻意制造的敞口，否则 demo 一开屏就是全绿。
func TestFlowsDefaultViewIncludesTheGaps(t *testing.T) {
	h, _, cookie := newTestRouter(t, fixtureReader())
	env := decodeFlowList(t, authedGet(t, h, cookie, "/api/v1/flows").Body.Bytes())

	if env.Data.Returned != env.Data.Total {
		t.Errorf("default view returned %d of %d flows; the dataset fits in one page",
			env.Data.Returned, env.Data.Total)
	}
	var unknown, cross int
	for _, f := range env.Data.Items {
		if f.Verdict == "UNKNOWN" {
			unknown++
		}
		if f.CrossCluster {
			cross++
		}
	}
	if unknown == 0 || cross == 0 {
		t.Errorf("default view has %d UNKNOWN and %d cross-cluster flows; both must be visible",
			unknown, cross)
	}
}

func TestFlowsFilterByVerdict(t *testing.T) {
	h, _, cookie := newTestRouter(t, fixtureReader())
	env := decodeFlowList(t, authedGet(t, h, cookie, "/api/v1/flows?verdict=DENY&limit=200").Body.Bytes())

	if len(env.Data.Items) == 0 {
		t.Fatal("no DENY flows returned")
	}
	for _, f := range env.Data.Items {
		if f.Verdict != "DENY" {
			t.Fatalf("filter leaked a %s flow", f.Verdict)
		}
	}
}

// limit 不是数字时是参数问题，返回 20001。
func TestFlowsRejectsBadLimit(t *testing.T) {
	h, _, cookie := newTestRouter(t, fixtureReader())
	rec := authedGet(t, h, cookie, "/api/v1/flows?limit=many")

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 — a bad query value is a business-level failure", rec.Code)
	}
	if got := bodyOf(t, rec)["code"]; got != float64(20001) {
		t.Errorf("code = %v, want 20001", got)
	}
}

// 拼错的取值必须报 20001，不能返回空列表：一次输入错误不该被界面读成
// "这个集群没有这类流量"。不存在的集群则与拓扑、质量端点一样报 20002。
func TestFlowsRejectsUnknownFilterValues(t *testing.T) {
	h, _, cookie := newTestRouter(t, fixtureReader())

	for _, tc := range []struct {
		query string
		code  float64
	}{
		{"verdict=ALOW", 20001},
		{"verdict=allow", 20001},
		{"confidence=TRUSTD", 20001},
		{"confidence=trusted", 20001},
		{"cluster=no-such", 20002},
	} {
		rec := authedGet(t, h, cookie, "/api/v1/flows?"+tc.query)
		if rec.Code != http.StatusOK {
			t.Errorf("%s status = %d, want 200", tc.query, rec.Code)
		}
		body := bodyOf(t, rec)
		if body["code"] != tc.code {
			t.Errorf("%s code = %v, want %v", tc.query, body["code"], tc.code)
		}
		if body["data"] != nil {
			t.Errorf("%s returned data on a failure", tc.query)
		}
	}
}

// 空取值仍然表示"不筛选"，不能被当成非法输入。
func TestFlowsEmptyFilterValuesMeanNoFilter(t *testing.T) {
	h, _, cookie := newTestRouter(t, fixtureReader())
	rec := authedGet(t, h, cookie, "/api/v1/flows?verdict=&confidence=&cluster=")

	env := decodeFlowList(t, rec.Body.Bytes())
	if env.Code != 0 {
		t.Fatalf("code = %d, want 0", env.Code)
	}
	if len(env.Data.Items) == 0 {
		t.Error("empty filter values returned no flows")
	}
}

// NetworkPolicy 管不到的流量必须自己说出来，不能混在普通 ALLOW 里。
func TestFlowsMarkUnmanagedTraffic(t *testing.T) {
	h, _, cookie := newTestRouter(t, fixtureReader())
	env := decodeFlowList(t, authedGet(t, h, cookie, "/api/v1/flows?limit=1000").Body.Bytes())

	for _, f := range env.Data.Items {
		if f.Unmanaged {
			return
		}
	}
	t.Error("no flow is marked unmanaged; traffic to the hostNetwork pod renders as a plain ALLOW")
}

func TestFlowDecisionEndpoint(t *testing.T) {
	h, _, cookie := newTestRouter(t, fixtureReader())

	list := decodeFlowList(t, authedGet(t, h, cookie, "/api/v1/flows?limit=1").Body.Bytes())
	if len(list.Data.Items) == 0 {
		t.Fatal("no flows to inspect")
	}

	rec := authedGet(t, h, cookie, "/api/v1/flows/"+list.Data.Items[0].ID+"/decision")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var env struct {
		Code int            `json:"code"`
		Data store.Decision `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Data.ID != list.Data.Items[0].ID {
		t.Errorf("decision is for flow %q, want %q", env.Data.ID, list.Data.Items[0].ID)
	}
}

func TestFlowDecisionUnknownID(t *testing.T) {
	h, _, cookie := newTestRouter(t, fixtureReader())
	rec := authedGet(t, h, cookie, "/api/v1/flows/flow-999999/decision")

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if got := bodyOf(t, rec)["code"]; got != float64(20002) {
		t.Errorf("code = %v, want 20002", got)
	}
}

func TestFlowsRequiresAuth(t *testing.T) {
	h, _, _ := newTestRouter(t, fixtureReader())
	rec := authedGet(t, h, &http.Cookie{Name: "distill_session", Value: "bogus"}, "/api/v1/flows") //nolint:gosec // G124: request cookie, not a response header

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// 省略 from/to 时用装配方注入的默认窗口，且必须回显 —— 界面要能说清
// 自己展示的是哪一段时间，否则按时间筛过的列表与全量列表无法区分。
func TestFlowsEchoesDefaultWindow(t *testing.T) {
	h, _, cookie := newTestRouter(t, fixtureReader())
	rec := authedGet(t, h, cookie, "/api/v1/flows?limit=5")

	env := decodeFlowList(t, rec.Body.Bytes())
	if env.Data.Window.From.IsZero() || env.Data.Window.To.IsZero() {
		t.Fatalf("默认窗口未回显: %+v", env.Data.Window)
	}
}

func TestFlowsFiltersByExplicitWindow(t *testing.T) {
	h, _, cookie := newTestRouter(t, fixtureReader())
	full := fixtureDataWindow()

	from := full.From.Format(time.RFC3339)
	to := full.From.Add(10 * time.Second).Format(time.RFC3339)
	rec := authedGet(t, h, cookie, "/api/v1/flows?from="+url.QueryEscape(from)+"&to="+url.QueryEscape(to))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	env := decodeFlowList(t, rec.Body.Bytes())
	if env.Data.Total != 10 {
		t.Errorf("十秒窗口 total = %d, want 10", env.Data.Total)
	}
	if !env.Data.Window.From.Equal(full.From) {
		t.Errorf("回显窗口起点 = %v, want %v", env.Data.Window.From, full.From)
	}
}

// 只给一端必须报错。补另一端要么取"现在"、要么取"开天辟地"，两个默认
// 都会让实际范围与用户以为筛的范围不一致，而界面只会照实回显那个它
// 并没有要求过的窗口。
func TestFlowsRejectsHalfWindow(t *testing.T) {
	h, _, cookie := newTestRouter(t, fixtureReader())
	full := fixtureDataWindow()
	from := url.QueryEscape(full.From.Format(time.RFC3339))

	for _, q := range []string{"?from=" + from, "?to=" + from} {
		rec := authedGet(t, h, cookie, "/api/v1/flows"+q)
		env := decodeFlowList(t, rec.Body.Bytes())
		if env.Code != int(response.CodeInvalidParam) {
			t.Errorf("%s: code = %d, want %d", q, env.Code, int(response.CodeInvalidParam))
		}
	}
}

func TestFlowsRejectsMalformedWindow(t *testing.T) {
	h, _, cookie := newTestRouter(t, fixtureReader())
	rec := authedGet(t, h, cookie, "/api/v1/flows?from=not-a-time&to=also-not")

	env := decodeFlowList(t, rec.Body.Bytes())
	if env.Code != int(response.CodeInvalidParam) {
		t.Errorf("code = %d, want %d", env.Code, int(response.CodeInvalidParam))
	}
}

// From 晚于 To 是一个必然返回空列表的窗口。放行它等于让界面把一次
// 输入错误显示成"这段时间没有流量"。
func TestFlowsRejectsInvertedWindow(t *testing.T) {
	h, _, cookie := newTestRouter(t, fixtureReader())
	full := fixtureDataWindow()
	from := url.QueryEscape(full.To.Format(time.RFC3339))
	to := url.QueryEscape(full.From.Format(time.RFC3339))

	rec := authedGet(t, h, cookie, "/api/v1/flows?from="+from+"&to="+to)
	env := decodeFlowList(t, rec.Body.Bytes())
	if env.Code != int(response.CodeInvalidParam) {
		t.Errorf("code = %d, want %d", env.Code, int(response.CodeInvalidParam))
	}
}

// clusterRequiredReader 让不点名集群的流量列表以 store.ErrClusterRequired 结束，
// 与装配层的分派器一致（cmd/distill-api/dispatch.go）。
//
// 包一层 fmt.Errorf 而不是直接返回哨兵：一个只认裸哨兵的映射在真实调用路径上
// 会失效，而失效之后的症状恰好是它本来要消除的那个 500 —— 与 noCollectionReader
// 同一处置。
type clusterRequiredReader struct{ brokenReader }

func (clusterRequiredReader) Flows(_ context.Context, f store.FlowFilter) (store.FlowPage, error) {
	if f.Cluster == "" {
		return store.FlowPage{}, fmt.Errorf("%w: dispatcher refused", store.ErrClusterRequired)
	}
	return store.FlowPage{Window: f.Window}, nil
}

// DefaultWindow 同样在不点名集群时拒绝，与分派器一致：默认窗口是**按集群**
// 推出来的结论，没有集群就没有这个结论（design doc 2026-08-18 §3.1）。
//
// 拒绝在这里先发生 —— /flows 现在要先问出这个集群的默认窗口才谈得上查询 ——
// 所以这个替身若在这里放行，测的就不再是那条拒绝了。
func (clusterRequiredReader) DefaultWindow(
	_ context.Context, clusterID string,
) (store.TimeWindow, error) {
	if clusterID == "" {
		return store.TimeWindow{}, fmt.Errorf("%w: dispatcher refused", store.ErrClusterRequired)
	}
	return fixtureDataWindow(), nil
}

// 不点名集群的流量列表要给出一个**说得出原因**的响应，不是 500。
//
// 这是 design doc 2026-08-18 §3.2 那条拒绝在边界层的落点：拒绝本身发生在
// 装配层，而它只有被 writeReaderError 认出来才到得了操作者眼前。少了这条
// 映射，界面显示的是"服务内部错误"，于是操作者去查一个完全健康的服务，
// 而该做的只是选一个集群。
//
// 三件事一起断言：HTTP 200（用法问题不计进服务错误率）、码 20006、
// 文案不是内部错误那一句（塌回 500 文案的症状）。
func TestFlowsWithoutAClusterExplainsItselfInsteadOf500(t *testing.T) {
	h, _, cookie := newTestRouter(t, clusterRequiredReader{})
	rec := authedGet(t, h, cookie, "/api/v1/flows")

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 —— 少一个筛选条件不是服务故障", rec.Code)
	}
	body := bodyOf(t, rec)
	if body["code"] != float64(response.CodeClusterRequired) {
		t.Errorf("code = %v, want %d（请先选择一个集群）；50001 会把操作者支去查一个健康的服务",
			body["code"], int(response.CodeClusterRequired))
	}
	if msg, _ := body["msg"].(string); msg == response.CodeInternal.Message() {
		t.Errorf("msg = %q，与内部错误同一句话，说明这条路径塌回了 500 的文案", msg)
	}
	if body["data"] != nil {
		t.Errorf("data = %v, want null", body["data"])
	}

	// 对照组：点名集群之后照常作答。少了这一条，一个对所有 /flows 都回
	// 20006 的实现也能让上面通过 —— 而那是把流量页整个关掉。
	named := authedGet(t, h, cookie, "/api/v1/flows?cluster=prod-asia-1")
	if got := bodyOf(t, named)["code"]; got != float64(response.CodeOK) {
		t.Errorf("cluster named: code = %v, want 0", got)
	}
}

func (clusterRequiredReader) DefaultWindowAt(
	ctx context.Context, clusterID string, _ time.Time,
) (store.TimeWindow, error) {
	return clusterRequiredReader{}.DefaultWindow(ctx, clusterID)
}
