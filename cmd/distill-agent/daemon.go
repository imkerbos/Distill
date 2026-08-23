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
	// leaderChanged 在 leadership 状态可能变化时收到一个信号。
	//
	// **没有它，一个刚当选的 leader 要空等一整个 assetsEvery 才首次采资产。**
	// 选举是并发的：进程起来时 leaderFor 还是 false，立即首轮被跳过，之后
	// tick 要等满一个间隔。生产 assetsEvery=30m —— 首次部署后头 30 分钟没有
	// 任何资产，flows 全程答「还没有可用的采集数据」。当选这件事本身要触发
	// 一次采集，这个通道就是那个信号（实测：worker 抢到租约后 +2m 才首采）。
	//
	// 允许为 nil：没有它时退回纯 ticker 行为，只是首采会晚。
	leaderChanged <-chan struct{}
	// flowHeartbeat 在 flow 循环每转到一圈的顶时被调用一次。
	//
	// **挂在 flow 循环上，不挂在资产循环上**：资产只有 leader 采，非 leader
	// 的心跳会永远停在启动那一刻。flow 每个 Pod 每轮都跑，是唯一能代表
	// 「这个 Pod 的进程还在转」的信号。
	//
	// **在一圈的顶调用，早于这一轮的采集**：探的是 goroutine 卡没卡死，不是
	// 这一轮采成没采成。conntrack 读不到时这一轮会失败，但循环仍在转，心跳
	// 仍该刷新 —— 否则一个持续报 FAILED（那是有意义的信号）的 agent 会被
	// liveness 反复重启。允许为 nil：没配 -heartbeat-file 就不写。
	flowHeartbeat func()
	logger        *slog.Logger
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
			// 心跳先写：探的是「循环转到了这里」，与这一轮采成没采成无关。
			if l.flowHeartbeat != nil {
				l.flowHeartbeat()
			}
			return l.flow(ctx)
		})
	}()
	go func() {
		defer wg.Done()
		assetLoop(ctx, l)
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

// assetLoop 采整集群资产，只有 leader 采。
//
// 与 flow 那条不同的地方全在这里：**它要对「刚当选」立刻反应，不能只靠
// ticker。** ticker 之外还选 leaderChanged —— 一次 false→true 的跳变触发一次
// 立即采集。没有它，一个刚起来或刚接手的 leader 会空等一整个 assetsEvery
// （生产 30m），期间 flows 一直答「还没有可用的采集数据」。
//
// **只在跳变时触发，不在每个 changed 上触发。** 已经是 leader 时又来一个
// changed 不该再采：两次相邻采集会撞 observed_at 主键（实测报过
// CodeConcurrentCollection）。wasLeader 记住上一次的判定，只有 false→true
// 才是一次新的当选。
func assetLoop(ctx context.Context, l loops) {
	if l.assetsEvery <= 0 {
		l.logger.Error("a loop was configured with a non-positive interval and will not run",
			"loop", "assets")
		return
	}
	wasLeader := false
	// collect 现问 leadership 并在是 leader 时采一轮；返回此刻是不是 leader，
	// 供调用点更新 wasLeader。**每一轮现问**：leadership 会变，抄一份会让
	// 刚失去 leadership 的 Pod 继续采。
	collect := func() bool {
		if !l.leaderFor(ctx) {
			return false
		}
		if err := l.assets(ctx); err != nil && ctx.Err() == nil {
			l.logger.Error("a collection round failed", "loop", "assets", "err", err)
		}
		return true
	}

	// 立即首轮：起步就是 leader 时不必等 changed。
	wasLeader = collect()

	t := time.NewTicker(l.assetsEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// 周期性采集：是 leader 就采。wasLeader 同步更新，好让紧随其后
			// 的一次 changed 不被当成新当选。
			wasLeader = collect()
		case <-l.leaderChanged:
			isLeader := l.leaderFor(ctx)
			// 只有 false→true 才是一次新当选，才立即采。已经是 leader 时
			// 的重复信号不采 —— 会撞 observed_at 主键。
			if isLeader && !wasLeader {
				wasLeader = collect()
			} else {
				wasLeader = isLeader
			}
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
