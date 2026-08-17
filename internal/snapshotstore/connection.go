package snapshotstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/imkerbos/Distill/internal/flow"
	"github.com/imkerbos/Distill/internal/identity"
)

// maxIngestConnections 是一次摄入允许落库的连接条数上限。
//
// 量级依据见 spec §7：500 Pod、平均 8 个对端的集群，一小时窗口约 4000 条。
// 上限取到它的一个数量级以上，是为了让"来源突然吐回百万行"这件事以一次
// 明确的拒绝出现，而不是以一次跑了二十分钟的事务出现（安全规范 §24）。
const maxIngestConnections = 50_000

// maxWindowRuns 是一次窗口查询允许命中的摄入运行数上限。
//
// 调用方可以传一个跨十年的窗口，那时命中的运行数没有上界。
const maxWindowRuns = 2_000

// maxWindowConnections 是一次窗口查询返回的连接条数上限。
//
// 超出即报错，**不截断**：截断会让一个繁忙的窗口读起来比实际安静，而"看起来
// 更安静"正是这个平台唯一不能出的那种错（spec §2）。宁可让调用方缩小窗口。
const maxWindowConnections = 50_000

// IngestStatus 是一次流量摄入的运行状态，封闭枚举。
//
// PARTIAL 必须与 OK 区分（spec §6）：一次丢了半个窗口的摄入若报成 OK，
// 下游会把一份缺了连接的流量当作完整事实，于是覆盖那些连接的规则被判
// "无流量、可收紧"。零值不合法 —— 一次说不出自己成没成的摄入，不该落库。
type IngestStatus string

const (
	// IngestOK 表示这次摄入按请求跑完了。
	IngestOK IngestStatus = "OK"
	// IngestPartial 表示跑起来了但没跑完，例如窗口只覆盖了一部分。
	IngestPartial IngestStatus = "PARTIAL"
	// IngestFailed 表示这次摄入根本没能拿到数据。
	IngestFailed IngestStatus = "FAILED"
)

// Valid 判断该状态是否已登记。
func (s IngestStatus) Valid() bool {
	switch s {
	case IngestOK, IngestPartial, IngestFailed:
		return true
	default:
		return false
	}
}

// IngestErrorReason 是摄入失败的原因，封闭枚举，不用自由文本
// （CLAUDE.md §3）：统计口径只认枚举，自由文本进了统计就再也对不上账。
//
// 取值与 collection_run 的失败原因**不共用**，理由与两张表分开同源
// （spec §6）：资产采集失败多半是 RBAC，流量摄入失败多半是 relay 或配额。
type IngestErrorReason string

const (
	// IngestErrorNone 表示这次摄入真的连上了来源，成败看 status。
	IngestErrorNone IngestErrorReason = ""
	// IngestErrorUnreachable 表示连不上来源（relay 挂了、地址被出站守卫拒绝）。
	IngestErrorUnreachable IngestErrorReason = "UNREACHABLE"
	// IngestErrorUnauthorized 表示来源拒绝了我们的凭据。
	IngestErrorUnauthorized IngestErrorReason = "UNAUTHORIZED"
	// IngestErrorQuotaExhausted 表示来源侧配额用尽。
	IngestErrorQuotaExhausted IngestErrorReason = "QUOTA_EXHAUSTED"
	// IngestErrorTimeout 表示在拿到完整窗口之前超时。
	IngestErrorTimeout IngestErrorReason = "TIMEOUT"
	// IngestErrorOther 是上面几种之外的失败。新增一类具体原因时要同步改
	// 这个枚举、迁移里的注释与统计口径。
	IngestErrorOther IngestErrorReason = "OTHER"
)

// Valid 判断该原因是否已登记。空串合法，它表示"没有失败原因"。
func (r IngestErrorReason) Valid() bool {
	switch r {
	case IngestErrorNone, IngestErrorUnreachable, IngestErrorUnauthorized,
		IngestErrorQuotaExhausted, IngestErrorTimeout, IngestErrorOther:
		return true
	default:
		return false
	}
}

// IngestRun 是一次流量摄入的运行：什么时候跑的、结果如何、看见了什么。
//
// Result 是 flow.IngestResult 而不是一串连接：一次摄入的结论不只是"看见了
// 这些"，还有"这段时间看得全不全"，两者必须一起进库，否则事实层里躺着的
// 就是一份看起来完整的观测。
type IngestRun struct {
	// ClusterID 是这次摄入针对的集群。
	ClusterID string
	// RunID 是这次摄入的标识。
	RunID string
	// StartedAt / FinishedAt 是运维时间线上的两个点，与观测窗口无关。
	StartedAt  time.Time
	FinishedAt time.Time
	// Status 是这次摄入的状态。
	Status IngestStatus
	// ErrorReason 仅在这次摄入失败时非空。
	ErrorReason IngestErrorReason
	// Result 是这次摄入看到的连接与它的完整度证据。
	Result flow.IngestResult
}

// SaveIngest 在单个事务里写入一次流量摄入的运行记录与它观测到的连接。
//
// 单事务而非逐表提交，理由与 Save 相同：先提交运行、后提交连接会让读取方在
// 两次提交之间看到一个"摄入成功、但一条连接也没有"的窗口，而这个状态与
// "这段时间真的没有流量"无法区分 —— 那正是本轮第一条要守住的分界。
//
// **不写 completeness 列。** 完整度不是可以被填写的字段，而是证据的函数
// （internal/flow 里它连 setter 都没有）。这里只落证据：请求窗口、来源报告的
// 实际覆盖、采样率、丢弃数。读取时交回 flow 包重算，于是"库里写着 COMPLETE
// 但没人给过依据"这个状态构造不出来。
func (s *Store) SaveIngest(ctx context.Context, run IngestRun) (err error) {
	if err = validateIngestRun(run); err != nil {
		return err
	}

	conns, _ := run.Result.Connections()
	// 完整度在这里被丢掉不是遗漏：它由下面这几列证据完全决定，FlowWindow
	// 读回来时用同一个 flow 包重算。存一份算出来的结论反而多一个会与证据
	// 对不上的位置。
	if len(conns) > maxIngestConnections {
		return fmt.Errorf(
			"snapshotstore: ingest run %s of cluster %s carries %d connections, over the %d limit; "+
				"refusing rather than storing part of a window",
			run.RunID, run.ClusterID, len(conns), maxIngestConnections)
	}
	for i, c := range conns {
		if err = validateConnection(c); err != nil {
			return fmt.Errorf("snapshotstore: ingest run %s connection %d: %w", run.RunID, i, err)
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("snapshotstore: begin ingest: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	if err = insertIngestRun(ctx, tx, run); err != nil {
		return err
	}
	if err = insertConnections(ctx, tx, run, conns); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("snapshotstore: commit ingest: %w", err)
	}
	return nil
}

// validateIngestRun 拒绝那些落库之后就再也分辨不出来的运行。
//
// 逐条都是"两种事实会变得无法区分"，不是格式洁癖：
//   - 没有窗口的摄入说不出自己观测的是哪一段时间，它的连接落进事实层之后
//     无从按时间解释；
//   - FAILED 而没有原因的运行，在界面上与一次"摄入成功、这段时间确实没有
//     流量"的运行长得一模一样（与 SaveAbortedRun 同一条理由）；
//   - OK 而带着失败原因，是调用方自己都没想清楚这次到底成没成。
func validateIngestRun(run IngestRun) error {
	if run.ClusterID == "" {
		return fmt.Errorf("snapshotstore: ingest run has no cluster id")
	}
	if run.RunID == "" {
		return fmt.Errorf("snapshotstore: ingest run for cluster %s has no run id", run.ClusterID)
	}
	if !run.Status.Valid() {
		return fmt.Errorf("snapshotstore: ingest run %s carries an unregistered status %q",
			run.RunID, string(run.Status))
	}
	if !run.ErrorReason.Valid() {
		return fmt.Errorf("snapshotstore: ingest run %s carries an unregistered error reason %q",
			run.RunID, string(run.ErrorReason))
	}
	if run.Status == IngestFailed && run.ErrorReason == IngestErrorNone {
		return fmt.Errorf(
			"snapshotstore: refusing to record a failed ingest for %s with no reason; "+
				"it would be indistinguishable from a window that simply had no traffic",
			run.ClusterID)
	}
	if run.Status == IngestOK && run.ErrorReason != IngestErrorNone {
		return fmt.Errorf(
			"snapshotstore: ingest run %s reports OK but carries error reason %q",
			run.RunID, string(run.ErrorReason))
	}
	if !run.Result.Source().Valid() {
		return fmt.Errorf(
			"snapshotstore: ingest run %s carries an unregistered source kind %q; "+
				"a batch of connections that cannot say where it came from cannot say how complete it is",
			run.RunID, string(run.Result.Source()))
	}
	if requested, _ := run.Result.Windows(); !requested.Valid() {
		return fmt.Errorf("snapshotstore: ingest run %s has no requested window", run.RunID)
	}
	return nil
}

// validateConnection 拒绝无法按封闭枚举解释的连接。
//
// 在写入侧拒绝而不是读取侧容错：一条协议写着 "tcp6" 的连接落了库，读回来时
// 只剩两条路 —— 猜一个协议，或者让整个窗口读不出来。前者会让判定引擎按错的
// 协议匹配规则，后者让一次早就该报出来的写入错误推迟到查询时才爆。
func validateConnection(c flow.Connection) error {
	if !c.Protocol.Valid() {
		return fmt.Errorf("unregistered protocol %q", string(c.Protocol))
	}
	if c.Port < 0 || c.Port > 65535 {
		return fmt.Errorf("port %d out of range", c.Port)
	}
	if c.ObservedCount < 0 {
		return fmt.Errorf("observed count %d is negative", c.ObservedCount)
	}
	if c.Source.IP == "" || c.Dest.IP == "" {
		return fmt.Errorf("both endpoints must carry an address")
	}
	return nil
}

func insertIngestRun(ctx context.Context, tx *sql.Tx, run IngestRun) error {
	requested, covered := run.Result.Windows()

	// 覆盖窗口不合法 → 落 NULL，不拿请求窗口顶替：NULL 是"来源说不出自己
	// 覆盖了多少"，而拿请求窗口填进去等于替来源宣称它跑满了整段时间。
	var coveredStart, coveredEnd any
	if covered.Valid() {
		coveredStart, coveredEnd = covered.From, covered.To
	}

	// 采样率取不到 → NULL，**不填 1.0**（spec §5）。1.0 是一句"一条没漏"的
	// 断言，而问不出采样率的时候没有人说过这句话。丢弃数同理：NULL 是"来源
	// 不报"，0 是"来源报告一条没丢"，只有后者配参与 COMPLETE 的判定。
	var sampleRate any
	if rate, known := run.Result.SampleRate(); known {
		sampleRate = rate
	}
	var dropped any
	if n, reported := run.Result.Dropped(); reported {
		dropped = n
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO flow_ingest_run
		   (cluster_id, run_id, source_kind, window_start, window_end,
		    covered_start, covered_end, started_at, finished_at,
		    status, error_reason, sample_rate, dropped)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.ClusterID, run.RunID, string(run.Result.Source()),
		requested.From, requested.To, coveredStart, coveredEnd,
		run.StartedAt, run.FinishedAt,
		string(run.Status), string(run.ErrorReason), sampleRate, dropped); err != nil {
		return fmt.Errorf("snapshotstore: insert ingest run %s: %w", run.RunID, err)
	}
	return nil
}

func insertConnections(ctx context.Context, tx *sql.Tx, run IngestRun, conns []flow.Connection) error {
	if len(conns) == 0 {
		return nil
	}
	requested, _ := run.Result.Windows()
	rate, rateKnown := run.Result.SampleRate()
	var sampleRate any
	if rateKnown {
		sampleRate = rate
	}

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO observed_connection
		   (cluster_id, window_start, window_end, ingest_run_id, seq,
		    src_ip, src_kind, src_namespace, src_workload, src_identity_confidence,
		    dst_ip, dst_kind, dst_namespace, dst_workload, dst_identity_confidence,
		    protocol, port, observed_count, verdict_observed, source_kind, sample_rate)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("snapshotstore: prepare connection: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for i, c := range conns {
		srcSubject, srcConfidence := c.Source.Identity()
		dstSubject, dstConfidence := c.Dest.Identity()

		// 来源没报判定时落空串，与 ALLOWED 是两件事（spec §4）。这里没有
		// "默认放行"这条路：reported 为 false 时 verdict 就是空。
		verdict, reported := c.Verdict()
		stored := ""
		if reported {
			stored = string(verdict)
		}

		if _, err := stmt.ExecContext(ctx,
			run.ClusterID, requested.From, requested.To, run.RunID, i,
			c.Source.IP, srcSubject.WorkloadKind, srcSubject.Namespace,
			srcSubject.WorkloadName, string(srcConfidence),
			c.Dest.IP, dstSubject.WorkloadKind, dstSubject.Namespace,
			dstSubject.WorkloadName, string(dstConfidence),
			string(c.Protocol), c.Port, c.ObservedCount, stored,
			string(run.Result.Source()), sampleRate); err != nil {
			return fmt.Errorf("snapshotstore: insert connection %d of run %s: %w", i, run.RunID, err)
		}
	}
	return nil
}

// ErrNoIngestWindow 表示这段时间**平台根本没有摄入过流量**。
//
// 与"摄入过、这段时间确实没有连接"必须区分，理由与 ErrNoRun、与 identity 的
// NOT_COVERED / NO_DATA 完全同源（spec §2）：把"我们没在看"读成"那时没有
// 东西"，会让一条真实存在的连接被判定成不存在，覆盖它的规则被判"无流量、
// 可收紧"，最后推荐一份切断它的策略。失败方向是单向的。
//
// 做成 error 而不是结果里的一个布尔：调用方拿不到 WindowFlow，也就写不出
// "先取连接、再顺手忽略那个布尔"的代码。
var ErrNoIngestWindow = errors.New("snapshotstore: no flow ingest run covers this window")

// WindowFlow 是一段时间窗内观测到的连接，以及这段观测有多可信。
//
// 字段不导出，两者只能一起取走：一个"只取连接、不看完整度"的调用方就是把
// 一份可能缺了连接的观测当成完整事实在用（spec §2）。
type WindowFlow struct {
	conns        []flow.Connection
	completeness flow.Completeness
}

// Connections 返回窗口内观测到的连接，以及这批观测的完整度。
func (w WindowFlow) Connections() ([]flow.Connection, flow.Completeness) {
	return w.conns, w.completeness
}

// FlowWindow 读出一个集群在给定时间窗内被观测到的连接。
//
// 没有任何摄入运行与这个窗口相交时返回 ErrNoIngestWindow，**不是一份空的
// WindowFlow** —— 那两件事在这里必须分开。
//
// 每一条查询都带 cluster_id（CLAUDE.md §4）：不同集群 Pod CIDR 可能重叠，
// 漏掉它会把另一个集群的连接算进这个窗口，且不报错。窗口一律带范围，
// 主键前缀 (cluster_id, window_start) 负责裁剪，不做全表扫描（spec §7）。
func (s *Store) FlowWindow(ctx context.Context, clusterID string, window flow.Window) (WindowFlow, error) {
	if clusterID == "" {
		return WindowFlow{}, fmt.Errorf("snapshotstore: flow window query has no cluster id")
	}
	if !window.Valid() {
		return WindowFlow{}, fmt.Errorf("snapshotstore: flow window query has no valid window")
	}

	runs, err := s.readIngestEvidence(ctx, clusterID, window)
	if err != nil {
		return WindowFlow{}, err
	}
	if len(runs) == 0 {
		return WindowFlow{}, fmt.Errorf(
			"snapshotstore: cluster %s window [%s,%s): %w",
			clusterID, window.From.UTC().Format(time.RFC3339), window.To.UTC().Format(time.RFC3339),
			ErrNoIngestWindow)
	}

	completeness, err := windowCompleteness(window, runs)
	if err != nil {
		return WindowFlow{}, err
	}
	conns, err := s.readConnections(ctx, clusterID, window)
	if err != nil {
		return WindowFlow{}, err
	}
	return WindowFlow{conns: conns, completeness: completeness}, nil
}

// ingestEvidence 是一次摄入留在库里的完整度证据。
//
// 只有证据，没有结论：完整度由 flow 包按同一套规则重算，本包不复制那段判定。
type ingestEvidence struct {
	source          flow.SourceKind
	requested       flow.Window
	covered         flow.Window
	sampleRate      float64
	sampleRateKnown bool
	dropped         uint64
	droppedReported bool
}

// completeness 把证据交回 flow 包重算。
func (e ingestEvidence) completeness() (flow.Completeness, error) {
	res, err := flow.NewIngestResult(e.source, e.requested, e.covered, nil)
	if err != nil {
		return "", fmt.Errorf("snapshotstore: rebuild ingest result: %w", err)
	}
	if e.sampleRateKnown {
		res = res.WithSampleRate(e.sampleRate)
	}
	if e.droppedReported {
		res = res.WithDropped(e.dropped)
	}
	_, c := res.Connections()
	return c, nil
}

// readIngestEvidence 取出与窗口相交的全部摄入运行。
//
// 相交而非完全包含：一个跨两次摄入的查询窗口，两次都只覆盖它的一半，若按
// 包含筛就一条也选不出来，于是这个窗口读起来变成"从没摄入过"。
func (s *Store) readIngestEvidence(
	ctx context.Context, clusterID string, window flow.Window,
) ([]ingestEvidence, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT source_kind, window_start, window_end, covered_start, covered_end,
		        sample_rate, dropped
		   FROM flow_ingest_run
		  WHERE cluster_id = ? AND window_start < ? AND window_end > ?
		  ORDER BY window_start
		  LIMIT ?`,
		clusterID, window.To, window.From, maxWindowRuns+1)
	if err != nil {
		return nil, fmt.Errorf("snapshotstore: read ingest runs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []ingestEvidence
	for rows.Next() {
		var (
			e            ingestEvidence
			source       string
			coveredStart sql.NullTime
			coveredEnd   sql.NullTime
			rate         sql.NullFloat64
			dropped      sql.NullInt64
		)
		if err := rows.Scan(&source, &e.requested.From, &e.requested.To,
			&coveredStart, &coveredEnd, &rate, &dropped); err != nil {
			return nil, fmt.Errorf("snapshotstore: scan ingest run: %w", err)
		}
		e.source = flow.SourceKind(source)
		// 两端都在才算一个覆盖窗口：只有一端的行说不出区间，那是未知，
		// 而未知在 flow 里落到 UNKNOWN，不落到"覆盖满了"。
		if coveredStart.Valid && coveredEnd.Valid {
			e.covered = flow.Window{From: coveredStart.Time, To: coveredEnd.Time}
		}
		if rate.Valid {
			e.sampleRate, e.sampleRateKnown = rate.Float64, true
		}
		if dropped.Valid && dropped.Int64 >= 0 {
			e.dropped, e.droppedReported = uint64(dropped.Int64), true
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("snapshotstore: iterate ingest runs: %w", err)
	}
	if len(out) > maxWindowRuns {
		return nil, fmt.Errorf(
			"snapshotstore: window of cluster %s matches more than %d ingest runs; narrow the window",
			clusterID, maxWindowRuns)
	}
	return out, nil
}

// windowCompleteness 把若干次摄入的完整度合成这个查询窗口的完整度。
//
// 取最差的一档而非平均或多数：一次 DEGRADED 的摄入意味着这个窗口里确实有
// 连接没被看见，另外三次 COMPLETE 并不能把它补回来。
//
// 除各次自身的完整度之外，还看这些摄入合起来有没有盖住被查询的窗口：查一个
// 一小时的窗口、只有一次覆盖了其中五分钟的 COMPLETE 摄入，答 COMPLETE 就是
// 把"我们只看了五分钟"说成"这一小时没漏"。
//
// 覆盖窗口未知的运行不参与缺口判定：它没说自己盖了多少，把它算成缺口等于
// 宣称"我们知道这里漏了"，而我们只是不知道 —— 那一档由它自身的 UNKNOWN 承担。
func windowCompleteness(queried flow.Window, runs []ingestEvidence) (flow.Completeness, error) {
	worst := flow.CompletenessComplete
	gapKnowable := true
	covered := make([]flow.Window, 0, len(runs))

	for _, e := range runs {
		c, err := e.completeness()
		if err != nil {
			return "", err
		}
		worst = worseCompleteness(worst, c)
		if e.covered.Valid() {
			covered = append(covered, e.covered)
			continue
		}
		gapKnowable = false
	}

	if gapKnowable && !windowsCover(covered, queried) {
		worst = worseCompleteness(worst, flow.CompletenessDegraded)
	}
	return worst, nil
}

// worseCompleteness 返回两档里更差的那个：DEGRADED > UNKNOWN > COMPLETE。
//
// DEGRADED 压过 UNKNOWN 而不是相反：已知漏了是一条比"不知道漏没漏"更硬的
// 结论，合并时保留更硬的那条。对下游判定而言两者一视同仁，都必须降级。
func worseCompleteness(a, b flow.Completeness) flow.Completeness {
	rank := func(c flow.Completeness) int {
		switch c {
		case flow.CompletenessDegraded:
			return 2
		case flow.CompletenessUnknown:
			return 1
		default:
			return 0
		}
	}
	if rank(b) > rank(a) {
		return b
	}
	return a
}

// windowsCover 报告若干个窗口合起来有没有盖满 queried。
//
// 逐段推进游标而不是比较首尾：两次摄入分别覆盖窗口的头和尾、中间空一个小时
// 的情形，比首尾会得出"盖满了"。
func windowsCover(windows []flow.Window, queried flow.Window) bool {
	if !queried.Valid() {
		return false
	}
	sorted := slices.Clone(windows)
	slices.SortFunc(sorted, func(a, b flow.Window) int { return a.From.Compare(b.From) })

	cursor := queried.From
	for _, w := range sorted {
		if w.From.After(cursor) {
			return false // 游标与下一段之间有缺口
		}
		if w.To.After(cursor) {
			cursor = w.To
		}
		if !cursor.Before(queried.To) {
			return true
		}
	}
	return !cursor.Before(queried.To)
}

// readConnections 读出与窗口相交的连接。
//
// 直接按 (cluster_id, window_start) 前缀查，不 join 运行表：连接表按 spec §7
// 的量级是百万行起步，一次范围查询上再叠一个 join 没有收益。窗口列在写入时
// 就冗余了一份，正是为了这条查询。
func (s *Store) readConnections(
	ctx context.Context, clusterID string, window flow.Window,
) ([]flow.Connection, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT src_ip, src_kind, src_namespace, src_workload, src_identity_confidence,
		        dst_ip, dst_kind, dst_namespace, dst_workload, dst_identity_confidence,
		        protocol, port, observed_count, verdict_observed
		   FROM observed_connection
		  WHERE cluster_id = ? AND window_start < ? AND window_end > ?
		  ORDER BY window_start, ingest_run_id, seq
		  LIMIT ?`,
		clusterID, window.To, window.From, maxWindowConnections+1)
	if err != nil {
		return nil, fmt.Errorf("snapshotstore: read connections: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []flow.Connection{}
	for rows.Next() {
		var (
			srcIP, srcKind, srcNS, srcWorkload, srcConfidence string
			dstIP, dstKind, dstNS, dstWorkload, dstConfidence string
			protocol, verdict                                 string
			port                                              int32
			observedCount                                     int
		)
		if err := rows.Scan(&srcIP, &srcKind, &srcNS, &srcWorkload, &srcConfidence,
			&dstIP, &dstKind, &dstNS, &dstWorkload, &dstConfidence,
			&protocol, &port, &observedCount, &verdict); err != nil {
			return nil, fmt.Errorf("snapshotstore: scan connection: %w", err)
		}

		src, err := rebuildEndpoint(srcIP, srcKind, srcNS, srcWorkload, srcConfidence)
		if err != nil {
			return nil, err
		}
		dst, err := rebuildEndpoint(dstIP, dstKind, dstNS, dstWorkload, dstConfidence)
		if err != nil {
			return nil, err
		}
		c := flow.Connection{
			Source:        src,
			Dest:          dst,
			Protocol:      flow.Protocol(protocol),
			Port:          port,
			ObservedCount: observedCount,
		}
		if !c.Protocol.Valid() {
			return nil, fmt.Errorf(
				"snapshotstore: stored connection of cluster %s carries an unregistered protocol %q",
				clusterID, protocol)
		}
		// 空串表示来源没报判定，那时**不调用** WithVerdict —— 空与放行是两件
		// 事（spec §4），而一次"顺手当成 ALLOWED"就是把现网的拒绝写成允许。
		if verdict != "" {
			v := flow.Verdict(verdict)
			if !v.Valid() {
				return nil, fmt.Errorf(
					"snapshotstore: stored connection of cluster %s carries an unregistered verdict %q",
					clusterID, verdict)
			}
			c = c.WithVerdict(v)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("snapshotstore: iterate connections: %w", err)
	}
	if len(out) > maxWindowConnections {
		return nil, fmt.Errorf(
			"snapshotstore: window of cluster %s holds more than %d connections; narrow the window",
			clusterID, maxWindowConnections)
	}
	return out, nil
}

// rebuildEndpoint 把落库的一端还原成 flow.Endpoint。
//
// 走 WithIdentity 而不是直接塞字段：那个方法在 outcome 不是 RESOLVED 时会把
// 主体丢掉，于是"AMBIGUOUS 却带着一个具体负载名"这种取值在读取路径上同样
// 构造不出来 —— 与写入侧同一条约束的另一半。
func rebuildEndpoint(ip, kind, namespace, workload, confidence string) (flow.Endpoint, error) {
	outcome := identity.Outcome(confidence)
	if !outcome.Valid() {
		// 拒绝而不是降级成 NO_DATA：一个本包不认识的可信度是"枚举加了取值、
		// 这里没跟上"的迹象，静默改写会让那次疏漏永远看不出来。
		return flow.Endpoint{}, fmt.Errorf(
			"snapshotstore: stored connection carries an identity confidence this code has not registered: %q",
			confidence)
	}
	return flow.Endpoint{IP: ip}.WithIdentity(identity.Identity{
		Namespace:    namespace,
		WorkloadKind: kind,
		WorkloadName: workload,
	}, outcome), nil
}
