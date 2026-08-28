package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/imkerbos/Distill/internal/bookkeeping"
	"github.com/imkerbos/Distill/internal/policygen"
	"github.com/imkerbos/Distill/internal/snapshotstore"
	"github.com/imkerbos/Distill/internal/store"
)

// previewer 是记证据要用的那一样能力：把当前窗口的候选集算出来。
type previewer interface {
	PolicyPreviewAtGranularity(
		ctx context.Context, clusterID, namespace string, window store.TimeWindow,
		granularity policygen.Granularity,
	) (store.PolicyPreview, error)
}

// evidenceStore 落一次证据。
type evidenceStore interface {
	RecordRuleEvidence(
		ctx context.Context, clusterID string, from, to time.Time,
		complete bool, rules []snapshotstore.RuleEvidence,
	) error
}

// recordEvidenceOnce 把这个窗口里生成出来的候选规则记进证据表
// （design doc 2026-08-25 §4）。
//
// **在采集器里做，而不是等有人打开预览页**：证据要跨窗口累积，而累积只能
// 发生在每一个窗口被观测到的那一刻。等人来看才记，记下的就只有他看过的
// 那几个窗口 —— 而"这条规则我们观察了多久"恰恰要的是他没看的那些。
//
// 记账本身在 internal/evidence 里，与推送式接入共用一份实现：拉取式与推送式
// 的差别只在"窗口从哪来"，各写一份记账会让同一条规则在两种接入形态下累积出
// 不同的证据，而操作者无从知道该信哪一个。
//
// 失败不影响采集，但必须留下日志：证据缺一个窗口的后果是那条规则显得比
// 实际更弱 —— 安全方向，但会让操作者永远等不到它变得可信。
func recordEvidenceOnce(
	ctx context.Context, clusterID string, from, to time.Time,
	p previewer, s evidenceStore, logger *slog.Logger,
) {
	if err := bookkeeping.NewRecorder(p, s, logger).RecordWindow(ctx, clusterID, from, to); err != nil {
		logger.Warn("could not record rule evidence for this window",
			"cluster", clusterID, "err", err)
	}
}
