package snapshotstore_test

import (
	"database/sql"
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

// storedConnection 是 observed_connection 的一行，逐列读回来。
//
// 摊成一个结构体而不是几个抽查的标量：下面那条用例要比的是**每一列**，
// 而漏掉一列的断言就漏掉了整整一类错误（见该用例的注释）。
type storedConnection struct {
	clusterID     string
	windowStart   time.Time
	windowEnd     time.Time
	runID         string
	seq           int
	srcIP         string
	srcKind       string
	srcNamespace  string
	srcWorkload   string
	srcConfidence string
	dstIP         string
	dstKind       string
	dstNamespace  string
	dstWorkload   string
	dstConfidence string
	protocol      string
	port          int32
	observedCount int64
	verdict       string
	sourceKind    string
	sampleRate    sql.NullFloat64
}

func readStoredConnection(t *testing.T, db *sql.DB, clusterID, runID string, seq int) storedConnection {
	t.Helper()
	var got storedConnection
	err := db.QueryRow(
		`SELECT cluster_id, window_start, window_end, ingest_run_id, seq,
		        src_ip, src_kind, src_namespace, src_workload, src_identity_confidence,
		        dst_ip, dst_kind, dst_namespace, dst_workload, dst_identity_confidence,
		        protocol, port, observed_count, verdict_observed, source_kind, sample_rate
		   FROM observed_connection
		  WHERE cluster_id = ? AND ingest_run_id = ? AND seq = ?`,
		clusterID, runID, seq).Scan(
		&got.clusterID, &got.windowStart, &got.windowEnd, &got.runID, &got.seq,
		&got.srcIP, &got.srcKind, &got.srcNamespace, &got.srcWorkload, &got.srcConfidence,
		&got.dstIP, &got.dstKind, &got.dstNamespace, &got.dstWorkload, &got.dstConfidence,
		&got.protocol, &got.port, &got.observedCount, &got.verdict, &got.sourceKind, &got.sampleRate)
	if err != nil {
		t.Fatalf("read stored connection (run %s seq %d): %v", runID, seq, err)
	}
	return got
}

// 一条连接落库之后必须**逐列**读得回来。
//
// 评审用三个探针改坏了 insertConnections 的列映射 —— 把目的地址写进源列、
// 把端口换成常量、把观测次数换成常量 —— 整个测试套件与 make test-integration
// 全绿。两端调了个个儿的连接不是一个存储缺陷，是一条**编造出来的事实**：
// 判定引擎把这一层读作"存在过的连接"，于是它拿反方向的流量去判一条规则，
// 推荐就跟着错（spec §2 的单向错误）。
//
// 因此这里逐列比对，不做抽查：三个探针各只动了一列，漏掉任何一列的断言就
// 漏掉了整整一类错误。两条路一起走：先直查表钉住列映射本身，再走 FlowWindow
// 钉住读取路径 —— 只走后者的话，写入与读取同时把两端调换的对称错误会互相
// 抵消，一次全绿的往返底下躺着一条方向反了的连接。
//
// 每一列的取值都刻意互不相同（两端的地址、kind、namespace、负载名都不一样，
// 端口不是 0/1，次数不是 0/1，采样率不是 1.0），于是任何一次"换成常量"或
// "拿另一列顶替"都有一条断言接得住。
func TestAStoredConnectionComesBackColumnForColumn(t *testing.T) {
	s, db := newTestStore(t)

	const runID = "ingest-roundtrip"

	c := flow.Connection{
		Source:        flow.Endpoint{IP: "10.4.0.9"},
		Dest:          flow.Endpoint{IP: "10.4.0.21"},
		Protocol:      flow.ProtocolSCTP,
		Port:          8443,
		ObservedCount: 7,
	}
	srcSubject := identity.Identity{
		Namespace: "shop", PodName: "web-abc",
		WorkloadKind: "Deployment", WorkloadName: "web",
	}
	dstSubject := identity.Identity{
		Namespace: "payment", PodName: "ledger-0",
		WorkloadKind: "StatefulSet", WorkloadName: "ledger",
	}
	c.Source = c.Source.WithIdentity(srcSubject, identity.OutcomeResolved)
	c.Dest = c.Dest.WithIdentity(dstSubject, identity.OutcomeResolved)
	c = c.WithVerdict(flow.VerdictDenied)

	// 第二条只为把两列可信度分开：两条都 RESOLVED 时，src/dst 两列对调
	// 看不出来。这一条的两端可信度不同，且都不带主体。
	unresolved := connection("10.4.0.10", "10.4.0.22", 9090)
	unresolved.Source = unresolved.Source.WithIdentity(srcSubject, identity.OutcomeAmbiguous)
	unresolved.Dest = unresolved.Dest.WithIdentity(dstSubject, identity.OutcomeNotCovered)

	res, err := flow.NewIngestResult(flow.SourceHubble, window, window, []flow.Connection{c, unresolved})
	if err != nil {
		t.Fatalf("NewIngestResult() error = %v", err)
	}
	// 采样率取 0.5 而不是 1：1.0 与"把这一列写死成 1.0"分不开。
	res = res.WithSampleRate(0.5).WithDropped(0)
	mustSaveIngest(t, s, ingestRun(clusterA, runID, snapshotstore.IngestOK, res))

	want := storedConnection{
		clusterID:     clusterA,
		windowStart:   winFrom,
		windowEnd:     winTo,
		runID:         runID,
		seq:           0,
		srcIP:         "10.4.0.9",
		srcKind:       "Deployment",
		srcNamespace:  "shop",
		srcWorkload:   "web",
		srcConfidence: string(identity.OutcomeResolved),
		dstIP:         "10.4.0.21",
		dstKind:       "StatefulSet",
		dstNamespace:  "payment",
		dstWorkload:   "ledger",
		dstConfidence: string(identity.OutcomeResolved),
		protocol:      string(flow.ProtocolSCTP),
		port:          8443,
		observedCount: 7,
		verdict:       string(flow.VerdictDenied),
		sourceKind:    string(flow.SourceHubble),
		sampleRate:    sql.NullFloat64{Float64: 0.5, Valid: true},
	}
	got := readStoredConnection(t, db, clusterA, runID, 0)

	// 时刻单独比：DATETIME(6) 读回来的 time.Time 与写进去的可能带不同的
	// Location，== 会因此判不等，而那不是列映射错了。
	if !got.windowStart.Equal(want.windowStart) || !got.windowEnd.Equal(want.windowEnd) {
		t.Errorf("stored window = [%s, %s), want [%s, %s)",
			got.windowStart, got.windowEnd, want.windowStart, want.windowEnd)
	}
	got.windowStart, got.windowEnd = want.windowStart, want.windowEnd

	if got != want {
		t.Errorf("stored row = %+v\nwant             %+v\n"+
			"a connection must land in the columns it was given: a swapped endpoint, a constant "+
			"port or a constant observation count is not a storage defect but a fabricated fact, "+
			"and the evaluator reads this layer as connections that existed", got, want)
	}

	// 两列可信度必须分得开，否则"src/dst 对调"这一类错误没有任何症状。
	second := readStoredConnection(t, db, clusterA, runID, 1)
	if second.srcConfidence != string(identity.OutcomeAmbiguous) ||
		second.dstConfidence != string(identity.OutcomeNotCovered) {
		t.Errorf("second row confidences = (%q, %q), want (AMBIGUOUS, NOT_COVERED)",
			second.srcConfidence, second.dstConfidence)
	}

	// 再走一遍读取路径：列映射对了，还要保证读回来的连接就是写进去的那一条。
	conns, _ := mustReadWindow(t, s, clusterA, window).Connections()
	if len(conns) != 2 {
		t.Fatalf("Connections() = %d rows, want 2", len(conns))
	}
	back := conns[0]
	if back.Source.IP != c.Source.IP || back.Dest.IP != c.Dest.IP {
		t.Errorf("round trip endpoints = %s -> %s, want %s -> %s",
			back.Source.IP, back.Dest.IP, c.Source.IP, c.Dest.IP)
	}
	if back.Protocol != c.Protocol || back.Port != c.Port || back.ObservedCount != c.ObservedCount {
		t.Errorf("round trip = %s/%d count %d, want %s/%d count %d",
			back.Protocol, back.Port, back.ObservedCount, c.Protocol, c.Port, c.ObservedCount)
	}
	if v, reported := back.Verdict(); !reported || v != flow.VerdictDenied {
		t.Errorf("round trip verdict = (%q, %t), want (DENIED, true)", v, reported)
	}

	// 身份两端各自比。PodName 不落库（评审 M-2）：这里把那个缺口钉成一条
	// 断言而不是让它悄悄过去 —— 对账一律用 workload 而非 pod（CLAUDE.md §4），
	// 但类型上仍然承诺了一个往返里会丢的字段。
	for _, tc := range []struct {
		side string
		ep   flow.Endpoint
		want identity.Identity
	}{
		{"source", back.Source, identity.Identity{
			Namespace: "shop", WorkloadKind: "Deployment", WorkloadName: "web"}},
		{"dest", back.Dest, identity.Identity{
			Namespace: "payment", WorkloadKind: "StatefulSet", WorkloadName: "ledger"}},
	} {
		subject, outcome := tc.ep.Identity()
		if outcome != identity.OutcomeResolved {
			t.Errorf("%s outcome = %q, want RESOLVED", tc.side, outcome)
		}
		if subject != tc.want {
			t.Errorf("%s identity = %+v, want %+v (PodName is not persisted, see review M-2)",
				tc.side, subject, tc.want)
		}
	}
}

// validateConnection 的**调用点**，不是守卫本身。
//
// 本项目已经出现十九次"守卫被单独测到、却没有任何东西证明调用方还在调用
// 它"。把 SaveIngest 里那个 for 循环删掉，这一条必须变红 —— 与两行之外的
// TestAFailedIngestWithoutAReasonIsRefused 同一个形状。
//
// 每一条都是"落了库就再也分辨不出来"：协议写着 ICMP 的连接读回来时只剩
// 猜一个协议或让整个窗口读不出来两条路；端口越界会在判定层匹配到别的规则
// 上；负观测次数是一次没人观测过的次数；缺地址的一端在事实层里指向不了
// 任何东西。写入侧拒绝，比读取侧容错早一步，也早在这些行被当成事实之前。
func TestAConnectionThatCannotBeExplainedIsRefused(t *testing.T) {
	s, db := newTestStore(t)

	// 每条用例一个自己的 run id：共用一个的话，第一条留下的行会让后面几条
	// 撞主键而"报错"，于是那几条即便守卫没跑也照样绿 —— 一个测不出东西的
	// 断言比没有断言更糟。
	bad := []struct {
		name    string
		runID   string
		breakIt func(flow.Connection) flow.Connection
	}{
		{"unregistered protocol", "ingest-bad-proto", func(c flow.Connection) flow.Connection {
			c.Protocol = flow.Protocol("ICMP")
			return c
		}},
		{"negative port", "ingest-bad-port-neg", func(c flow.Connection) flow.Connection {
			c.Port = -1
			return c
		}},
		{"port above the range", "ingest-bad-port-high", func(c flow.Connection) flow.Connection {
			c.Port = 65536
			return c
		}},
		{"negative observed count", "ingest-bad-count", func(c flow.Connection) flow.Connection {
			c.ObservedCount = -1
			return c
		}},
		{"source without an address", "ingest-bad-src", func(c flow.Connection) flow.Connection {
			c.Source = flow.Endpoint{}
			return c
		}},
		{"dest without an address", "ingest-bad-dst", func(c flow.Connection) flow.Connection {
			c.Dest = flow.Endpoint{}
			return c
		}},
	}

	for _, tc := range bad {
		name := tc.name
		run := ingestRun(clusterA, tc.runID, snapshotstore.IngestOK,
			completeIngest(t, tc.breakIt(connection("10.4.0.9", "10.4.0.21", 8080))))
		if err := s.SaveIngest(t.Context(), run); err == nil {
			t.Errorf("%s: SaveIngest() accepted it, want the connection refused at the write side", name)
		}
		// 运行行也不得留下：拒绝必须发生在事务之前或被整体回滚，否则库里
		// 躺着一次"摄入成功、却一条连接都没有"的运行 —— 与"这段时间真的
		// 没有流量"分不开。
		if n := scanInt(t, db,
			`SELECT COUNT(*) FROM flow_ingest_run WHERE cluster_id = ?`, clusterA); n != 0 {
			t.Errorf("%s: flow_ingest_run holds %d rows after the refusal, want 0", name, n)
		}
		if n := scanInt(t, db,
			`SELECT COUNT(*) FROM observed_connection WHERE cluster_id = ?`, clusterA); n != 0 {
			t.Errorf("%s: observed_connection holds %d rows after the refusal, want 0", name, n)
		}
	}

	// 反方向：一条解释得了的连接必须落得进去。少了这一半，一个"永远拒绝"
	// 的实现照样让上面全绿。
	mustSaveIngest(t, s, ingestRun(clusterA, "ingest-good", snapshotstore.IngestOK,
		completeIngest(t, connection("10.4.0.9", "10.4.0.21", 8080))))
	if n := scanInt(t, db,
		`SELECT COUNT(*) FROM observed_connection WHERE cluster_id = ?`, clusterA); n != 1 {
		t.Errorf("observed_connection holds %d rows after a valid ingest, want 1", n)
	}
}

// 重复推同一个 run_id 不是错误，是同一次摄入又说了一遍。
//
// agent 跑在 CronJob 里，网络抖动重试是常态。撞主键报一个通用失败会让 agent
// 拿到 500 然后原样重试，而每一次都会得到同样的结果 —— 与 ErrRunExists 那条
// 是同一个道理，只是对象换成了摄入。
//
// **已存的那一份不动**：覆盖等于让后到的推送改写历史，而历史正是这个平台
// 用来解释「那时候是什么样」的东西（CLAUDE.md §4）。
func TestReingestingTheSameRunIsRecognisedNotClobbered(t *testing.T) {
	s, db := newTestStore(t)

	first := ingestRun(clusterA, "ingest-dup", snapshotstore.IngestOK,
		completeIngest(t, connection("10.4.0.9", "10.4.0.21", 8080)))
	mustSaveIngest(t, s, first)

	// 第二次带着**不同的**连接：认出重复的实现什么都不会写，而一个
	// 「先删后插」的实现会让窗口里变成两条 9090。
	second := ingestRun(clusterA, "ingest-dup", snapshotstore.IngestOK,
		completeIngest(t, connection("10.4.0.9", "10.4.0.99", 9090)))
	err := s.SaveIngest(t.Context(), second)
	if !errors.Is(err, snapshotstore.ErrIngestRunExists) {
		t.Fatalf("SaveIngest() error = %v, want ErrIngestRunExists; a retrying agent would "+
			"see a server failure and retry forever", err)
	}

	if n := scanInt(t, db,
		`SELECT COUNT(*) FROM flow_ingest_run WHERE cluster_id = ? AND run_id = ?`,
		clusterA, "ingest-dup"); n != 1 {
		t.Errorf("flow_ingest_run holds %d rows for this run, want 1", n)
	}
	conns, _ := mustReadWindow(t, s, clusterA, window).Connections()
	if len(conns) != 1 {
		t.Fatalf("window holds %d connections, want 1 — the retry rewrote history", len(conns))
	}
	if conns[0].Dest.IP != "10.4.0.21" {
		t.Errorf("stored destination = %q, want 10.4.0.21: the second push overwrote the first",
			conns[0].Dest.IP)
	}
}

// 超过条数上限要报成一个**认得出来的**拒绝，不是一次通用失败。
//
// 边界层要靠它区分「调用方一次要得太多」与「平台坏了」：前者的处置是缩短
// 窗口，后者是去查平台。塌成一个通用失败，agent 拿到 500 之后会原样重试。
func TestTooManyConnectionsIsARecognisableRefusal(t *testing.T) {
	s, db := newTestStore(t)

	conns := make([]flow.Connection, 0, 50_001)
	for i := 0; i < 50_001; i++ {
		conns = append(conns, connection("10.4.0.9", "10.4.0.21", int32(1024+i%40000)))
	}
	err := s.SaveIngest(t.Context(), ingestRun(clusterA, "ingest-huge",
		snapshotstore.IngestOK, completeIngest(t, conns...)))
	if !errors.Is(err, snapshotstore.ErrTooManyConnections) {
		t.Fatalf("SaveIngest() error = %v, want ErrTooManyConnections", err)
	}
	// 拒绝是整份的，不是截断：留下半个窗口比留下空窗口更糟 —— 那半份
	// 看起来是一段完整的观测。
	if n := scanInt(t, db,
		`SELECT COUNT(*) FROM observed_connection WHERE cluster_id = ?`, clusterA); n != 0 {
		t.Errorf("observed_connection holds %d rows after the refusal, want 0", n)
	}
}
