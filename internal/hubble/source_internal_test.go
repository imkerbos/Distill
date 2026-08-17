package hubble

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	flowpb "github.com/cilium/cilium/api/v1/flow"
	"github.com/cilium/cilium/api/v1/observer"
	relaypb "github.com/cilium/cilium/api/v1/relay"
	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/imkerbos/Distill/internal/flow"
	"github.com/imkerbos/Distill/internal/identity"
	"github.com/imkerbos/Distill/internal/kubeclient"
)

// testWindow 是所有用例共用的摄入窗口。
var testWindow = flow.Window{
	From: time.Date(2026, 8, 17, 10, 0, 0, 0, time.UTC),
	To:   time.Date(2026, 8, 17, 11, 0, 0, 0, time.UTC),
}

// fakeRelay 是一个进程内的 Observer 服务端。
type fakeRelay struct {
	observer.UnimplementedObserverServer
	getFlows func(*observer.GetFlowsRequest, grpc.ServerStreamingServer[observer.GetFlowsResponse]) error
}

func (f *fakeRelay) GetFlows(
	req *observer.GetFlowsRequest,
	stream grpc.ServerStreamingServer[observer.GetFlowsResponse],
) error {
	return f.getFlows(req, stream)
}

// startRelay 起一个假 relay，并返回一个连向它的 Source。
//
// 这里把 s.dial 换掉，于是**这些用例不经过地址守卫** —— 守卫由
// TestOutboundIsRefusedWhenTheRelayAddressIsBlocked 用生产构造函数单独钉住。
// 两者分开是刻意的：一个能被换掉拨号的用例，证明不了生产路径还在调用守卫。
func startRelay(
	t *testing.T,
	handler func(*observer.GetFlowsRequest, grpc.ServerStreamingServer[observer.GetFlowsResponse]) error,
) *Source {
	t.Helper()

	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	observer.RegisterObserverServer(srv, &fakeRelay{getFlows: handler})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() {
		srv.Stop()
		_ = lis.Close()
	})

	src, err := New(Config{Address: "hubble-relay.invalid:4245", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	src.dial = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return lis.DialContext(ctx)
	}
	return src
}

// streamOf 返回一个把给定消息依次发出去的 handler。
func streamOf(msgs ...*observer.GetFlowsResponse) func(
	*observer.GetFlowsRequest, grpc.ServerStreamingServer[observer.GetFlowsResponse],
) error {
	return func(_ *observer.GetFlowsRequest, stream grpc.ServerStreamingServer[observer.GetFlowsResponse]) error {
		for _, m := range msgs {
			if err := stream.Send(m); err != nil {
				return err
			}
		}
		return nil
	}
}

// tcpFlow 造一条最小可用的 TCP flow。
func tcpFlow(src, dst string, port uint32, verdict flowpb.Verdict) *observer.GetFlowsResponse {
	return &observer.GetFlowsResponse{
		ResponseTypes: &observer.GetFlowsResponse_Flow{Flow: &flowpb.Flow{
			Time:    timestamppb.New(testWindow.From.Add(10 * time.Minute)),
			Verdict: verdict,
			IP:      &flowpb.IP{Source: src, Destination: dst},
			L4: &flowpb.Layer4{Protocol: &flowpb.Layer4_TCP{
				TCP: &flowpb.TCP{DestinationPort: port},
			}},
		}},
	}
}

// TestReplyFlowsAreDroppedButAnUnknownDirectionIsKept 钉的是两半，必须一起读。
//
// 前一半：回程报文的五元组是反着的，照收会凭空造出一条目的端口是临时端口的
// "连接"，策略生成器会据此给出一条指向临时端口的 egress 规则 —— 错的输出，
// 且运维认不出它是摄入的假象。
//
// 后一半：`is_reply` 缺失是"不知道方向"，不是"不是回程"。把不知道当成回程丢掉，
// 会丢掉真实存在的连接，而那是本轮唯一要防的方向。
func TestReplyFlowsAreDroppedButAnUnknownDirectionIsKept(t *testing.T) {
	forward := tcpFlow("10.0.0.1", "10.0.0.2", 8080, flowpb.Verdict_FORWARDED)
	forward.GetFlow().IsReply = wrapperspb.Bool(false)

	reply := tcpFlow("10.0.0.2", "10.0.0.1", 54321, flowpb.Verdict_FORWARDED)
	reply.GetFlow().IsReply = wrapperspb.Bool(true)

	// 方向未知：Hubble 没带 is_reply。必须留下。
	unknown := tcpFlow("10.0.0.3", "10.0.0.4", 9090, flowpb.Verdict_FORWARDED)

	src := startRelay(t, streamOf(forward, reply, unknown))
	res, err := src.Ingest(context.Background(), "cluster-a", testWindow)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	conns, _ := res.Connections()

	seen := map[string]bool{}
	for _, c := range conns {
		seen[fmt.Sprintf("%s->%s:%d", c.Source.IP, c.Dest.IP, c.Port)] = true
	}

	if !seen["10.0.0.1->10.0.0.2:8080"] {
		t.Errorf("the forward flow did not produce a connection")
	}
	if !seen["10.0.0.3->10.0.0.4:9090"] {
		t.Errorf("a flow with no is_reply was dropped; absent direction is unknown, not a reply — " +
			"dropping it loses a real connection, and a lost connection gets its rule cut")
	}
	if seen["10.0.0.2->10.0.0.1:54321"] {
		t.Errorf("a reply flow became a connection to an ephemeral port; " +
			"the policy generator reads the fact layer as connections that exist")
	}
	if len(conns) != 2 {
		t.Fatalf("connections = %d, want 2", len(conns))
	}
}

// TestUnavailableDropCountDegradesRatherThanClaimsComplete 钉的是 spec §5：
// relay 没报丢弃计数时，采样率必须留空，**不得填 1.0**。
//
// 填 1.0 等于宣称"一条没漏"。下游据此不降级，于是一条没被看见的连接被读成
// 不存在，覆盖它的规则被判"无流量、可收紧"，推荐出来的策略把它切断。
func TestUnavailableDropCountDegradesRatherThanClaimsComplete(t *testing.T) {
	src := startRelay(t, streamOf(tcpFlow("10.0.0.1", "10.0.0.2", 8080, flowpb.Verdict_FORWARDED)))

	res, err := src.Ingest(context.Background(), "cluster-a", testWindow)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	if _, reported := res.Dropped(); reported {
		t.Fatalf("relay sent no LostEvent, yet the result reports a drop count")
	}
	rate, known := res.SampleRate()
	if known {
		t.Fatalf("sample rate must stay unknown when the drop count is unavailable; got %v", rate)
	}

	conns, completeness := res.Connections()
	if len(conns) != 1 {
		t.Fatalf("connections = %d, want 1", len(conns))
	}
	if completeness == flow.CompletenessComplete {
		t.Fatalf("completeness = COMPLETE with no sampling and no drop evidence")
	}
	if completeness != flow.CompletenessUnknown {
		t.Fatalf("completeness = %s, want UNKNOWN", completeness)
	}
}

// TestAReportedDropCountIsCarriedThroughAndDegrades 是上一条的另一半：
// relay 真的报了丢弃时，那个数字必须落到结果里，且完整度降级。
func TestAReportedDropCountIsCarriedThroughAndDegrades(t *testing.T) {
	src := startRelay(t, streamOf(
		tcpFlow("10.0.0.1", "10.0.0.2", 8080, flowpb.Verdict_FORWARDED),
		&observer.GetFlowsResponse{ResponseTypes: &observer.GetFlowsResponse_LostEvents{
			LostEvents: &flowpb.LostEvent{NumEventsLost: 17},
		}},
	))

	res, err := src.Ingest(context.Background(), "cluster-a", testWindow)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	n, reported := res.Dropped()
	if !reported || n != 17 {
		t.Fatalf("Dropped() = (%d, %v), want (17, true)", n, reported)
	}
	if _, completeness := res.Connections(); completeness != flow.CompletenessDegraded {
		t.Fatalf("completeness = %s, want DEGRADED when the relay reports losses", completeness)
	}
}

// TestVerdictIsCarriedThroughAndAbsenceIsPreserved 钉的是 spec §4：
// Hubble 报得出的判定必须活着落到结果里；报不出来的必须留空，**不得变成放行**。
func TestVerdictIsCarriedThroughAndAbsenceIsPreserved(t *testing.T) {
	src := startRelay(t, streamOf(
		tcpFlow("10.0.0.1", "10.0.0.2", 8080, flowpb.Verdict_FORWARDED),
		tcpFlow("10.0.0.1", "10.0.0.3", 8080, flowpb.Verdict_DROPPED),
		tcpFlow("10.0.0.1", "10.0.0.4", 8080, flowpb.Verdict_TRACED),
		tcpFlow("10.0.0.1", "10.0.0.5", 8080, flowpb.Verdict_AUDIT),
	))

	res, err := src.Ingest(context.Background(), "cluster-a", testWindow)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	conns, _ := res.Connections()

	byDest := map[string]flow.Connection{}
	for _, c := range conns {
		byDest[c.Dest.IP] = c
	}

	for _, tc := range []struct {
		dest     string
		want     flow.Verdict
		reported bool
	}{
		{"10.0.0.2", flow.VerdictAllowed, true},
		{"10.0.0.3", flow.VerdictDenied, true},
		{"10.0.0.4", "", false},
		{"10.0.0.5", "", false},
	} {
		c, ok := byDest[tc.dest]
		if !ok {
			t.Fatalf("no connection to %s", tc.dest)
		}
		got, reported := c.Verdict()
		if reported != tc.reported || got != tc.want {
			t.Errorf("verdict for %s = (%q, %v), want (%q, %v)", tc.dest, got, reported, tc.want, tc.reported)
		}
		if !tc.reported && got == flow.VerdictAllowed {
			t.Errorf("an unreported verdict for %s became ALLOWED", tc.dest)
		}
	}
}

// TestATimedOutIngestIsNotAnEmptyWindow 钉的是：超时必须报错。
//
// 一次因为放弃而安静返回的摄入，与"这段时间真的没有流量"在下游长得一模一样，
// 而后者会让覆盖这些连接的规则被判成可以收紧。
func TestATimedOutIngestIsNotAnEmptyWindow(t *testing.T) {
	src := startRelay(t, func(
		_ *observer.GetFlowsRequest, stream grpc.ServerStreamingServer[observer.GetFlowsResponse],
	) error {
		<-stream.Context().Done()
		return stream.Context().Err()
	})
	src.timeout = 200 * time.Millisecond

	res, err := src.Ingest(context.Background(), "cluster-a", testWindow)
	if err == nil {
		conns, completeness := res.Connections()
		t.Fatalf("a timed-out ingest returned no error: %d connections, completeness %s", len(conns), completeness)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want it to carry context.DeadlineExceeded", err)
	}
}

// TestOutboundIsRefusedWhenTheRelayAddressIsBlocked 钉的是**调用点**，不是守卫。
//
// 守卫本身在 internal/kubeclient 有自己的用例。这一条要证明的是另一件事：
// 生产构造出来的 Source 出站时**仍然经过它**。本项目已经出现十八次"守卫被
// 测到、却没有任何东西证明调用方还在调用它"的用例，这一条就是冲那个形状写的
// —— 把 New 里的 kubeclient.GuardedDialContext 换成一个裸 net.Dialer，
// 这里必须变红。
//
// 用云元数据地址而不是随便一个私有地址：kubeclient 的判定刻意比 gitssh 宽，
// 私有地址是**允许**的（集群内的 relay 与 apiserver 同类），链路本地才是拦截目标。
func TestOutboundIsRefusedWhenTheRelayAddressIsBlocked(t *testing.T) {
	src, err := New(Config{Address: "169.254.169.254:4245", Timeout: 3 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = src.Ingest(context.Background(), "cluster-a", testWindow)
	if !errors.Is(err, kubeclient.ErrBlockedDestination) {
		t.Fatalf("err = %v, want kubeclient.ErrBlockedDestination; "+
			"the outbound path is no longer going through the address guard", err)
	}
}

// TestPrivateRelayAddressesAreNotBlocked 是上一条的反向：守卫不能宽到没用，
// 也不能严到把正常情况挡掉。集群内的 relay 就在 RFC1918 里。
func TestPrivateRelayAddressesAreNotBlocked(t *testing.T) {
	src, err := New(Config{Address: "10.96.0.10:4245", Timeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = src.Ingest(context.Background(), "cluster-a", testWindow)
	if errors.Is(err, kubeclient.ErrBlockedDestination) {
		t.Fatalf("an in-cluster relay address was refused by the address guard; "+
			"copying gitssh's policy would block the normal case: %v", err)
	}
}

// TestIdentityIsFilledFromPodLabelsAndLeftAbsentOtherwise 钉的是 spec §3：
// Hubble 的 flow 自带 Pod 标签，这个来源填得上身份 —— 但对端在集群外、
// 或标签缺失时，它必须说得出"填不上"，**不得凭空造一个主体**。
func TestIdentityIsFilledFromPodLabelsAndLeftAbsentOtherwise(t *testing.T) {
	resp := tcpFlow("10.0.0.1", "203.0.113.9", 443, flowpb.Verdict_FORWARDED)
	f := resp.GetFlow()
	f.Source = &flowpb.Endpoint{
		Namespace: "payments",
		PodName:   "api-7d9f-abcde",
		Workloads: []*flowpb.Workload{{Kind: "Deployment", Name: "api"}},
	}
	// 目的端是集群外的地址：Hubble 给不出 namespace / pod 名。
	f.Destination = &flowpb.Endpoint{}

	src := startRelay(t, streamOf(resp))
	res, err := src.Ingest(context.Background(), "cluster-a", testWindow)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	conns, _ := res.Connections()
	if len(conns) != 1 {
		t.Fatalf("connections = %d, want 1", len(conns))
	}

	subject, outcome := conns[0].Source.Identity()
	if outcome != identity.OutcomeResolved {
		t.Fatalf("source outcome = %s, want RESOLVED", outcome)
	}
	want := identity.Identity{
		Namespace: "payments", PodName: "api-7d9f-abcde",
		WorkloadKind: "Deployment", WorkloadName: "api",
	}
	if subject != want {
		t.Fatalf("source identity = %+v, want %+v", subject, want)
	}

	peer, peerOutcome := conns[0].Dest.Identity()
	if peerOutcome != identity.OutcomeNoData {
		t.Fatalf("peer outcome = %s, want NO_DATA; a peer without labels must not get an invented subject", peerOutcome)
	}
	if peer != (identity.Identity{}) {
		t.Fatalf("peer identity = %+v, want zero", peer)
	}
}

// TestANodeGapLeavesCoverageUnknown 钉的是：relay 说某个节点不在线时，
// 结果不得表现成"这段时间全看见了"。
func TestANodeGapLeavesCoverageUnknown(t *testing.T) {
	src := startRelay(t, streamOf(
		tcpFlow("10.0.0.1", "10.0.0.2", 8080, flowpb.Verdict_FORWARDED),
		&observer.GetFlowsResponse{ResponseTypes: &observer.GetFlowsResponse_NodeStatus{
			NodeStatus: &relaypb.NodeStatusEvent{
				StateChange: relaypb.NodeState_NODE_UNAVAILABLE,
				NodeNames:   []string{"node-3"},
			},
		}},
	))

	res, err := src.Ingest(context.Background(), "cluster-a", testWindow)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	_, covered := res.Windows()
	if covered != (flow.Window{}) {
		t.Fatalf("covered = %+v, want zero: a node we could not see leaves coverage unknown", covered)
	}
	if _, completeness := res.Connections(); completeness == flow.CompletenessComplete {
		t.Fatalf("completeness = COMPLETE despite a node the relay could not reach")
	}
}

// TestAnUnparsableCredentialFailsRatherThanFallingBackToPlaintext 钉的是：
// 凭据坏掉必须失败。回退明文的症状是"它还在工作"，而实际上已经没有任何
// 东西在验证对端是不是 relay。
func TestAnUnparsableCredentialFailsRatherThanFallingBackToPlaintext(t *testing.T) {
	src := startRelay(t, streamOf())
	src.credentialRef = "relay-ca"
	src.secrets = resolverFunc(func(context.Context, string) ([]byte, error) {
		return []byte("not a pem block"), nil
	})

	if _, err := src.Ingest(context.Background(), "cluster-a", testWindow); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("err = %v, want ErrInvalidCredential", err)
	}
}

// fillToReadLimit 一直喂互不相同的连接，直到 observe 报告读满为止。
//
// 走 observe 而不是直接把 truncated 置上：这一轮的教训正是"守卫被测到、
// 调用点没有"，把上限那一行也一起钉住才算数。withTime 为 false 时喂的 flow
// 不带时间戳 —— 那是覆盖窗口说不出停在哪一刻的那种情形。
func fillToReadLimit(t *testing.T, a *accumulator, withTime bool, at time.Time) int {
	t.Helper()
	for i := 0; i < maxFlowsPerIngest+1; i++ {
		f := &flowpb.Flow{
			Verdict: flowpb.Verdict_FORWARDED,
			// 每条一个新的目的地址，于是每条都自成一个 connKey。
			IP: &flowpb.IP{
				Source:      "10.0.0.1",
				Destination: fmt.Sprintf("10.%d.%d.%d", (i>>16)&0xff, (i>>8)&0xff, i&0xff),
			},
			L4: &flowpb.Layer4{Protocol: &flowpb.Layer4_TCP{TCP: &flowpb.TCP{DestinationPort: 8080}}},
		}
		if withTime {
			f.Time = timestamppb.New(at)
		}
		if a.observe(&observer.GetFlowsResponse{ResponseTypes: &observer.GetFlowsResponse_Flow{Flow: f}}) {
			return i + 1
		}
	}
	t.Fatalf("observe never reported the read limit after %d flows", maxFlowsPerIngest+1)
	return 0
}

// TestReachingTheReadLimitIsAKnownGapNotFullCoverage 钉的是截断 → DEGRADED
// 这条路，它此前一条用例都没有：把整个截断分支换成 `return flow.Window{}`，
// 全套测试原本一条都不红。
//
// 读到上限就停，意味着这个窗口的后半段我们根本没在看。此时若把请求窗口原样
// 当作覆盖窗口交出去，完整度就会落到"没有缺口证据"那一档，而那是一句我们
// 恰恰有反证的话 —— 覆盖窗口必须缩到我们真正看见的最后一刻。
func TestReachingTheReadLimitIsAKnownGapNotFullCoverage(t *testing.T) {
	a := newAccumulator()
	last := testWindow.From.Add(10 * time.Minute)

	if n := fillToReadLimit(t, a, true, last); n != maxFlowsPerIngest {
		t.Fatalf("observe reported the read limit after %d flows, want %d", n, maxFlowsPerIngest)
	}
	if !a.truncated {
		t.Fatal("the accumulator did not record that it stopped at the read limit")
	}

	want := flow.Window{From: testWindow.From, To: last}
	if got := a.covered(testWindow); got != want {
		t.Fatalf("covered = %+v, want %+v: a truncated read covers only up to the last flow it saw",
			got, want)
	}

	res, err := a.result(testWindow)
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if _, completeness := res.Connections(); completeness != flow.CompletenessDegraded {
		t.Fatalf("completeness = %s, want DEGRADED; stopping at the read limit is evidence that "+
			"traffic in this window was not looked at, and evidence of loss is not the same as "+
			"no evidence either way", completeness)
	}
}

// TestATruncatedReadWithNoMeasuredCutStaysUnknown 记的是一条**裁定**，
// 不只是当前行为。
//
// 截断了、却说不出停在哪一刻（喂进来的 flow 一条都不带时间戳，或最后一条
// 正落在窗口边界上）时，覆盖窗口留空、完整度 UNKNOWN。
//
// 为什么不是 DEGRADED —— spec §5 把"读取达到上限被截断"列为正面的缺口证据：
// 本层能表达"已知漏了"的通道**只有覆盖窗口**，而说得出 DEGRADED 就得说出一个
// 具体的截止时刻。没量到那一刻还硬填一个，就是编一个我们没有的事实，与
// nodeGap 那一支拒绝硬把时间窗截短是同一条理由（也与 TestANodeGapLeavesCoverageUnknown
// 同一个裁定）。两档对下游一视同仁 —— 都不是完整，都必须降级 —— 差别只在
// 排查线索，而"编一个截止时刻"会让那条线索指向一段从没被测量过的时间。
//
// 要让这一支说得出 DEGRADED，需要的是一个与覆盖窗口无关的证据位（截断标志），
// 且它必须跟着落库才能活过一次往返；那是一次 schema 变更，不在本次修复范围内。
func TestATruncatedReadWithNoMeasuredCutStaysUnknown(t *testing.T) {
	a := newAccumulator()

	if n := fillToReadLimit(t, a, false, time.Time{}); n != maxFlowsPerIngest {
		t.Fatalf("observe reported the read limit after %d flows, want %d", n, maxFlowsPerIngest)
	}
	if !a.truncated {
		t.Fatal("the accumulator did not record that it stopped at the read limit")
	}

	if got := a.covered(testWindow); got != (flow.Window{}) {
		t.Fatalf("covered = %+v, want zero: no flow carried a time, so the moment the read stopped "+
			"was never measured, and a covered window filled in without one is invented", got)
	}

	res, err := a.result(testWindow)
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	_, completeness := res.Connections()
	if completeness == flow.CompletenessComplete {
		t.Fatalf("completeness = COMPLETE after a truncated read")
	}
	if completeness != flow.CompletenessUnknown {
		t.Fatalf("completeness = %s, want UNKNOWN", completeness)
	}
}

// resolverFunc 让一个函数满足 secrets.Resolver。
type resolverFunc func(ctx context.Context, ref string) ([]byte, error)

func (f resolverFunc) Resolve(ctx context.Context, ref string) ([]byte, error) { return f(ctx, ref) }

// TestErrorsCarryNoRelayAddress 钉的是安全规范 §19/§22：relay 地址与传输层
// 细节属于内网拓扑，不得随错误文本走出去。
func TestErrorsCarryNoRelayAddress(t *testing.T) {
	src, err := New(Config{Address: "10.96.0.10:4245", Timeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = src.Ingest(context.Background(), "cluster-a", testWindow)
	if err == nil {
		t.Skip("relay address unexpectedly reachable in this environment")
	}
	for _, leak := range []string{"10.96.0.10", "4245", "dial tcp"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("error text %q leaks %q", err.Error(), leak)
		}
	}
}
