// Package bookkeeping 把一个观测窗口之后要做的两件派生记账放在一处：
// 证据记账（每条候选规则观察了多少个窗口）与对账（平台判定与执行面是否一致）。
//
// 两件事共用同一段调度：列出集群、取它最近一次摄入窗口、问"这个窗口记过没有"、
// 没记过才做。**共用是刻意的** —— 各写一份的话，两边的"记过没有"迟早会漂成
// 两种判据，而漂了之后两个数字都答得出、都不报错。
//
// 记账**必须发生在每一个窗口被观测到的那一刻**，而不是等有人打开页面：
// 等人来看才记，记下的就只有他看过的那几个窗口，而"这条规则我们观察了多久"、
// "一致率在变好还是变坏"恰恰要的是他没看的那些。
//
// 两种接入形态各有一个入口：
//
//   - 拉取式：采集器每跑完一轮，就着它自己那个流量窗口调对应的 Recorder
//   - 推送式：平台按固定周期调 Accountant，窗口取集群最近一次摄入
//
// 分开的是"窗口从哪来"，不是"怎么记"。
package bookkeeping

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/imkerbos/Distill/internal/flow"
	"github.com/imkerbos/Distill/internal/policygen"
	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/snapshotstore"
	"github.com/imkerbos/Distill/internal/store"
)

// Previewer 是记证据要用的那一样能力：把一个窗口的候选集算出来。
type Previewer interface {
	PolicyPreviewAtGranularity(
		ctx context.Context, clusterID, namespace string, window store.TimeWindow,
		granularity policygen.Granularity,
	) (store.PolicyPreview, error)
}

// Store 落一次证据。
type Store interface {
	RecordRuleEvidence(
		ctx context.Context, clusterID string, from, to time.Time,
		complete bool, rules []snapshotstore.RuleEvidence,
	) error
}

// Recorder 把一个指定窗口的候选规则记进证据表。
type Recorder struct {
	previewer Previewer
	store     Store
	logger    *slog.Logger
}

// NewRecorder 构造一个记账器。
func NewRecorder(p Previewer, s Store, logger *slog.Logger) Recorder {
	return Recorder{previewer: p, store: s, logger: logger}
}

// RecordWindow 把窗口 [from, to) 里生成出来的候选规则记进证据表。
//
// **按 WORKLOAD 粒度记**：证据挂在规则指纹上，而指纹由规则内容决定；
// namespace 粒度是同一批规则折叠之后的产物，两套粒度混着记会让同一条放行
// 在两个指纹下各积累一半证据。
//
// 失败返回错误而不是咽下去：证据缺一个窗口的后果是那条规则显得比实际更弱
// —— 安全方向，但会让操作者永远等不到它变得可信，而没有任何东西说得出为
// 什么。调用方决定这次失败要不要中止整轮。
func (r Recorder) RecordWindow(ctx context.Context, clusterID string, from, to time.Time) error {
	pv, err := r.previewer.PolicyPreviewAtGranularity(ctx, clusterID, "",
		store.TimeWindow{From: from, To: to}, policygen.GranularityWorkload)
	if err != nil {
		return fmt.Errorf(
			"evidence: cannot generate candidates for cluster %s; "+
				"this window will be missing from every rule's evidence: %w", clusterID, err)
	}

	var rules []snapshotstore.RuleEvidence
	for _, c := range pv.Candidates {
		for _, rule := range c.Rules {
			// 被否决的规则也记：操作者取消确认之后，那条规则的证据不该
			// 从零开始重数 —— 它一直在被观测，只是没有被采纳。
			// 规则体一并记下。没有它，这张表只有指纹，而指纹是单向的——
			// 于是"跨窗口累积的规则集"取不回来，平台学了一天、导出时只
			// 拿得到最后一个窗口里跑过的那些（design doc 2026-08-29 §1）。
			//
			// 序列化失败**只跳过这一条的规则体、不中断记账**：计数仍然要记，
			// 否则一条规则体有问题的规则会把整个窗口的证据一起吞掉。
			body, err := policygen.MarshalRule(rule)
			if err != nil {
				r.logger.Warn("cannot persist a rule body; its counters are still recorded",
					"cluster", clusterID, "namespace", c.Namespace,
					"workload", c.Workload, "fingerprint", rule.Fingerprint, "err", err)
				body = nil
			}
			rules = append(rules, snapshotstore.RuleEvidence{
				Fingerprint: rule.Fingerprint,
				Namespace:   c.Namespace,
				Workload:    c.Workload,
				// FlowCount 是这条规则在**这个窗口**里的观测次数；
				// 跨窗口的累计由落库层做。
				Observations: int64(rule.FlowCount),
				Body:         body,
			})
		}
	}
	// 完整度取预览回显的那一个，不在这里另算一遍：屏幕上的完整度与证据里
	// 记下的完整度必须是同一次计算的两种呈现。
	complete := pv.WindowCompleteness == flow.CompletenessComplete
	if err := r.store.RecordRuleEvidence(ctx, clusterID, from, to, complete, rules); err != nil {
		return fmt.Errorf("evidence: cannot record rule evidence for cluster %s: %w", clusterID, err)
	}
	r.logger.Info("recorded rule evidence",
		"cluster", clusterID, "rules", len(rules),
		// 完整度进日志：一串"记了 75 条"的日志读起来像证据在稳步积累，
		// 而如果每一轮都证明不了自己没漏，积累起来的强度不是那个意思。
		"windowComplete", complete,
		"windowFrom", from, "windowTo", to)
	return nil
}

// Clusters 列出登记过的集群。
type Clusters interface {
	Clusters(ctx context.Context) ([]registry.Cluster, error)
}

// Windows 报告一个集群最近一次摄入窗口。
type Windows interface {
	DefaultWindow(ctx context.Context, clusterID string) (store.TimeWindow, error)
}

// Accounted 报告某一类记账已经记到哪个窗口末端。
type Accounted interface {
	LastAccountedWindowEnd(ctx context.Context, clusterID string) (time.Time, bool, error)
}

// WindowRecorder 把一个指定窗口的派生结果记下来。
type WindowRecorder interface {
	RecordWindow(ctx context.Context, clusterID string, from, to time.Time) error
}

// Task 是一件"每个观测窗口做一次"的记账。
//
// Accounted 与 Recorder 成对出现，**每件事各带各的守卫**：合成一个守卫的话，
// 证据记成功、对账失败之后，下一轮会因为"这个窗口记过了"把对账永久跳过，
// 而没有任何东西说得出为什么。
type Task struct {
	// Name 进日志，用来分清是哪一件记账失败了。
	Name string
	// Recorder 做这件事。
	Recorder WindowRecorder
	// Accounted 回答"这件事记到哪个窗口了"。
	Accounted Accounted
}

// Accountant 是推送式接入下的记账调度器。
//
// 推送式接入没有采集器那一轮循环可挂：资产与流量都由被管集群自己推上来，
// 落库即返回。**派生记账因此必须由平台自己按周期发起** —— 否则每一次推送
// 都记一遍，而窗口数是逐次加一，一个 15 节点的集群会比单节点集群快 15 倍地
// "积累证据"，那个数字随即失去意义。
type Accountant struct {
	clusters Clusters
	windows  Windows
	tasks    []Task
	logger   *slog.Logger
}

// NewAccountant 构造一个按集群记账的调度器。
func NewAccountant(c Clusters, w Windows, logger *slog.Logger, tasks ...Task) Accountant {
	return Accountant{clusters: c, windows: w, tasks: tasks, logger: logger}
}

// Once 给每个集群的每件记账各做一次。
//
// **一个集群失败不带走其余集群，一件记账失败不带走其余记账。** 没有摄入过、
// 还没接入、这一刻算不出候选集 —— 每一种都只影响它自己那一行，而把它们并成
// 一次返回会让一个新登记的空集群挡住整个 fleet 的记账。只有列不出集群才是
// 整轮失败。
func (a Accountant) Once(ctx context.Context) error {
	clusters, err := a.clusters.Clusters(ctx)
	if err != nil {
		return fmt.Errorf("bookkeeping: cannot list clusters for accounting: %w", err)
	}
	for _, c := range clusters {
		if err := ctx.Err(); err != nil {
			return err
		}
		// **只给读真实采集数据的集群记账。** fixture 数据集是合成的，它的
		// "观测"不描述任何真实集群；给它记账会让一条规则带着一份看起来很扎实
		// 的证据出现在屏幕上，而那份证据背后一条真实连接都没有。
		//
		// 采集侧 Reader 的来源门禁也会拒掉它，这里是第二道 —— 那一道守的是
		// "读谁的数据"，这一道守的是"给谁记账"。未登记的取值一并落到这里，
		// 失败方向朝关。
		if c.DataSource != registry.DataSourceCollected {
			continue
		}
		a.accountCluster(ctx, c.ID)
	}
	return nil
}

// accountCluster 给一个集群做一轮记账，失败只留日志。
func (a Accountant) accountCluster(ctx context.Context, clusterID string) {
	window, err := a.windows.DefaultWindow(ctx, clusterID)
	if err != nil {
		// 绝大多数是"这个集群还没摄入过流量"或"它不是采集来源"，
		// 两者都是合法状态，不是故障 —— 因此是 Debug 而不是 Warn。
		a.logger.Debug("no ingest window to account for",
			"cluster", clusterID, "err", err)
		return
	}
	for _, task := range a.tasks {
		if err := ctx.Err(); err != nil {
			return
		}
		a.runTask(ctx, task, clusterID, window)
	}
}

// runTask 做一件记账，先过它自己的守卫。
func (a Accountant) runTask(
	ctx context.Context, task Task, clusterID string, window store.TimeWindow,
) {
	last, ok, err := task.Accounted.LastAccountedWindowEnd(ctx, clusterID)
	if err != nil {
		// **读不到就跳过，不当成"没记过"。** 当成没记过会在这条查询坏掉的
		// 每一轮都重记同一个窗口，而这些计数只增不减，涨上去就下不来了。
		a.logger.Warn("cannot tell which window this bookkeeping reaches; skipping this round",
			"cluster", clusterID, "task", task.Name, "err", err)
		return
	}
	// 已经记过的窗口不再记。窗口数是逐次加一，按窗口不幂等：记账周期比推送
	// 快、或者 agent 整个停了，同一个窗口会被反复记，于是一条规则在无人观测的
	// 情况下看起来越来越可信 —— 朝着让人放心的方向错。
	if ok && !window.To.After(last) {
		a.logger.Debug("this cluster's latest ingest window is already accounted",
			"cluster", clusterID, "task", task.Name,
			"windowTo", window.To, "accountedThrough", last)
		return
	}

	if err := task.Recorder.RecordWindow(ctx, clusterID, window.From, window.To); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		a.logger.Warn("could not account this window",
			"cluster", clusterID, "task", task.Name, "err", err)
	}
}

// AccountedFunc 让一个具名的读取方法充当 Accounted。
//
// 落库层保留两个各自具名的方法（LastRuleEvidenceWindowEnd /
// LastReconciliationWindowEnd）而不是一个同名方法：它们读的是两张表，
// 撞名会让"这一行到底问的是哪一张表"只能靠上下文猜。适配放在这里，
// 名字留在它该在的地方。
type AccountedFunc func(ctx context.Context, clusterID string) (time.Time, bool, error)

// LastAccountedWindowEnd 满足 Accounted。
func (f AccountedFunc) LastAccountedWindowEnd(
	ctx context.Context, clusterID string,
) (time.Time, bool, error) {
	return f(ctx, clusterID)
}
