package httpapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/imkerbos/Distill/internal/flow"
	"github.com/imkerbos/Distill/internal/response"
	"github.com/imkerbos/Distill/internal/snapshotstore"
)

// recordingFlowSink 记下摄入到的整条运行记录。
//
// 记整条而不是一个计数：一条只数次数的断言，对一次把窗口、来源或完整度
// 证据记错的摄入照样是绿的。
type recordingFlowSink struct {
	runs []snapshotstore.IngestRun
	err  error
}

func (s *recordingFlowSink) SaveIngest(_ context.Context, run snapshotstore.IngestRun) error {
	if s.err != nil {
		return s.err
	}
	s.runs = append(s.runs, run)
	return nil
}

func (s *recordingFlowSink) only(t *testing.T) snapshotstore.IngestRun {
	t.Helper()
	if len(s.runs) != 1 {
		t.Fatalf("sink holds %d ingest runs, want exactly 1", len(s.runs))
	}
	return s.runs[0]
}

const flowIngestPath = "/api/v1/agent/flow-ingests"

func postFlow(t *testing.T, h http.Handler, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, flowIngestPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// flowBody 是一份形状正确的摄入报文，caller 可以往里塞额外字段。
//
// 刻意**不带** sampleRate 与 dropped：那两项缺席是常态（Hubble 报不出），
// 而它们缺席时完整度必须落到 UNKNOWN。要 COMPLETE 的用例自己加上。
func flowBody(extra string) string {
	return `{"schemaVersion":1,"runId":"fi-1","source":"HUBBLE","status":"OK",` + extra + `
	  "startedAt":"2026-08-18T00:00:00Z","finishedAt":"2026-08-18T00:05:00Z",
	  "requestedWindow":{"from":"2026-08-18T00:00:00Z","to":"2026-08-18T00:05:00Z"},
	  "connections":[{"srcIp":"10.128.0.5","dstIp":"10.128.0.9",
	     "protocol":"TCP","port":8080,"observedCount":3,"verdict":"ALLOWED"}]}`
}

func okFlow(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if got := bodyOf(t, rec)["code"]; got != float64(response.CodeOK) {
		t.Fatalf("code = %v, want 0: %s", got, rec.Body.String())
	}
}

func refusedFlow(t *testing.T, rec *httptest.ResponseRecorder, why string) {
	t.Helper()
	if got := bodyOf(t, rec)["code"]; got == float64(response.CodeOK) {
		t.Fatalf("%s (%s)", why, rec.Body.String())
	}
}

// 归属只来自 token。这是整条摄入路径上唯一写下集群归属的地方。
func TestFlowIngestTakesTheClusterFromTheTokenNotTheBody(t *testing.T) {
	sink := &recordingFlowSink{}
	h, _, cookie := newTestRouterWithFlowSink(t, sink)
	_, token := issueAgent(t, h, cookie, "prod-asia-1")

	okFlow(t, postFlow(t, h, token, flowBody(``)))
	if got := sink.only(t).ClusterID; got != "prod-asia-1" {
		t.Errorf("stored ClusterID = %q, want prod-asia-1", got)
	}
}

// 报文里带 clusterId 要整份拒。忽略它会让一个装错 token 的 agent 静默地
// 把流量写进别的集群，而它自己以为写对了。
func TestFlowIngestRejectsAPayloadThatNamesItsOwnCluster(t *testing.T) {
	sink := &recordingFlowSink{}
	h, _, cookie := newTestRouterWithFlowSink(t, sink)
	_, token := issueAgent(t, h, cookie, "prod-asia-1")

	rec := postFlow(t, h, token, flowBody(`"clusterId":"prod-eu-1",`))
	refusedFlow(t, rec, "a payload naming its own cluster was accepted")
	if len(sink.runs) != 0 {
		t.Error("the refused payload still reached the sink")
	}
}

// **agent 连声称一个主体的语法都没有。** 身份由平台在读侧从
// pod_identity_interval 解析（collectstore.subjectAt），让 agent 报身份等于
// 开第二条解析路径，而两条会漂。
func TestFlowIngestHasNoFieldForAnIdentity(t *testing.T) {
	sink := &recordingFlowSink{}
	h, _, cookie := newTestRouterWithFlowSink(t, sink)
	_, token := issueAgent(t, h, cookie, "prod-asia-1")

	for _, claim := range []string{
		`"srcNamespace":"payment",`,
		`"srcWorkload":"api",`,
		`"srcPodUid":"u-1",`,
	} {
		body := strings.Replace(flowBody(``), `"srcIp":`, claim+`"srcIp":`, 1)
		rec := postFlow(t, h, token, body)
		refusedFlow(t, rec, "an agent asserted a subject and was accepted: "+claim)
	}
}

// 两端的 IP 必须原样落到 sink 上 —— 读侧靠它解析主体。
func TestFlowIngestCarriesTheAddressesThroughUntouched(t *testing.T) {
	sink := &recordingFlowSink{}
	h, _, cookie := newTestRouterWithFlowSink(t, sink)
	_, token := issueAgent(t, h, cookie, "prod-asia-1")

	okFlow(t, postFlow(t, h, token, flowBody(``)))
	conns, _ := sink.only(t).Result.Connections()
	if len(conns) != 1 {
		t.Fatalf("stored %d connections, want 1", len(conns))
	}
	c := conns[0]
	if c.Source.IP != "10.128.0.5" || c.Dest.IP != "10.128.0.9" {
		t.Errorf("addresses = %s -> %s, want 10.128.0.5 -> 10.128.0.9", c.Source.IP, c.Dest.IP)
	}
	if c.Protocol != flow.ProtocolTCP || c.Port != 8080 || c.ObservedCount != 3 {
		t.Errorf("connection = %s/%d ×%d, want TCP/8080 ×3", c.Protocol, c.Port, c.ObservedCount)
	}
}

// **缺席不是零值。** sampleRate 缺席时完整度不得是 COMPLETE —— 填 1.0 等于
// 宣称"一条没漏"，而那是一句没人说过的话。
func TestAnAbsentSampleRateNeverYieldsCompleteness(t *testing.T) {
	sink := &recordingFlowSink{}
	h, _, cookie := newTestRouterWithFlowSink(t, sink)
	_, token := issueAgent(t, h, cookie, "prod-asia-1")

	// 覆盖窗口给满、丢弃数给 0 —— 只差采样率这一项证据。
	okFlow(t, postFlow(t, h, token, flowBody(
		`"coveredWindow":{"from":"2026-08-18T00:00:00Z","to":"2026-08-18T00:05:00Z"},"dropped":0,`)))

	_, completeness := sink.only(t).Result.Connections()
	if completeness == flow.CompletenessComplete {
		t.Error("completeness is COMPLETE while the source never reported a sample rate; " +
			"downstream will not degrade, and connections nobody saw get read as absent")
	}
}

// dropped 缺席同理：0 是证据（"来源说一条没丢"），缺席是没有证据。
func TestAnAbsentDroppedCountNeverYieldsCompleteness(t *testing.T) {
	sink := &recordingFlowSink{}
	h, _, cookie := newTestRouterWithFlowSink(t, sink)
	_, token := issueAgent(t, h, cookie, "prod-asia-1")

	okFlow(t, postFlow(t, h, token, flowBody(
		`"coveredWindow":{"from":"2026-08-18T00:00:00Z","to":"2026-08-18T00:05:00Z"},"sampleRate":1.0,`)))

	_, completeness := sink.only(t).Result.Connections()
	if completeness == flow.CompletenessComplete {
		t.Error("completeness is COMPLETE while the source never reported a dropped count")
	}
}

// **对照组。** 三项证据齐备且都指向没漏时必须给得出 COMPLETE ——
// 少了这条，一个"一律不给 COMPLETE"的实现会让上面两条永远通过。
func TestCompleteEvidenceStillYieldsCompleteness(t *testing.T) {
	sink := &recordingFlowSink{}
	h, _, cookie := newTestRouterWithFlowSink(t, sink)
	_, token := issueAgent(t, h, cookie, "prod-asia-1")

	okFlow(t, postFlow(t, h, token, flowBody(
		`"coveredWindow":{"from":"2026-08-18T00:00:00Z","to":"2026-08-18T00:05:00Z"},`+
			`"sampleRate":1.0,"dropped":0,`)))

	_, completeness := sink.only(t).Result.Connections()
	if completeness != flow.CompletenessComplete {
		t.Errorf("completeness = %q, want COMPLETE: every piece of evidence was supplied and "+
			"all of them say nothing was missed", completeness)
	}
}

// 报文里没有 completeness 这个字段 —— 完整度是证据的函数，不是可以被填的值。
func TestFlowIngestHasNoFieldForCompleteness(t *testing.T) {
	sink := &recordingFlowSink{}
	h, _, cookie := newTestRouterWithFlowSink(t, sink)
	_, token := issueAgent(t, h, cookie, "prod-asia-1")

	rec := postFlow(t, h, token, flowBody(`"completeness":"COMPLETE",`))
	refusedFlow(t, rec, "an agent declared its own completeness and was accepted")
}

// 未登记的来源整份拒，不降级成 UNKNOWN：一批说不出自己从哪来的连接，
// 也说不出自己有多完整。
func TestFlowIngestRejectsAnUnregisteredSource(t *testing.T) {
	sink := &recordingFlowSink{}
	h, _, cookie := newTestRouterWithFlowSink(t, sink)
	_, token := issueAgent(t, h, cookie, "prod-asia-1")

	// 取一个**真的没登记过**的取值。原来这里写的是 NODE_CONNTRACK，而
	// conntrack 采集器落地那一轮把它登记了 —— 反例变成了正例，用例于是
	// 静默失去了判别力。挑一个来源枚举里不会有的词。
	body := strings.Replace(flowBody(``), `"source":"HUBBLE"`, `"source":"CARRIER_PIGEON"`, 1)
	rec := postFlow(t, h, token, body)
	refusedFlow(t, rec, "an unregistered source kind was accepted")
}

// 失败的摄入要落库 —— 什么都不留下的话，这个集群在界面上就是
// "这段时间没有流量"，而下游把"没有流量"读成"这些规则可以收紧"。
func TestAFailedIngestIsStoredSoTheWindowIsNotReadAsQuiet(t *testing.T) {
	sink := &recordingFlowSink{}
	h, _, cookie := newTestRouterWithFlowSink(t, sink)
	_, token := issueAgent(t, h, cookie, "prod-asia-1")

	body := `{"schemaVersion":1,"runId":"fi-fail","source":"HUBBLE","status":"FAILED",
	  "errorReason":"UNREACHABLE",
	  "startedAt":"2026-08-18T00:00:00Z","finishedAt":"2026-08-18T00:00:01Z",
	  "requestedWindow":{"from":"2026-08-18T00:00:00Z","to":"2026-08-18T00:05:00Z"}}`
	okFlow(t, postFlow(t, h, token, body))

	run := sink.only(t)
	if run.Status != snapshotstore.IngestFailed {
		t.Errorf("stored status = %q, want FAILED", run.Status)
	}
	if run.ErrorReason != snapshotstore.IngestErrorUnreachable {
		t.Errorf("stored reason = %q, want UNREACHABLE", run.ErrorReason)
	}
}

// 一次没能拿到数据的摄入不可能带着连接。两者同时出现说明报文自相矛盾，
// 收下它会让一份"失败但有数据"的运行进库 —— 之后没有任何一屏知道该信哪一半。
func TestAFailedIngestThatCarriesConnectionsIsRefused(t *testing.T) {
	sink := &recordingFlowSink{}
	h, _, cookie := newTestRouterWithFlowSink(t, sink)
	_, token := issueAgent(t, h, cookie, "prod-asia-1")

	body := strings.Replace(flowBody(``), `"status":"OK",`,
		`"status":"FAILED","errorReason":"UNREACHABLE",`, 1)
	rec := postFlow(t, h, token, body)
	refusedFlow(t, rec, "a failed ingest carrying connections was accepted")
}

// 未登记的失败原因要拒：error_reason 在库里只是一列 VARCHAR，封闭性由 Go 侧
// 保证，而这个取值来自别人集群里的进程。
func TestFlowIngestRejectsAnUnregisteredErrorReason(t *testing.T) {
	sink := &recordingFlowSink{}
	h, _, cookie := newTestRouterWithFlowSink(t, sink)
	_, token := issueAgent(t, h, cookie, "prod-asia-1")

	body := `{"schemaVersion":1,"runId":"fi-fail","source":"HUBBLE","status":"FAILED",
	  "errorReason":"THE_NETWORK_WAS_SAD",
	  "startedAt":"2026-08-18T00:00:00Z","finishedAt":"2026-08-18T00:00:01Z",
	  "requestedWindow":{"from":"2026-08-18T00:00:00Z","to":"2026-08-18T00:05:00Z"}}`
	refusedFlow(t, postFlow(t, h, token, body), "an unregistered error reason was accepted")
}

// 重复推同一个 runId 答成功：agent 跑在 CronJob 里，重试是常态。
func TestReingestingTheSameRunAnswersSuccess(t *testing.T) {
	sink := &recordingFlowSink{err: snapshotstore.ErrIngestRunExists}
	h, _, cookie := newTestRouterWithFlowSink(t, sink)
	_, token := issueAgent(t, h, cookie, "prod-asia-1")

	okFlow(t, postFlow(t, h, token, flowBody(``)))
}

// 条数超限是**调用方要得太多**，不是平台故障。答一个说得出成因的业务码，
// 而不是 500 —— 500 会让 agent 原样重试，每一次都得到同样的结果。
func TestTooManyConnectionsIsAnswerableNotAServerFailure(t *testing.T) {
	sink := &recordingFlowSink{err: snapshotstore.ErrTooManyConnections}
	h, _, cookie := newTestRouterWithFlowSink(t, sink)
	_, token := issueAgent(t, h, cookie, "prod-asia-1")

	rec := postFlow(t, h, token, flowBody(``))
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 with a business code: an over-large ingest is not a "+
			"platform failure, and a 5xx makes the agent retry forever", rec.Code)
	}
	refusedFlow(t, rec, "an over-large ingest was reported as success")
}

// 没有装配 sink 的部署收不下摄入。答"依赖不可用"而不是成功 ——
// 答成功会让 agent 把这一轮当成已经交付，那批观测就此丢了。
func TestFlowIngestWithoutASinkIsNotSilentlyAccepted(t *testing.T) {
	h, _, cookie := newTestRouterWithFlowSink(t, nil)
	_, token := issueAgent(t, h, cookie, "prod-asia-1")

	rec := postFlow(t, h, token, flowBody(``))
	// **断言具体的 503，不是"反正不是 200"。** 绕过这道检查的实现会在
	// nil 接口上 panic，被 Recoverer 兜成 500 —— 那同样不是 200，于是一条
	// 宽松的断言会以"被拒了"的面貌通过，而真实行为是每个请求打一次 panic。
	//
	// 503 与 500 对 agent 也不是一回事：前者说"这个部署收不下流量"，
	// 后者说"平台坏了"，而后者会让 agent 一直重试。
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rec.Code, rec.Body.String())
	}
	if got := bodyOf(t, rec)["code"]; got != float64(response.CodeDependencyUnavailable) {
		t.Errorf("code = %v, want CodeDependencyUnavailable", got)
	}
}

// 这条路由挂在 agent 链上：没有有效 token 一律 401。
func TestFlowIngestRequiresAnAgentToken(t *testing.T) {
	sink := &recordingFlowSink{}
	h, _, _ := newTestRouterWithFlowSink(t, sink)

	req := httptest.NewRequest(http.MethodPost, flowIngestPath, strings.NewReader(flowBody(``)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if len(sink.runs) != 0 {
		t.Error("an unauthenticated ingest reached the sink")
	}
}
