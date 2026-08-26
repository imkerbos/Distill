package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/imkerbos/Distill/internal/flow"
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
// **按 WORKLOAD 粒度记**：证据挂在规则指纹上，而指纹由规则内容决定；
// namespace 粒度是同一批规则折叠之后的产物，两套粒度混着记会让同一条放行
// 在两个指纹下各积累一半证据。
//
// 失败不影响采集，但必须留下日志：证据缺一个窗口的后果是那条规则显得比
// 实际更弱 —— 安全方向，但会让操作者永远等不到它变得可信。
func recordEvidenceOnce(
	ctx context.Context, clusterID string, from, to time.Time,
	p previewer, s evidenceStore, logger *slog.Logger,
) {
	pv, err := p.PolicyPreviewAtGranularity(ctx, clusterID, "",
		store.TimeWindow{From: from, To: to}, policygen.GranularityWorkload)
	if err != nil {
		logger.Warn("could not generate candidates for evidence accounting; "+
			"this window will be missing from every rule's evidence",
			"cluster", clusterID)
		return
	}

	var rules []snapshotstore.RuleEvidence
	for _, c := range pv.Candidates {
		for _, r := range c.Rules {
			// 被否决的规则也记：操作者取消确认之后，那条规则的证据不该
			// 从零开始重数 —— 它一直在被观测，只是没有被采纳。
			rules = append(rules, snapshotstore.RuleEvidence{
				Fingerprint: r.Fingerprint,
				Namespace:   c.Namespace,
				Workload:    c.Workload,
				// FlowCount 是这条规则在**这个窗口**里的观测次数；
				// 跨窗口的累计由落库层做。
				Observations: int64(r.FlowCount),
			})
		}
	}
	// 完整度取预览回显的那一个，不在这里另算一遍：屏幕上的完整度与证据里
	// 记下的完整度必须是同一次计算的两种呈现（同 PolicyPreview 那条理由）。
	complete := pv.WindowCompleteness == flow.CompletenessComplete
	if err := s.RecordRuleEvidence(ctx, clusterID, from, to, complete, rules); err != nil {
		logger.Warn("could not record rule evidence for this window",
			"cluster", clusterID, "rules", len(rules))
		return
	}
	logger.Info("recorded rule evidence",
		"cluster", clusterID, "rules", len(rules),
		// 完整度进日志：一串"记了 75 条"的日志读起来像证据在稳步积累，
		// 而如果每一轮都证明不了自己没漏，积累起来的强度不是那个意思。
		"windowComplete", complete)
}
