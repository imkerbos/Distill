package snapshotstore

import (
	"context"
	"fmt"
	"time"

	"github.com/imkerbos/Distill/internal/reconcile"
	"github.com/imkerbos/Distill/internal/replay"
)

// maxReconciliationSubjects 是一次对账允许落库的主体数上限。
//
// 超过即整次拒绝，不截断：一份被截掉一半的分歧清单在界面上读起来是
// "只有这几个 workload 有问题"，而那句话没有人算过。与其余几处上限
// 同一方向 —— 宁可答不出，不可自信地少答。
const maxReconciliationSubjects = 5000

// ReconciliationRun 是一次对账的落库形态。
type ReconciliationRun struct {
	ClusterID  string
	RunID      string
	WindowFrom time.Time
	WindowTo   time.Time
	ComputedAt time.Time
	// SourceReports 说的是来源到底报不报判定。
	//
	// 与计数分开存：为 false 时所有连接都在 SOURCE_SILENT 里，一致率不成立 ——
	// 落下来才分得开"那段时间对不了账"与"那段时间一致率很低"。
	SourceReports bool
	Report        reconcile.Report
}

// SaveReconciliation 落一次对账结果。
//
// **只存聚合，不存每一条分歧的原文**（design doc 2026-08-25 §3.4）：分歧量
// 随流量增长，全存会让这张表成为账单上最大的一项；而回答"哪个 workload
// 有问题"只需要计数。要看具体是哪几条连接，去 /flows 按同一个窗口查 ——
// 那份数据本来就在，不必抄第二份。
//
// 整次一个事务：主体明细与总计对不上的那一刻，界面会显示一个各行之和
// 不等于总数的表格，而没有人会相信一份自相矛盾的报告。
func (s *Store) SaveReconciliation(ctx context.Context, run ReconciliationRun) (err error) {
	if run.ClusterID == "" || run.RunID == "" {
		return fmt.Errorf("snapshotstore: reconciliation run needs a cluster and a run id")
	}
	if len(run.Report.BySubject) > maxReconciliationSubjects {
		return fmt.Errorf(
			"snapshotstore: reconciliation of cluster %s carries %d subjects, over the %d limit; "+
				"refusing rather than storing part of it",
			run.ClusterID, len(run.Report.BySubject), maxReconciliationSubjects)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("snapshotstore: begin reconciliation: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	c := run.Report.Overall
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO reconciliation_run
		   (cluster_id, run_id, window_from, window_to, computed_at, source_reports,
		    agree, source_silent, platform_unknown, over_permissive, under_permissive)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.ClusterID, run.RunID, run.WindowFrom.UTC(), run.WindowTo.UTC(),
		run.ComputedAt.UTC(), run.SourceReports,
		c[reconcile.ClassAgree], c[reconcile.ClassSourceSilent], c[reconcile.ClassPlatformUnknown],
		c[reconcile.ClassOverPermissive], c[reconcile.ClassUnderPermissive],
	); err != nil {
		return fmt.Errorf("snapshotstore: insert reconciliation run: %w", err)
	}

	for _, sub := range run.Report.BySubject {
		sc := sub.Counts
		if _, err = tx.ExecContext(ctx,
			`INSERT INTO reconciliation_subject
			   (cluster_id, run_id, namespace, workload, agree, over_permissive, under_permissive)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			run.ClusterID, run.RunID, sub.Subject.Namespace, sub.Subject.Workload,
			sc[reconcile.ClassAgree], sc[reconcile.ClassOverPermissive],
			sc[reconcile.ClassUnderPermissive],
		); err != nil {
			return fmt.Errorf("snapshotstore: insert reconciliation subject: %w", err)
		}
	}

	// 样本与计数同一个事务：一份有比率却没有证据的记录，正是这张表要消除的
	// 那种状态。条数上限由 reconcile.MaxSamplesPerClass 与主体数共同封顶，
	// 主体数上面已经拦过，因此这里不再单独设限。
	//
	// seq 由本地计数给出，不用连接内容做键：同一条连接在一个窗口里可能出现
	// 多次，而"同一个端口反复出现"恰恰是最要紧的那个信号，用五元组做键会把
	// 它折叠掉。
	seq := map[string]int{}
	for _, smp := range run.Report.Samples {
		k := smp.Subject.Namespace + "\x00" + smp.Subject.Workload + "\x00" + string(smp.Class)
		if _, err = tx.ExecContext(ctx,
			`INSERT INTO reconciliation_sample
			   (cluster_id, run_id, namespace, workload, class, seq,
			    src_ip, dst_ip, protocol, dst_port, occurred_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			run.ClusterID, run.RunID, smp.Subject.Namespace, smp.Subject.Workload,
			string(smp.Class), seq[k],
			smp.Flow.Source.IP, smp.Flow.Dest.IP, string(smp.Flow.Protocol),
			smp.Flow.Port, smp.Flow.Timestamp.UTC(),
		); err != nil {
			return fmt.Errorf("snapshotstore: insert reconciliation sample: %w", err)
		}
		seq[k]++
	}
	return tx.Commit()
}

// ReconciliationSamples 读回一次对账留下的分歧样本。
//
// 按 (class, namespace, workload, seq) 排序，与 reconcile.Run 的输出次序同源：
// UNDER_PERMISSIVE 排在前面 —— 它是唯一能造成生产阻断的那一类，而翻页翻不到
// 的证据等于没有。
func (s *Store) ReconciliationSamples(
	ctx context.Context, clusterID, runID string,
) ([]reconcile.Sample, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT namespace, workload, class, src_ip, dst_ip, protocol, dst_port, occurred_at
		   FROM reconciliation_sample
		  WHERE cluster_id = ? AND run_id = ?
		  ORDER BY class = ? DESC, namespace, workload, seq`,
		clusterID, runID, string(reconcile.ClassUnderPermissive))
	if err != nil {
		return nil, fmt.Errorf("snapshotstore: read reconciliation samples: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []reconcile.Sample
	for rows.Next() {
		var (
			smp      reconcile.Sample
			class    string
			protocol string
		)
		if err := rows.Scan(
			&smp.Subject.Namespace, &smp.Subject.Workload, &class,
			&smp.Flow.Source.IP, &smp.Flow.Dest.IP, &protocol,
			&smp.Flow.Port, &smp.Flow.Timestamp,
		); err != nil {
			return nil, fmt.Errorf("snapshotstore: scan reconciliation sample: %w", err)
		}
		smp.Class = reconcile.Class(class)
		smp.Flow.Protocol = replay.Protocol(protocol)
		out = append(out, smp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("snapshotstore: iterate reconciliation samples: %w", err)
	}
	return out, nil
}

// ReconciliationTrend 是一致率随时间的走向，最近的在前。
//
// **回答的是「在变好还是变坏」**，那才是这个指标唯一有行动含义的读法：
// 一次 97% 无所谓，从 100% 掉到 97% 才是信号。
func (s *Store) ReconciliationTrend(
	ctx context.Context, clusterID string, limit int,
) ([]ReconciliationRun, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT run_id, window_from, window_to, computed_at, source_reports,
		        agree, source_silent, platform_unknown, over_permissive, under_permissive
		   FROM reconciliation_run
		  WHERE cluster_id = ?
		  ORDER BY window_from DESC
		  LIMIT ?`, clusterID, limit)
	if err != nil {
		return nil, fmt.Errorf("snapshotstore: read reconciliation trend: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []ReconciliationRun
	for rows.Next() {
		var (
			r                                   ReconciliationRun
			agree, silent, unknown, over, under int
			reports                             bool
		)
		if err := rows.Scan(&r.RunID, &r.WindowFrom, &r.WindowTo, &r.ComputedAt, &reports,
			&agree, &silent, &unknown, &over, &under); err != nil {
			return nil, fmt.Errorf("snapshotstore: scan reconciliation trend: %w", err)
		}
		r.ClusterID = clusterID
		r.SourceReports = reports
		r.Report = reconcile.Report{Overall: reconcile.Counts{
			reconcile.ClassAgree:           agree,
			reconcile.ClassSourceSilent:    silent,
			reconcile.ClassPlatformUnknown: unknown,
			reconcile.ClassOverPermissive:  over,
			reconcile.ClassUnderPermissive: under,
		}}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("snapshotstore: iterate reconciliation trend: %w", err)
	}
	return out, nil
}

// maxCoverageWindows 是算覆盖时允许读入的摄入窗口数上限。
//
// 超过即报错，不截断：一份被截掉一半的窗口清单算出来的覆盖偏小，看起来
// 安全，但它会让一个观测其实已经充分的集群永远过不了门禁，而排查方向
// （"为什么还差 3 天"）根本指不到这里。
//
// 15 分钟一个窗口时，10 万行是约三年。真到了这个量级，要解决的是保留策略，
// 不是让门禁悄悄用一份残缺的清单作答。
const maxCoverageWindows = 100000

// ObservedCoverage 返回这个集群的观测**跨度**与实际**覆盖**。
//
// 两个数不是一回事，而门禁要的是后者（design doc 2026-08-25 §5）：
// 一个集群 90 天前摄入过一次、之后采集器坏了 89 天、今天恢复，跨度是 90 天，
// 真正被观测到的只有两分钟。拿跨度与业务周期比会把它放行 —— 而它恰恰是最
// 不该放行的那一类：一份基于两分钟观测的 default-deny，下发的是"这两分钟
// 之外的一切都拦掉"。
//
// **重叠的窗口只算一次。** 重跑一段历史、或者两条采集链路窗口交叠，都会让
// 同一段时间被记两次；直接把窗口时长求和会让覆盖凭空变长。区间合并在 Go 里
// 做而不是在 SQL 里：这段逻辑要能被单独证伪，而一段写在 SQL 字符串里的
// 区间合并只能靠真库整体验证。
//
// **只看成功的摄入**：一次失败的摄入证明不了那段时间
// 被观测过。
//
// 第三个返回值为 false 表示一次成功摄入都没有。
func (s *Store) ObservedCoverage(
	ctx context.Context, clusterID string,
) (span, covered time.Duration, ok bool, err error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT window_start, window_end FROM flow_ingest_run
		  WHERE cluster_id = ? AND status <> ?
		  ORDER BY window_start LIMIT ?`,
		clusterID, string(IngestFailed), maxCoverageWindows+1)
	if err != nil {
		return 0, 0, false, fmt.Errorf("snapshotstore: read observed coverage: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type window struct{ from, to time.Time }
	var windows []window
	for rows.Next() {
		var w window
		if err := rows.Scan(&w.from, &w.to); err != nil {
			return 0, 0, false, fmt.Errorf("snapshotstore: scan observed coverage: %w", err)
		}
		// 起止颠倒的行不参与计算：它会让合并后的区间变长，而"覆盖变长"
		// 正是这个函数最不能出的错。这种行不该存在（写入侧校验过），
		// 真出现了就当它不存在，而不是让它把结论往放行的方向推。
		if !w.to.After(w.from) {
			continue
		}
		windows = append(windows, w)
	}
	if err := rows.Err(); err != nil {
		return 0, 0, false, fmt.Errorf("snapshotstore: iterate observed coverage: %w", err)
	}
	if len(windows) > maxCoverageWindows {
		return 0, 0, false, fmt.Errorf(
			"snapshotstore: cluster %s holds more than %d ingest windows; "+
				"refusing to answer coverage from a truncated list",
			clusterID, maxCoverageWindows)
	}
	if len(windows) == 0 {
		return 0, 0, false, nil
	}

	// 已按 window_start 排好序，一次线性合并即可。
	merged := 0 * time.Second
	cur := windows[0]
	for _, w := range windows[1:] {
		if w.from.After(cur.to) {
			merged += cur.to.Sub(cur.from)
			cur = w
			continue
		}
		// 相接或重叠：并进当前区间。相邻（w.from == cur.to）也合并 ——
		// 两个首尾相接的 15 分钟窗口就是连续观测的 30 分钟，中间没有缝。
		if w.to.After(cur.to) {
			cur.to = w.to
		}
	}
	merged += cur.to.Sub(cur.from)

	return windows[len(windows)-1].to.Sub(windows[0].from), merged, true, nil
}
