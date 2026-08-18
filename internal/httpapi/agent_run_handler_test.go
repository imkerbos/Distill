package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/cluster"
	"github.com/imkerbos/Distill/internal/response"
	"github.com/imkerbos/Distill/internal/snapshot"
	"github.com/imkerbos/Distill/internal/snapshotstore"
)

// errStorage 是一次落库故障，与业务失败区分开。
var errStorage = errors.New("storage is down")

// recordingSink 记下平台收下并准备落库的每一次运行。
type recordingSink struct {
	runs    []snapshot.Run
	aborted []abortedPush
	err     error
}

func (s *recordingSink) Save(_ context.Context, run snapshot.Run) error {
	s.runs = append(s.runs, run)
	return s.err
}

// abortedPush 是一次中止运行的落库参数。
type abortedPush struct {
	clusterID string
	runID     string
	reason    snapshot.RunErrorReason
}

func (s *recordingSink) SaveAbortedRun(
	_ context.Context, clusterID, runID string,
	_, _ time.Time, reason snapshot.RunErrorReason,
) error {
	s.aborted = append(s.aborted, abortedPush{clusterID: clusterID, runID: runID, reason: reason})
	return s.err
}

func (s *recordingSink) only(t *testing.T) snapshot.Run {
	t.Helper()
	if len(s.runs) != 1 {
		t.Fatalf("sink received %d runs, want exactly 1", len(s.runs))
	}
	return s.runs[0]
}

// postRun 用 agent token 推一次采集结果。
func postRun(t *testing.T, h http.Handler, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/collection-runs", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// runBody 是一份形状正确的上报，caller 可以往里塞额外字段。
func runBody(extra string) string {
	return `{"schemaVersion":1,"runId":"r-1","status":"OK",` + extra + `
	  "startedAt":"2026-08-18T00:00:00Z","finishedAt":"2026-08-18T00:00:05Z",
	  "observedAt":"2026-08-18T00:00:05Z",
	  "observation":{"pods":[{"namespace":"app","name":"web-1","ip":"10.128.0.5","uid":"u-1",
	     "phase":"Running","hostNetwork":true}]}}`
}

func TestIngestTakesTheClusterFromTheTokenNotTheBody(t *testing.T) {
	sink := &recordingSink{}
	h, _, cookie := newTestRouterWithAgentSink(t, sink)
	_, token := issueAgent(t, h, cookie, "prod-asia-1")

	rec := postRun(t, h, token, runBody(``))
	if got := bodyOf(t, rec)["code"]; got != float64(response.CodeOK) {
		t.Fatalf("code = %v, want 0: %s", got, rec.Body.String())
	}
	run := sink.only(t)
	if run.Observation.ClusterID != "prod-asia-1" {
		t.Errorf("stored ClusterID = %q, want prod-asia-1 — 归属没有取自 token",
			run.Observation.ClusterID)
	}
}

func TestIngestRejectsAPayloadThatNamesItsOwnCluster(t *testing.T) {
	sink := &recordingSink{}
	h, _, cookie := newTestRouterWithAgentSink(t, sink)
	_, token := issueAgent(t, h, cookie, "prod-asia-1")

	// **拒绝整个请求，不是忽略那个字段**（design doc 2026-08-18 §2）。
	// 忽略会让一个装错 token 的 agent 静默地把数据写进别的集群，而它自己
	// 以为写对了 —— 之后的 join 会落到错误的 Pod 上且不报错（CLAUDE.md §4）。
	rec := postRun(t, h, token, runBody(`"clusterId":"prod-eu-1",`))
	if got := bodyOf(t, rec)["code"]; got != float64(response.CodeInvalidParam) {
		t.Errorf("code = %v, want %d: %s", got, response.CodeInvalidParam, rec.Body.String())
	}
	if len(sink.runs) != 0 {
		t.Errorf("the sink received %d runs, want none — 被拒的报文写进去了", len(sink.runs))
	}
}

func TestIngestRejectsAnyUnknownField(t *testing.T) {
	sink := &recordingSink{}
	h, _, cookie := newTestRouterWithAgentSink(t, sink)
	_, token := issueAgent(t, h, cookie, "prod-asia-1")

	// 契约版本对得上、却带着平台不认识的字段，说明两边对同一个版本的
	// 理解不一致 —— 那比版本号对不上更危险，因为它不会被版本检查拦住。
	rec := postRun(t, h, token, runBody(`"somethingNew":1,`))
	if got := bodyOf(t, rec)["code"]; got != float64(response.CodeInvalidParam) {
		t.Errorf("code = %v, want %d", got, response.CodeInvalidParam)
	}
}

func TestIngestRejectsAnUnsupportedSchemaVersion(t *testing.T) {
	sink := &recordingSink{}
	h, _, cookie := newTestRouterWithAgentSink(t, sink)
	_, token := issueAgent(t, h, cookie, "prod-asia-1")

	// agent 跑在别人的集群里，升级节奏不受平台控制。不认识就拒绝，
	// 不做尽力而为的解析：字段错位会变成一批静默的错误数据，而错误数据
	// 在这个平台上会一路走到策略推荐。
	body := strings.Replace(runBody(``), `"schemaVersion":1`, `"schemaVersion":99`, 1)
	rec := postRun(t, h, token, body)
	if got := bodyOf(t, rec)["code"]; got != float64(response.CodeInvalidParam) {
		t.Errorf("code = %v, want %d", got, response.CodeInvalidParam)
	}
	if len(sink.runs) != 0 {
		t.Errorf("the sink received %d runs, want none", len(sink.runs))
	}
}

func TestIngestRejectsARunWithoutAnID(t *testing.T) {
	sink := &recordingSink{}
	h, _, cookie := newTestRouterWithAgentSink(t, sink)
	_, token := issueAgent(t, h, cookie, "prod-asia-1")

	// run_id 是幂等键。放行一个空的，一次网络抖动重试就会变成第二份历史
	// 记录 —— 同一次采集在库里出现两遍，而「那时候是什么样」从此有两个
	// 答案（CLAUDE.md §4：禁止用当前状态解释历史数据）。
	body := strings.Replace(runBody(``), `"runId":"r-1"`, `"runId":""`, 1)
	rec := postRun(t, h, token, body)
	if got := bodyOf(t, rec)["code"]; got != float64(response.CodeInvalidParam) {
		t.Errorf("code = %v, want %d: %s", got, response.CodeInvalidParam, rec.Body.String())
	}
	if len(sink.runs) != 0 {
		t.Errorf("the sink received %d runs, want none", len(sink.runs))
	}
}

func TestIngestRejectsAnUnregisteredRunStatus(t *testing.T) {
	sink := &recordingSink{}
	h, _, cookie := newTestRouterWithAgentSink(t, sink)
	_, token := issueAgent(t, h, cookie, "prod-asia-1")

	// status 在库里只是一列 VARCHAR，封闭性由 Go 侧保证（CLAUDE.md §3），
	// 而这个取值来自别人集群里的进程。放进去一个不认识的字符串，可见面
	// 会渲染出一个没人定义过的状态。
	body := strings.Replace(runBody(``), `"status":"OK"`, `"status":"SORTOF"`, 1)
	rec := postRun(t, h, token, body)
	if got := bodyOf(t, rec)["code"]; got != float64(response.CodeInvalidParam) {
		t.Errorf("code = %v, want %d", got, response.CodeInvalidParam)
	}
	if len(sink.runs) != 0 {
		t.Errorf("the sink received %d runs, want none", len(sink.runs))
	}
}

func TestIngestClassifiesOnTheServerSide(t *testing.T) {
	sink := &recordingSink{}
	h, _, cookie := newTestRouterWithAgentSink(t, sink)
	_, token := issueAgent(t, h, cookie, "prod-asia-1")

	// agent 上报的 Pod 不带归属：那是平台的判定，要看全 fleet 的网段
	// （design doc §3.4）。
	if got := bodyOf(t, postRun(t, h, token, runBody(``)))["code"]; got != float64(response.CodeOK) {
		t.Fatalf("code = %v, want 0", got)
	}
	// 取一个**只属于 prod-asia-1** 的地址（它的节点网段 10.128.0.0/20）：
	// 两个 fixture 集群的 Pod 网段是同一段，用 Pod 地址会得到 AMBIGUOUS ——
	// 那也是正确答案，但它区分不出「平台判过」与「平台没判」。
	pod := sink.only(t).Observation.Pods[0]
	if pod.IPScope != cluster.ScopeNode {
		t.Errorf("IPScope = %q, want %q — 平台没有在收下时分类", pod.IPScope, cluster.ScopeNode)
	}
}

func TestIngestRefusesAnAgentThatClaimsAScope(t *testing.T) {
	sink := &recordingSink{}
	h, _, cookie := newTestRouterWithAgentSink(t, sink)
	_, token := issueAgent(t, h, cookie, "prod-asia-1")

	// 这份观测来自被管集群里的 agent。相信它自己声明的归属，等于把
	// 「这条流量属于哪个集群」外包给被判断的一方（design doc §3.4）。
	//
	// 报文结构里**根本没有 ipScope 这个字段**，于是它连声称一个归属的语法
	// 都没有 —— 这比「收下再覆盖」更强：后者依赖覆盖那一步不被谁删掉，
	// 而这里少写一行就是整个请求被拒。
	body := strings.Replace(runBody(``),
		`"phase":"Running"`, `"phase":"Running","ipScope":"EXTERNAL"`, 1)
	rec := postRun(t, h, token, body)
	if got := bodyOf(t, rec)["code"]; got != float64(response.CodeInvalidParam) {
		t.Errorf("code = %v, want %d: %s", got, response.CodeInvalidParam, rec.Body.String())
	}
	if len(sink.runs) != 0 {
		t.Errorf("the sink received %d runs, want none", len(sink.runs))
	}
}

func TestIngestAnswersSuccessWhenTheRunWasAlreadyStored(t *testing.T) {
	// agent 在 CronJob 里跑，网络抖动重试是常态。重复的一次是同一次采集
	// 又说了一遍，不是失败 —— 答 500 会让 agent 接着重试，而每一次都会
	// 得到同样的结果。
	sink := &recordingSink{err: snapshotstore.ErrRunExists}
	h, _, cookie := newTestRouterWithAgentSink(t, sink)
	_, token := issueAgent(t, h, cookie, "prod-asia-1")

	rec := postRun(t, h, token, runBody(``))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := bodyOf(t, rec)["code"]; got != float64(response.CodeOK) {
		t.Errorf("code = %v, want 0: %s", got, rec.Body.String())
	}
}

func TestIngestFailsLoudlyWhenItCannotStore(t *testing.T) {
	sink := &recordingSink{err: errStorage}
	h, _, cookie := newTestRouterWithAgentSink(t, sink)
	_, token := issueAgent(t, h, cookie, "prod-asia-1")

	rec := postRun(t, h, token, runBody(``))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 — 落不了库却答成功，agent 会把这一轮"+
			"当成已经交付，而那批观测就此丢了", rec.Code)
	}
}

func TestIngestRequiresAnAgentToken(t *testing.T) {
	sink := &recordingSink{}
	h, _, _ := newTestRouterWithAgentSink(t, sink)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/collection-runs",
		strings.NewReader(runBody(``)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if len(sink.runs) != 0 {
		t.Errorf("the sink received %d runs from an unauthenticated call", len(sink.runs))
	}
}

func TestIngestStoresAnAbortedRunWithoutFakingAnEmptyCluster(t *testing.T) {
	sink := &recordingSink{}
	h, _, cookie := newTestRouterWithAgentSink(t, sink)
	_, token := issueAgent(t, h, cookie, "prod-asia-1")

	// 一次在读到集群之前就失败的运行走另一条落库路径：它没有观测，走
	// Save 会为每一类资源写一行 0，而那些 0 全是假话 —— 界面会显示
	// 「采到了零个 Pod、零个 Service」，读起来像一个空集群。
	body := `{"schemaVersion":1,"runId":"r-aborted","status":"FAILED",
	          "errorReason":"CREDENTIAL_UNAVAILABLE",
	          "startedAt":"2026-08-18T00:00:00Z","finishedAt":"2026-08-18T00:00:01Z",
	          "observedAt":"2026-08-18T00:00:01Z","observation":{"pods":[]}}`
	rec := postRun(t, h, token, body)
	if got := bodyOf(t, rec)["code"]; got != float64(response.CodeOK) {
		t.Fatalf("code = %v, want 0: %s", got, rec.Body.String())
	}
	if len(sink.runs) != 0 {
		t.Errorf("the sink stored %d ordinary runs, want none", len(sink.runs))
	}
	if len(sink.aborted) != 1 {
		t.Fatalf("the sink stored %d aborted runs, want 1", len(sink.aborted))
	}
	got := sink.aborted[0]
	if got.clusterID != "prod-asia-1" {
		t.Errorf("aborted run cluster = %q, want prod-asia-1", got.clusterID)
	}
	if got.reason != snapshot.RunErrorCredentialUnavailable {
		t.Errorf("reason = %q, want %q", got.reason, snapshot.RunErrorCredentialUnavailable)
	}
}

func TestIngestRejectsAnAbortedRunThatContradictsItself(t *testing.T) {
	sink := &recordingSink{}
	h, _, cookie := newTestRouterWithAgentSink(t, sink)
	_, token := issueAgent(t, h, cookie, "prod-asia-1")

	cases := map[string]string{
		// 一轮没能开始的运行不可能带着资产：两者同时出现说明报文自相矛盾，
		// 而收下它会让一份「失败但有数据」的运行进库 —— 之后没有任何一屏
		// 知道该信哪一半。
		"carries assets": `{"schemaVersion":1,"runId":"r-x","status":"FAILED",
		  "errorReason":"CREDENTIAL_UNAVAILABLE",
		  "startedAt":"2026-08-18T00:00:00Z","finishedAt":"2026-08-18T00:00:01Z",
		  "observedAt":"2026-08-18T00:00:01Z",
		  "observation":{"pods":[{"namespace":"app","name":"web-1","ip":"10.128.0.5","uid":"u","phase":"Running"}]}}`,
		// 带着原因却说自己成功，同样自相矛盾。
		"claims success": `{"schemaVersion":1,"runId":"r-y","status":"OK",
		  "errorReason":"CREDENTIAL_UNAVAILABLE",
		  "startedAt":"2026-08-18T00:00:00Z","finishedAt":"2026-08-18T00:00:01Z",
		  "observedAt":"2026-08-18T00:00:01Z","observation":{"pods":[]}}`,
		// 原因不在封闭枚举内：它落进 collection_run.error_reason，那一列的
		// 封闭性只由 Go 侧保证，而这个取值来自别人集群里的进程。
		"an unregistered reason": `{"schemaVersion":1,"runId":"r-z","status":"FAILED",
		  "errorReason":"SOMETHING_ELSE",
		  "startedAt":"2026-08-18T00:00:00Z","finishedAt":"2026-08-18T00:00:01Z",
		  "observedAt":"2026-08-18T00:00:01Z","observation":{"pods":[]}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			rec := postRun(t, h, token, body)
			if got := bodyOf(t, rec)["code"]; got != float64(response.CodeInvalidParam) {
				t.Errorf("code = %v, want %d: %s", got, response.CodeInvalidParam, rec.Body.String())
			}
		})
	}
	if len(sink.aborted) != 0 || len(sink.runs) != 0 {
		t.Errorf("the sink received %d runs and %d aborted runs, want none",
			len(sink.runs), len(sink.aborted))
	}
}
