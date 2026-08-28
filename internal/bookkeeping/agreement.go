package bookkeeping

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/imkerbos/Distill/internal/collectrun"
	"github.com/imkerbos/Distill/internal/reconcile"
	"github.com/imkerbos/Distill/internal/snapshotstore"
	"github.com/imkerbos/Distill/internal/store"
)

// Reconciler 算一次对账。
type Reconciler interface {
	Reconciliation(
		ctx context.Context, clusterID string, window store.TimeWindow,
	) (store.ReconciliationReport, error)
}

// ReconcileStore 落一次对账结果。
type ReconcileStore interface {
	SaveReconciliation(ctx context.Context, run snapshotstore.ReconciliationRun) error
}

// AgreementRecorder 把一个窗口的一致率落进对账历史（design doc 2026-08-25 §3）。
//
// **在记账里做，而不是等有人打开界面**：一致率唯一有行动含义的读法是
// 「在变好还是变坏」，而那需要一条连续的时间线。等人来看才算的指标，
// 只会在出事之后才第一次有数据。
type AgreementRecorder struct {
	reconciler Reconciler
	store      ReconcileStore
	logger     *slog.Logger
}

// NewAgreementRecorder 构造一个对账记账器。
func NewAgreementRecorder(r Reconciler, s ReconcileStore, logger *slog.Logger) AgreementRecorder {
	return AgreementRecorder{reconciler: r, store: s, logger: logger}
}

// RecordWindow 算一次窗口 [from, to) 的一致率并落库。
//
// **来源不报判定时只落这一轮本身，丢掉逐主体明细。** conntrack 接入下每个
// 主体都是 SOURCE_SILENT，几百行长得一模一样、零信息量，而这一轮每几分钟就跑
// 一次 —— 那是纯粹的账单（CLAUDE.md §5）。
//
// 但**这一轮必须落**：趋势页在没有 run 时返回空数组，而代码自己的注释写着
// 空趋势读起来是「这个集群还没对过账」（httpapi/fleet_handler.go）。落下来它
// 才说得出「这条接入方式对不了账」，而那两句对下一步做什么的含义完全不同。
func (a AgreementRecorder) RecordWindow(
	ctx context.Context, clusterID string, from, to time.Time,
) error {
	rep, err := a.reconciler.Reconciliation(ctx, clusterID,
		store.TimeWindow{From: from, To: to})
	if err != nil {
		return fmt.Errorf(
			"bookkeeping: cannot reconcile cluster %s against the enforcement plane; "+
				"this window will be missing from the agreement-rate trend: %w", clusterID, err)
	}
	runID, err := collectrun.NewRunID()
	if err != nil {
		return fmt.Errorf("bookkeeping: cannot mint a reconciliation run id: %w", err)
	}

	report := rep.Report
	if !rep.SourceReportsVerdicts {
		report.BySubject = nil
		report.Samples = nil
	}
	if err := a.store.SaveReconciliation(ctx, snapshotstore.ReconciliationRun{
		ClusterID: clusterID, RunID: runID,
		WindowFrom: from, WindowTo: to, ComputedAt: nowUTC(),
		SourceReports: rep.SourceReportsVerdicts,
		Report:        report,
	}); err != nil {
		return fmt.Errorf("bookkeeping: cannot store the reconciliation of cluster %s: %w",
			clusterID, err)
	}

	c := rep.Report.Overall
	rate, ok := c.AgreementRate()
	switch {
	case !rep.SourceReportsVerdicts:
		// 不是"一致率低"，是"这条接入方式对不了账"。说出来，否则趋势里
		// 一串没有数字的点会被读成平台坏了。
		a.logger.Info("this cluster's flow source reports no verdicts; "+
			"there is no agreement rate to compute",
			"cluster", clusterID, "windowFrom", from, "windowTo", to)
	case !ok:
		a.logger.Info("no connection in this window was both judged by the platform and "+
			"reported by the enforcement plane; the agreement rate is not computable here",
			"cluster", clusterID)
	default:
		a.logger.Info("reconciled platform verdicts against the enforcement plane",
			"cluster", clusterID, "agreementRate", rate,
			"underPermissive", c[reconcile.ClassUnderPermissive],
			"overPermissive", c[reconcile.ClassOverPermissive])
	}
	return nil
}

// nowUTC 留一个函数是为了让"算出来的时刻"只有一个来源。
func nowUTC() time.Time { return time.Now().UTC() }
