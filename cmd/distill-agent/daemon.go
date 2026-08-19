package main

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// job 是一条循环每一轮要做的事。
type job func(ctx context.Context) error

// loops 是常驻 agent 的两条节奏。
//
// 两件事的**基数不同**，这是它们必须分开跑的全部理由：conntrack 按节点
// （每个 Pod 都要跑，否则那个节点是盲区），资产按集群（只能有一个 Pod 跑，
// 否则 N 份数据撞 observed_at 主键 —— 实测报过 CodeConcurrentCollection）。
type loops struct {
	// flow 采本节点的流量，每个 Pod 都跑。
	flow      job
	flowEvery time.Duration
	// assets 采整个集群的资产，只有 leader 跑。
	assets      job
	assetsEvery time.Duration
	// leaderFor 报告此刻本 Pod 是不是 leader。
	//
	// 做成一个函数而不是一个布尔：leadership 会在进程生命周期内变化，
	// 而一个在启动时抄下来的布尔会让一个刚接手的 Pod 永远不采资产、
	// 或者让一个刚失去 leadership 的 Pod 继续采。
	leaderFor func(ctx context.Context) bool
	logger    *slog.Logger
}

// runLoops 起两条互不阻塞的循环，返回一个在两条都停下之后关闭的 channel。
//
// **互不阻塞是重点。** 资产那一轮要列整个集群，慢；让它拖住 conntrack 的话，
// 停摆期间的流量没有任何来源看得见，而缺席在库里与「这段时间没有流量」长得
// 一模一样 —— 后者会被下游读成「覆盖这些连接的规则可以收紧」
// （design doc 2026-08-19-unified-agent §3）。
func runLoops(ctx context.Context, l loops) <-chan struct{} {
	done := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(2)
	go func() {
		defer wg.Done()
		tick(ctx, l.flowEvery, l.logger, "conntrack", func(ctx context.Context) error {
			return l.flow(ctx)
		})
	}()
	go func() {
		defer wg.Done()
		tick(ctx, l.assetsEvery, l.logger, "assets", func(ctx context.Context) error {
			// leadership **每一轮现问**：它会在进程生命周期内变化，而一个
			// 启动时抄下来的答案会让刚接手的 Pod 永远不采、或让刚失去
			// leadership 的 Pod 继续采。
			if !l.leaderFor(ctx) {
				return nil
			}
			return l.assets(ctx)
		})
	}()

	go func() {
		wg.Wait()
		close(done)
	}()
	return done
}

// tick 每隔 every 跑一次 fn，直到 ctx 结束。
//
// **第一轮立即跑，不等一个间隔**：一个刚拉起来的 agent 要马上给出信号，
// 而不是让人等三十分钟才知道它到底活没活。
//
// **一轮失败不停循环**：conntrack 读不到可能是模块还没加载，而一个自己
// 关掉了的采集器不会再告诉任何人。持续报错正是「这个集群的数据面绕开了
// netfilter」这个结论的依据（design doc §5）。
func tick(ctx context.Context, every time.Duration, logger *slog.Logger, what string, fn job) {
	if every <= 0 {
		logger.Error("a loop was configured with a non-positive interval and will not run",
			"loop", what)
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := fn(ctx); err != nil && ctx.Err() == nil {
			// 只记，不停。错误文本由下层保证不带地址与凭据。
			logger.Error("a collection round failed", "loop", what, "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// 两条循环的默认间隔与那个 Lease 的默认名字。
//
// 分开给默认值，理由与分开跑同源：两件事的基数不同。conntrack 一分钟一轮
// 是为了让短连接有更多次被看见的机会；资产三十分钟一轮已经远快于集群拓扑
// 真正变化的速度，而它每一轮要列整个集群。
const (
	defaultFlowEvery   = time.Minute
	defaultAssetsEvery = 30 * time.Minute
	defaultLeaseName   = "distill-agent-assets"
)
