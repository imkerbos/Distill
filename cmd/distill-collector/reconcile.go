package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/imkerbos/Distill/internal/collectrun"
	"github.com/imkerbos/Distill/internal/reconcile"
	"github.com/imkerbos/Distill/internal/snapshotstore"
	"github.com/imkerbos/Distill/internal/store"
)

// reconciler 是采集器需要的那两样能力：算一次对账、把它落下来。
//
// 收成接口而不是直接吃两个具体类型：这一步是"采完之后顺手做的一件事"，
// 它不该成为把整个读栈拖进采集器的理由。
type reconciler interface {
	Reconciliation(
		ctx context.Context, clusterID string, window store.TimeWindow,
	) (store.ReconciliationReport, error)
	previewer
}

// reconcileStore 落一次对账结果。
type reconcileStore interface {
	SaveReconciliation(ctx context.Context, run snapshotstore.ReconciliationRun) error
}

// reconcileOnce 在一次摄入之后算一次一致率并落库
// （design doc 2026-08-25 §3）。
//
// **在采集器里做，而不是等有人打开界面**：一致率唯一有行动含义的读法是
// 「在变好还是变坏」，而那需要一条连续的时间线。等人来看才算的指标，
// 只会在出事之后才第一次有数据。
//
// **失败不影响这一轮采集**：资产与流量本身是有价值的，对账是它们之上的
// 一层解释。失败必须留下日志 —— 静默失败会让趋势里出现一个没人解释的
// 空洞，而空洞会被读成"那段时间没问题"。
func reconcileOnce(
	ctx context.Context, clusterID string, from, to time.Time,
	r reconciler, s reconcileStore, logger *slog.Logger,
) {
	// 收两个时刻而不是 store.TimeWindow：调用点那边 store 是个参数名，
	// 包名被遮住了。让边界收窄成两个 time.Time，调用方不必为了拼一个
	// 结构体去改自己的命名。
	window := store.TimeWindow{From: from, To: to}
	rep, err := r.Reconciliation(ctx, clusterID, window)
	if err != nil {
		logger.Warn("could not reconcile platform verdicts against the enforcement plane; "+
			"this window will be missing from the agreement-rate trend",
			"cluster", clusterID)
		return
	}
	runID, err := collectrun.NewRunID()
	if err != nil {
		logger.Warn("could not mint a reconciliation run id", "cluster", clusterID)
		return
	}
	if err := s.SaveReconciliation(ctx, snapshotstore.ReconciliationRun{
		ClusterID: clusterID, RunID: runID,
		WindowFrom: from, WindowTo: to, ComputedAt: time.Now().UTC(),
		SourceReports: rep.SourceReportsVerdicts,
		Report:        rep.Report,
	}); err != nil {
		logger.Warn("could not store the reconciliation result", "cluster", clusterID)
		return
	}

	c := rep.Report.Overall
	rate, ok := c.AgreementRate()
	switch {
	case !rep.SourceReportsVerdicts:
		// 不是"一致率低"，是"这条接入方式对不了账"。说出来，否则趋势里
		// 一串没有数字的点会被读成平台坏了。
		logger.Info("this cluster's flow source reports no verdicts; there is no agreement rate to compute",
			"cluster", clusterID)
	case !ok:
		logger.Info("no connection in this window was both judged by the platform and reported by the "+
			"enforcement plane; the agreement rate is not computable here", "cluster", clusterID)
	default:
		logger.Info("reconciled platform verdicts against the enforcement plane",
			"cluster", clusterID, "agreementRate", rate,
			"underPermissive", c[reconcile.ClassUnderPermissive], "overPermissive", c[reconcile.ClassOverPermissive])
	}
}
