package snapshotstore_test

import (
	"errors"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/flow"
	"github.com/imkerbos/Distill/internal/identity"
	"github.com/imkerbos/Distill/internal/snapshotstore"
)

// 一个固定的一小时观测窗口。
var (
	winFrom = time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)
	winTo   = winFrom.Add(time.Hour)
	window  = flow.Window{From: winFrom, To: winTo}
)

// connection 造一条连接，两端都带 IP，身份留空（由调用方按需附上）。
func connection(srcIP, dstIP string, port int32) flow.Connection {
	return flow.Connection{
		Source:        flow.Endpoint{IP: srcIP},
		Dest:          flow.Endpoint{IP: dstIP},
		Protocol:      flow.ProtocolTCP,
		Port:          port,
		ObservedCount: 3,
	}
}

// resolvedIdentity 是一个解析成功的主体。
func resolvedIdentity(workload string) identity.Identity {
	return identity.Identity{
		Namespace:    "shop",
		PodName:      workload + "-abc",
		PodUID:       "3c9d2b1a-0000-4000-8000-00000000000a",
		WorkloadKind: "Deployment",
		WorkloadName: workload,
	}
}

// completeIngest 是一次三项证据齐备、明确证明了"没漏"的摄入。
func completeIngest(t *testing.T, conns ...flow.Connection) flow.IngestResult {
	t.Helper()
	res, err := flow.NewIngestResult(flow.SourceHubble, window, window, conns)
	if err != nil {
		t.Fatalf("NewIngestResult() error = %v", err)
	}
	return res.WithSampleRate(1).WithDropped(0)
}

func ingestRun(clusterID, runID string, status snapshotstore.IngestStatus, res flow.IngestResult) snapshotstore.IngestRun {
	return snapshotstore.IngestRun{
		ClusterID:  clusterID,
		RunID:      runID,
		StartedAt:  winFrom,
		FinishedAt: winTo,
		Status:     status,
		Result:     res,
	}
}

func mustSaveIngest(t *testing.T, s *snapshotstore.Store, run snapshotstore.IngestRun) {
	t.Helper()
	if err := s.SaveIngest(t.Context(), run); err != nil {
		t.Fatalf("SaveIngest(%s) error = %v", run.RunID, err)
	}
}

func mustReadWindow(t *testing.T, s *snapshotstore.Store, clusterID string, w flow.Window) snapshotstore.WindowFlow {
	t.Helper()
	got, err := s.FlowWindow(t.Context(), clusterID, w)
	if err != nil {
		t.Fatalf("FlowWindow(%s) error = %v", clusterID, err)
	}
	return got
}

// 一个窗口没有任何摄入记录，不得表现为「这段时间没有流量」（spec §2）。
//
// 与 C 轮 NOT_COVERED / NO_DATA 同源：把「我们没在看」读成「那时没有东西」，
// 会让一条真实存在的连接被判定成不存在，覆盖它的规则被判「无流量、可收紧」，
// 最后推荐一份切断它的策略。失败方向是单向的。
//
// 两个方向都断言：没有摄入必须报 ErrNoIngestWindow，而一次真的跑过、确实
// 一条连接都没看见的摄入必须**读得出来**、且不报这个错。只断言前者的测试，
// 在一个「永远报 ErrNoIngestWindow」的实现下照样是绿的。
func TestNoIngestRunIsNotZeroTraffic(t *testing.T) {
	s, _ := newTestStore(t)

	_, err := s.FlowWindow(t.Context(), clusterA, window)
	if !errors.Is(err, snapshotstore.ErrNoIngestWindow) {
		t.Fatalf("FlowWindow() on a window with no ingest run: error = %v, want ErrNoIngestWindow", err)
	}

	// 同一个窗口，这次真的摄入过，只是一条连接也没有。
	mustSaveIngest(t, s, ingestRun(clusterA, "ingest-empty", snapshotstore.IngestOK, completeIngest(t)))

	got := mustReadWindow(t, s, clusterA, window)
	conns, completeness := got.Connections()
	if len(conns) != 0 {
		t.Errorf("Connections() = %d rows, want 0", len(conns))
	}
	if completeness != flow.CompletenessComplete {
		t.Errorf("completeness = %q, want COMPLETE: an ingest that proved it missed nothing "+
			"must be readable as such, otherwise 'we looked and saw nothing' cannot be told "+
			"apart from 'we did not look'", completeness)
	}
}

// identity_confidence 逐条落库，不是一个全局值（spec §4）。
//
// 一个窗口里既有解析成功的连接也有解析不了的。汇总成一次运行一个数字，会让
// 「90% 可信」掩盖掉那 10% 恰好落在关键路径上的连接。
func TestConfidenceIsPerConnectionNotPerRun(t *testing.T) {
	s, _ := newTestStore(t)

	resolved := connection("10.4.0.9", "10.4.0.21", 8080)
	resolved.Source = resolved.Source.WithIdentity(resolvedIdentity("web"), identity.OutcomeResolved)
	resolved.Dest = resolved.Dest.WithIdentity(resolvedIdentity("api"), identity.OutcomeResolved)

	// 同一次摄入里的第二条：源解析不出唯一主体，目的那一刻根本没有 Pod。
	unresolved := connection("10.4.0.9", "10.128.0.5", 9090)
	unresolved.Source = unresolved.Source.WithIdentity(resolvedIdentity("web"), identity.OutcomeAmbiguous)
	unresolved.Dest = unresolved.Dest.WithIdentity(identity.Identity{}, identity.OutcomeNotCovered)

	mustSaveIngest(t, s, ingestRun(clusterA, "ingest-mixed", snapshotstore.IngestOK,
		completeIngest(t, resolved, unresolved)))

	conns, _ := mustReadWindow(t, s, clusterA, window).Connections()
	if len(conns) != 2 {
		t.Fatalf("Connections() = %d rows, want 2", len(conns))
	}

	type endpointFact struct {
		outcome  identity.Outcome
		workload string
	}
	got := make([]endpointFact, 0, 4)
	for _, c := range conns {
		srcSubject, srcOutcome := c.Source.Identity()
		dstSubject, dstOutcome := c.Dest.Identity()
		got = append(got,
			endpointFact{srcOutcome, srcSubject.WorkloadName},
			endpointFact{dstOutcome, dstSubject.WorkloadName})
	}

	want := []endpointFact{
		{identity.OutcomeResolved, "web"},
		{identity.OutcomeResolved, "api"},
		// AMBIGUOUS 的那一端不得带回一个具体负载：任选一个仍然查得出结果、
		// 仍然不报错，而错的那次没有任何症状。
		{identity.OutcomeAmbiguous, ""},
		{identity.OutcomeNotCovered, ""},
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("endpoint %d = %+v, want %+v; identity confidence must be stored per "+
				"connection endpoint, not summarised into one value for the run", i, got[i], want[i])
		}
	}

	// 单看每一条还不够：一个「整次运行存一个可信度」的实现会让两条连接读回
	// 同一个值，而那正是本测试要挡的形状。
	if got[0].outcome == got[2].outcome {
		t.Errorf("both connections came back with confidence %q; one window holds both "+
			"resolved and unresolvable connections, and a single figure lets the resolved "+
			"ones vouch for the rest", got[0].outcome)
	}
}

// 部分成功的摄入不得读回成完整（spec §6）。
//
// 一次丢了半个窗口的摄入若报成完整，判定引擎会看到一个比实际更安静的集群，
// 于是覆盖那些没被看见的连接的规则被判「无流量、可收紧」。
func TestAPartialIngestIsStoredAsPartial(t *testing.T) {
	s, db := newTestStore(t)

	// 请求一小时，来源只覆盖到前半小时。
	half := flow.Window{From: winFrom, To: winFrom.Add(30 * time.Minute)}
	res, err := flow.NewIngestResult(flow.SourceHubble, window, half,
		[]flow.Connection{connection("10.4.0.9", "10.4.0.21", 8080)})
	if err != nil {
		t.Fatalf("NewIngestResult() error = %v", err)
	}
	res = res.WithSampleRate(1).WithDropped(0)

	if _, written := res.Connections(); written != flow.CompletenessDegraded {
		t.Fatalf("completeness before storing = %q, want DEGRADED", written)
	}

	run := ingestRun(clusterA, "ingest-half", snapshotstore.IngestPartial, res)
	run.ErrorReason = snapshotstore.IngestErrorTimeout
	mustSaveIngest(t, s, run)

	_, readBack := mustReadWindow(t, s, clusterA, window).Connections()
	if readBack != flow.CompletenessDegraded {
		t.Errorf("completeness after the round trip = %q, want DEGRADED; a run that lost half "+
			"its window must read back as having lost it", readBack)
	}

	if got := scanString(t, db,
		`SELECT status FROM flow_ingest_run WHERE cluster_id = ? AND run_id = 'ingest-half'`,
		clusterA); got != string(snapshotstore.IngestPartial) {
		t.Errorf("stored status = %q, want PARTIAL", got)
	}
	if got := scanString(t, db,
		`SELECT error_reason FROM flow_ingest_run WHERE cluster_id = ? AND run_id = 'ingest-half'`,
		clusterA); got != string(snapshotstore.IngestErrorTimeout) {
		t.Errorf("stored error_reason = %q, want TIMEOUT", got)
	}
}

// 来源没报判定，与来源报告「放行」必须不同（spec §4）。
//
// 把没报当放行，会让一批实际被网络拒掉的连接变成正常业务流量的证据，
// 进而生成一条允许规则 —— 把现网的拒绝变成允许。
func TestAnAbsentVerdictComesBackAbsent(t *testing.T) {
	s, _ := newTestStore(t)

	silent := connection("10.4.0.9", "10.4.0.21", 8080)
	allowed := connection("10.4.0.9", "10.4.0.22", 8080).WithVerdict(flow.VerdictAllowed)
	denied := connection("10.4.0.9", "10.4.0.23", 8080).WithVerdict(flow.VerdictDenied)

	mustSaveIngest(t, s, ingestRun(clusterA, "ingest-verdicts", snapshotstore.IngestOK,
		completeIngest(t, silent, allowed, denied)))

	conns, _ := mustReadWindow(t, s, clusterA, window).Connections()
	if len(conns) != 3 {
		t.Fatalf("Connections() = %d rows, want 3", len(conns))
	}

	type verdictFact struct {
		verdict  flow.Verdict
		reported bool
	}
	want := []verdictFact{
		{"", false},
		{flow.VerdictAllowed, true},
		{flow.VerdictDenied, true},
	}
	for i, c := range conns {
		v, reported := c.Verdict()
		if (verdictFact{v, reported}) != want[i] {
			t.Errorf("connection %d verdict = (%q, %t), want (%q, %t); an absent verdict must "+
				"survive the round trip as absent, not arrive back as permitted",
				i, v, reported, want[i].verdict, want[i].reported)
		}
	}
}

// 只摄入了窗口一小段的查询不得答 COMPLETE。
//
// 查一个一小时的窗口、只有一次覆盖了其中五分钟的完整摄入，答 COMPLETE 就是
// 把「我们只看了五分钟」说成「这一小时没漏」。这一条与逐次摄入自身的完整度
// 是两件事：那次摄入对它自己的五分钟确实是完整的。
func TestAWindowOnlyPartlyIngestedIsNotComplete(t *testing.T) {
	s, _ := newTestStore(t)

	fiveMin := flow.Window{From: winFrom, To: winFrom.Add(5 * time.Minute)}
	res, err := flow.NewIngestResult(flow.SourceHubble, fiveMin, fiveMin, nil)
	if err != nil {
		t.Fatalf("NewIngestResult() error = %v", err)
	}
	res = res.WithSampleRate(1).WithDropped(0)
	if _, own := res.Connections(); own != flow.CompletenessComplete {
		t.Fatalf("the five-minute ingest is %q on its own, want COMPLETE", own)
	}

	mustSaveIngest(t, s, ingestRun(clusterA, "ingest-5m", snapshotstore.IngestOK, res))

	if _, got := mustReadWindow(t, s, clusterA, window).Connections(); got != flow.CompletenessDegraded {
		t.Errorf("completeness of the hour = %q, want DEGRADED; only five minutes of it were ingested", got)
	}
}

// 一次说不出自己失败原因的摄入不得落库。
//
// 它在界面上与一次「摄入成功、这段时间确实没有流量」的运行长得一模一样，
// 而那正是本轮要守住的那条分界（与 SaveAbortedRun 同一条理由）。
func TestAFailedIngestWithoutAReasonIsRefused(t *testing.T) {
	s, db := newTestStore(t)

	run := ingestRun(clusterA, "ingest-failed", snapshotstore.IngestFailed, completeIngest(t))
	if err := s.SaveIngest(t.Context(), run); err == nil {
		t.Fatal("SaveIngest() accepted a failed ingest with no reason, want it refused")
	}
	if n := scanInt(t, db,
		`SELECT COUNT(*) FROM flow_ingest_run WHERE cluster_id = ?`, clusterA); n != 0 {
		t.Errorf("flow_ingest_run holds %d rows after the refusal, want 0", n)
	}

	run.ErrorReason = snapshotstore.IngestErrorUnreachable
	mustSaveIngest(t, s, run)
}

// 每一条查询都带 cluster_id（CLAUDE.md §4）：两个集群的 Pod CIDR 可以重叠，
// 漏掉它会把另一个集群的连接算进这个窗口，且不报错。
func TestFlowWindowIsScopedToOneCluster(t *testing.T) {
	s, _ := newTestStore(t)

	mustSaveIngest(t, s, ingestRun(clusterA, "ingest-a", snapshotstore.IngestOK,
		completeIngest(t, connection("10.4.0.9", "10.4.0.21", 8080))))
	mustSaveIngest(t, s, ingestRun(clusterB, "ingest-b", snapshotstore.IngestOK,
		completeIngest(t,
			connection("10.4.0.9", "10.4.0.21", 8080),
			connection("10.4.0.9", "10.4.0.22", 9090))))

	if conns, _ := mustReadWindow(t, s, clusterA, window).Connections(); len(conns) != 1 {
		t.Errorf("cluster %s window holds %d connections, want 1", clusterA, len(conns))
	}
	if conns, _ := mustReadWindow(t, s, clusterB, window).Connections(); len(conns) != 2 {
		t.Errorf("cluster %s window holds %d connections, want 2", clusterB, len(conns))
	}
}

// 采样率取不到时落 NULL，读回来仍然是「未知」，不是 1.0（spec §5）。
//
// 填 1.0 等于宣称「一条没漏」，而那是一句没人说过的话。这里断言的是那句话
// 没有在存储层被悄悄补上：一次连采样率都问不出来的摄入，完整度必须是 UNKNOWN。
func TestAnUnknownSampleRateStaysUnknown(t *testing.T) {
	s, db := newTestStore(t)

	res, err := flow.NewIngestResult(flow.SourceHubble, window, window,
		[]flow.Connection{connection("10.4.0.9", "10.4.0.21", 8080)})
	if err != nil {
		t.Fatalf("NewIngestResult() error = %v", err)
	}
	mustSaveIngest(t, s, ingestRun(clusterA, "ingest-norate", snapshotstore.IngestOK, res))

	if n := scanInt(t, db,
		`SELECT COUNT(*) FROM flow_ingest_run
		  WHERE cluster_id = ? AND run_id = 'ingest-norate' AND sample_rate IS NULL`,
		clusterA); n != 1 {
		t.Errorf("sample_rate IS NULL matched %d rows, want 1; an unknown sampling rate must "+
			"not be stored as 1.0", n)
	}
	if _, got := mustReadWindow(t, s, clusterA, window).Connections(); got != flow.CompletenessUnknown {
		t.Errorf("completeness = %q, want UNKNOWN", got)
	}
}
