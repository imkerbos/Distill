package flow_test

import (
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/flow"
	"github.com/imkerbos/Distill/internal/identity"
)

func hourWindow() flow.Window {
	from := time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)
	return flow.Window{From: from, To: from.Add(time.Hour)}
}

// fullResult 是一次证据齐备、且每一项都指向"没漏"的摄入 —— 本文件里唯一
// 允许读出 COMPLETE 的构造。其余用例都从它上面拿掉一项证据。
func fullResult(t *testing.T, conns []flow.Connection) flow.IngestResult {
	t.Helper()
	win := hourWindow()
	r, err := flow.NewIngestResult(flow.SourceHubble, win, win, conns)
	if err != nil {
		t.Fatalf("NewIngestResult: %v", err)
	}
	return r.WithSampleRate(1).WithDropped(0)
}

// 采样率未知就是未知。填 1.0 等于宣称"一条没漏"，而那是一句没有依据的
// 话 —— 下游据此不降级，于是一批没被看见的连接被当成不存在，其规则被判
// "可收紧"（spec §2）。
func TestUnknownSampleRateIsNotOne(t *testing.T) {
	// 零值的 IngestResult 必须报"完整度未知"，而不是"完整"。
	var zero flow.IngestResult

	rate, known := zero.SampleRate()
	if known {
		t.Fatalf("zero IngestResult reports a known sample rate %v", rate)
	}
	if rate == 1 {
		t.Fatalf("zero IngestResult sample rate reads as 1.0")
	}

	// 走调用方真正用的那条路：完整度只能从 Connections 拿到。
	conns, completeness := zero.Connections()
	if len(conns) != 0 {
		t.Fatalf("zero IngestResult has %d connections", len(conns))
	}
	if completeness == flow.CompletenessComplete {
		t.Fatalf("zero IngestResult reads as COMPLETE")
	}
	if completeness != flow.CompletenessUnknown {
		t.Fatalf("zero IngestResult completeness = %q, want UNKNOWN", completeness)
	}

	// 一次覆盖满了整段窗口、但拿不到采样率的摄入同样是未知：窗口对上
	// 不足以证明没漏，Hubble 高负载下的丢弃正是发生在窗口之内。
	win := hourWindow()
	covered, err := flow.NewIngestResult(flow.SourceHubble, win, win, nil)
	if err != nil {
		t.Fatalf("NewIngestResult: %v", err)
	}
	if _, c := covered.WithDropped(0).Connections(); c != flow.CompletenessUnknown {
		t.Fatalf("窗口覆盖满但采样率未知时 completeness = %q, want UNKNOWN", c)
	}

	// 采样率取不到时 WithSampleRate 不该被调用；即便调用方递进一个越界值，
	// 也必须停在未知，而不是被当成一份证据。
	if _, known := covered.WithSampleRate(0).SampleRate(); known {
		t.Fatal("WithSampleRate(0) 被当成了已知采样率")
	}
	if _, known := covered.WithSampleRate(1.5).SampleRate(); known {
		t.Fatal("WithSampleRate(1.5) 被当成了已知采样率")
	}
}

// 空的 verdict 与"放行"必须是两个值。把空当放行，会让一批实际被拒的连接
// 被当成正常业务流量，进而生成一条"允许"规则。
func TestAbsentVerdictIsNotAllow(t *testing.T) {
	var absent flow.Connection

	v, reported := absent.Verdict()
	if reported {
		t.Fatalf("零值 Connection 报告了判定 %q", v)
	}
	if v == flow.VerdictAllowed {
		t.Fatal("缺席的 verdict 等于 ALLOWED")
	}
	if flow.Verdict("").Valid() {
		t.Fatal("空 verdict 被登记为合法取值")
	}

	// 来源报了拒绝时必须原样带出来，否则上面那条断言可以靠"永远不报"通过。
	denied, reported := absent.WithVerdict(flow.VerdictDenied).Verdict()
	if !reported || denied != flow.VerdictDenied {
		t.Fatalf("WithVerdict(DENIED) = (%q, %v), want (DENIED, true)", denied, reported)
	}
	allowed, reported := absent.WithVerdict(flow.VerdictAllowed).Verdict()
	if !reported || allowed != flow.VerdictAllowed {
		t.Fatalf("WithVerdict(ALLOWED) = (%q, %v), want (ALLOWED, true)", allowed, reported)
	}

	// 未登记的取值不得成为判定：一个平台不认识的字符串不该决定放行与否。
	if v, reported := absent.WithVerdict(flow.Verdict("PERMITTED")).Verdict(); reported {
		t.Fatalf("未登记的 verdict %q 被当成了判定", v)
	}
	// 附上判定后又被清掉，同样要退回"没报"，不得留着上一次的取值。
	if v, reported := absent.WithVerdict(flow.VerdictDenied).WithVerdict("").Verdict(); reported {
		t.Fatalf("清空判定后仍报告 %q", v)
	}
}

// 接口不得假设流量自带身份：自带的填上，不自带的留空由解析器补。
func TestConnectionCanCarryNoIdentity(t *testing.T) {
	// VPC flow logs 那一侧：只有地址。
	bare := flow.Connection{
		Source:        flow.Endpoint{IP: "10.4.1.7"},
		Dest:          flow.Endpoint{IP: "10.4.2.9"},
		Protocol:      flow.ProtocolTCP,
		Port:          8080,
		ObservedCount: 3,
	}

	for name, ep := range map[string]flow.Endpoint{"source": bare.Source, "dest": bare.Dest} {
		subject, outcome := ep.Identity()
		if outcome != identity.OutcomeNoData {
			t.Fatalf("%s: 未解析端点的可信度 = %q, want NO_DATA", name, outcome)
		}
		if subject != (identity.Identity{}) {
			t.Fatalf("%s: 未解析端点带出了主体 %+v", name, subject)
		}
	}

	// Hubble 那一侧：流量自带 Pod 标签，填上就该原样带出来。
	api := identity.Identity{Namespace: "payment", PodName: "api-7d9", WorkloadKind: "Deployment", WorkloadName: "api"}
	resolved := bare.Source.WithIdentity(api, identity.OutcomeResolved)
	subject, outcome := resolved.Identity()
	if outcome != identity.OutcomeResolved || subject != api {
		t.Fatalf("WithIdentity(RESOLVED) = (%+v, %q), want (%+v, RESOLVED)", subject, outcome, api)
	}

	// 解析器答 AMBIGUOUS / NOT_COVERED 时主体就是未知，不得猜。
	for _, outcome := range []identity.Outcome{identity.OutcomeAmbiguous, identity.OutcomeNotCovered, identity.OutcomeNoData} {
		got, gotOutcome := bare.Source.WithIdentity(api, outcome).Identity()
		if gotOutcome != outcome {
			t.Fatalf("WithIdentity(%q) 可信度 = %q", outcome, gotOutcome)
		}
		if got != (identity.Identity{}) {
			t.Fatalf("WithIdentity(%q) 仍带出主体 %+v", outcome, got)
		}
	}

	// 未登记的可信度按"没解析过"处理，且不得留下主体。
	got, gotOutcome := bare.Source.WithIdentity(api, identity.Outcome("PROBABLY")).Identity()
	if gotOutcome != identity.OutcomeNoData || got != (identity.Identity{}) {
		t.Fatalf("未登记 outcome = (%+v, %q), want (zero, NO_DATA)", got, gotOutcome)
	}

	// 没有身份的连接必须能一路走完摄入结果，而不是在构造时被挡下来。
	conns, _ := fullResult(t, []flow.Connection{bare}).Connections()
	if len(conns) != 1 || conns[0].Source.IP != "10.4.1.7" {
		t.Fatalf("无身份连接没能通过 IngestResult: %+v", conns)
	}
}

// 完整度是证据的函数：拿掉任何一项证据都不得读出 COMPLETE。
func TestCompletenessFollowsEvidence(t *testing.T) {
	win := hourWindow()
	half := flow.Window{From: win.From, To: win.From.Add(30 * time.Minute)}

	partial, err := flow.NewIngestResult(flow.SourceHubble, win, half, nil)
	if err != nil {
		t.Fatalf("NewIngestResult: %v", err)
	}
	unknownCovered, err := flow.NewIngestResult(flow.SourceHubble, win, flow.Window{}, nil)
	if err != nil {
		t.Fatalf("NewIngestResult: %v", err)
	}

	cases := map[string]struct {
		result flow.IngestResult
		want   flow.Completeness
	}{
		"证据齐备":     {fullResult(t, nil), flow.CompletenessComplete},
		"窗口只覆盖一半":  {partial.WithSampleRate(1).WithDropped(0), flow.CompletenessDegraded},
		"覆盖范围未知":   {unknownCovered.WithSampleRate(1).WithDropped(0), flow.CompletenessUnknown},
		"有采样":      {fullResult(t, nil).WithSampleRate(0.1), flow.CompletenessDegraded},
		"来源报告有丢弃":  {fullResult(t, nil).WithDropped(12), flow.CompletenessDegraded},
		"丢弃计数取不到":  {mustResult(t, win, win).WithSampleRate(1), flow.CompletenessUnknown},
		"采样率取不到":   {mustResult(t, win, win).WithDropped(0), flow.CompletenessUnknown},
		"两项证据都取不到": {mustResult(t, win, win), flow.CompletenessUnknown},
	}
	for name, tc := range cases {
		if _, got := tc.result.Connections(); got != tc.want {
			t.Errorf("%s: completeness = %q, want %q", name, got, tc.want)
		}
	}
}

func mustResult(t *testing.T, requested, covered flow.Window) flow.IngestResult {
	t.Helper()
	r, err := flow.NewIngestResult(flow.SourceHubble, requested, covered, nil)
	if err != nil {
		t.Fatalf("NewIngestResult: %v", err)
	}
	return r
}

// 构造器挡住的是摄入器自己写坏的情况：来源与请求窗口是调用方本来就知道的
// 事，错了不该被降级成一份"未知完整度"的结果悄悄流下去。
func TestNewIngestResultRejectsUnusableMetadata(t *testing.T) {
	win := hourWindow()
	backwards := flow.Window{From: win.To, To: win.From}

	cases := map[string]struct {
		source             flow.SourceKind
		requested, covered flow.Window
	}{
		"来源未登记":  {flow.SourceKind("VPC"), win, win},
		"来源为零值":  {"", win, win},
		"请求窗口为空": {flow.SourceHubble, flow.Window{}, win},
		"请求窗口倒置": {flow.SourceHubble, backwards, win},
		"覆盖窗口倒置": {flow.SourceHubble, win, backwards},
	}
	for name, tc := range cases {
		r, err := flow.NewIngestResult(tc.source, tc.requested, tc.covered, nil)
		if err == nil {
			t.Errorf("%s: NewIngestResult 没有报错", name)
			continue
		}
		if _, c := r.Connections(); c != flow.CompletenessUnknown {
			t.Errorf("%s: 报错时返回的结果 completeness = %q, want UNKNOWN", name, c)
		}
	}
}

// 封闭枚举的零值一律不合法：没被赋过值的取值不得被当成任何一种结论。
func TestClosedEnumsRejectZeroAndUnregistered(t *testing.T) {
	if flow.SourceKind("").Valid() || flow.SourceKind("hubble").Valid() {
		t.Error("SourceKind 接受了零值或未登记取值")
	}
	if !flow.SourceHubble.Valid() {
		t.Error("SourceHubble 被判为非法")
	}
	if flow.Completeness("").Valid() || flow.Completeness("PARTIAL").Valid() {
		t.Error("Completeness 接受了零值或未登记取值")
	}
	for _, c := range []flow.Completeness{flow.CompletenessComplete, flow.CompletenessDegraded, flow.CompletenessUnknown} {
		if !c.Valid() {
			t.Errorf("Completeness %q 被判为非法", c)
		}
	}
	if flow.Protocol("").Valid() || flow.Protocol("tcp").Valid() {
		t.Error("Protocol 接受了零值或未登记取值")
	}
	if !flow.ProtocolTCP.Valid() || !flow.ProtocolUDP.Valid() || !flow.ProtocolSCTP.Valid() {
		t.Error("已登记的 Protocol 被判为非法")
	}
	if flow.Verdict("ALLOW").Valid() {
		t.Error("Verdict 接受了未登记取值")
	}
	if !flow.VerdictAllowed.Valid() || !flow.VerdictDenied.Valid() {
		t.Error("已登记的 Verdict 被判为非法")
	}
}

func TestWindowCovers(t *testing.T) {
	win := hourWindow()
	inner := flow.Window{From: win.From.Add(time.Minute), To: win.To.Add(-time.Minute)}

	if !win.Covers(inner) || !win.Covers(win) {
		t.Error("窗口没能包住自己或内层窗口")
	}
	if inner.Covers(win) {
		t.Error("内层窗口被判为包住了外层")
	}
	// 说不出边界的窗口不得被读成"包住了"：那正是把未知读成没漏的方向。
	if (flow.Window{}).Covers(win) || win.Covers(flow.Window{}) {
		t.Error("零值窗口参与了覆盖判断")
	}
	if (flow.Window{}).Valid() {
		t.Error("零值窗口被判为合法")
	}
}
