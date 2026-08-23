package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeJob 记下自己被调了几次，并可以人为放慢。
type fakeJob struct {
	mu    sync.Mutex
	calls int
	block chan struct{}
	err   error
}

func (j *fakeJob) run(ctx context.Context) error {
	j.mu.Lock()
	j.calls++
	block := j.block
	err := j.err
	j.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func (j *fakeJob) count() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.calls
}

// waitFor 等一个条件成立，超时即失败。
func waitFor(t *testing.T, why string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", why)
}

// 两条节奏各自按自己的间隔触发。
func TestBothLoopsRunOnTheirOwnCadence(t *testing.T) {
	flows, assets := &fakeJob{}, &fakeJob{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := runLoops(ctx, loops{
		flow:        flows.run,
		flowEvery:   time.Millisecond,
		assets:      assets.run,
		assetsEvery: time.Millisecond,
		leaderFor:   alwaysLeader,
		logger:      quietLogger(t),
	})
	waitFor(t, "both loops to run", func() bool { return flows.count() > 2 && assets.count() > 2 })
	cancel()
	<-done
}

// **资产那一轮慢，不能让 conntrack 停摆。**
//
// 停摆期间的流量没有任何来源看得见，而缺席在库里与「这段时间没有流量」
// 长得一模一样 —— 而后者会被下游读成「覆盖这些连接的规则可以收紧」。
func TestASlowAssetRunDoesNotStallTheFlowLoop(t *testing.T) {
	flows := &fakeJob{}
	assets := &fakeJob{block: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := runLoops(ctx, loops{
		flow:        flows.run,
		flowEvery:   time.Millisecond,
		assets:      assets.run,
		assetsEvery: time.Millisecond,
		leaderFor:   alwaysLeader,
		logger:      quietLogger(t),
	})
	waitFor(t, "the flow loop to keep going while assets are stuck", func() bool {
		return flows.count() > 5 && assets.count() == 1
	})
	close(assets.block)
	cancel()
	<-done
}

// 非 leader 的 Pod 一次资产都不采 —— 每个节点都采会让 N 份数据撞
// observed_at 主键，而那正是实测报过 CodeConcurrentCollection 的那件事。
func TestANonLeaderNeverCollectsAssets(t *testing.T) {
	flows, assets := &fakeJob{}, &fakeJob{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := runLoops(ctx, loops{
		flow:        flows.run,
		flowEvery:   time.Millisecond,
		assets:      assets.run,
		assetsEvery: time.Millisecond,
		leaderFor:   neverLeader,
		logger:      quietLogger(t),
	})
	waitFor(t, "the flow loop to run several times", func() bool { return flows.count() > 5 })
	cancel()
	<-done

	if assets.count() != 0 {
		t.Errorf("a non-leader collected assets %d times; every node listing the whole cluster "+
			"is N times the API load and N copies colliding on the same instant", assets.count())
	}
}

// conntrack 读不到时**不自我关闭**：模块可能事后加载，而一个自己关掉了的
// 采集器不会再告诉任何人。持续报错正是「这个集群的数据面绕开了 netfilter」
// 这个结论的依据。
func TestAFailingFlowLoopKeepsReporting(t *testing.T) {
	flows := &fakeJob{err: errors.New("the conntrack table could not be opened")}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := runLoops(ctx, loops{
		flow:        flows.run,
		flowEvery:   time.Millisecond,
		assets:      func(context.Context) error { return nil },
		assetsEvery: time.Hour,
		leaderFor:   alwaysLeader,
		logger:      quietLogger(t),
	})
	waitFor(t, "the failing loop to keep trying", func() bool { return flows.count() > 5 })
	cancel()
	<-done
}

// 取消要让两条循环都停下来并交回 —— 否则 SIGTERM 之后进程挂着不退。
func TestCancellingStopsBothLoops(t *testing.T) {
	flows, assets := &fakeJob{}, &fakeJob{}
	ctx, cancel := context.WithCancel(context.Background())

	done := runLoops(ctx, loops{
		flow:        flows.run,
		flowEvery:   time.Millisecond,
		assets:      assets.run,
		assetsEvery: time.Millisecond,
		leaderFor:   alwaysLeader,
		logger:      quietLogger(t),
	})
	waitFor(t, "the loops to start", func() bool { return flows.count() > 0 })
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the loops did not stop after the context was cancelled")
	}
}

// 第一轮立即跑，不等一个间隔：一个刚拉起来的 agent 要马上给出信号，
// 而不是让人等三十分钟才知道它到底活没活。
func TestTheFirstRoundIsImmediate(t *testing.T) {
	flows, assets := &fakeJob{}, &fakeJob{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := runLoops(ctx, loops{
		flow:        flows.run,
		flowEvery:   time.Hour,
		assets:      assets.run,
		assetsEvery: time.Hour,
		leaderFor:   alwaysLeader,
		logger:      quietLogger(t),
	})
	waitFor(t, "both loops to run once without waiting an interval", func() bool {
		return flows.count() == 1 && assets.count() == 1
	})
	cancel()
	<-done
}

func alwaysLeader(context.Context) bool { return true }
func neverLeader(context.Context) bool  { return false }

// Lease 的 namespace：显式参数优先，其次 ServiceAccount 挂载。
//
// **不从环境变量兜底。** 那要求部署清单记得配 downward API，而忘了配的
// 症状是 Lease 落进 "default" —— 一个跑在别的 namespace 的 agent 于是与
// 它无关的进程抢同一把锁，两个集群的资产采集互相踩。
func TestTheLeaseNamespaceComesFromTheServiceAccountNotTheEnvironment(t *testing.T) {
	if got, err := resolveNamespace("explicit-ns"); err != nil || got != "explicit-ns" {
		t.Errorf("resolveNamespace(explicit) = %q, %v; want the explicit value", got, err)
	}
	// 这个进程不在集群里，SA 文件不存在 —— 必须报错，不得兜底成任何默认值。
	t.Setenv("POD_NAMESPACE", "sneaky")
	if got, err := resolveNamespace(""); err == nil {
		t.Errorf("resolveNamespace(\"\") = %q with no error; outside a cluster it must refuse "+
			"rather than guess, and it must not read POD_NAMESPACE", got)
	}
}

// flipLeader 是一个可以中途翻转的 leader 判据。
type flipLeader struct {
	v      sync.Mutex
	leader bool
}

func (f *flipLeader) is(context.Context) bool { f.v.Lock(); defer f.v.Unlock(); return f.leader }
func (f *flipLeader) set(b bool)              { f.v.Lock(); f.leader = b; f.v.Unlock() }

// **刚当选的 leader 必须立刻采一次资产，不能空等一整个 assetsEvery。**
//
// leader 选举是并发 goroutine：进程刚起来时 leaderFor 还是 false，于是资产
// 那条循环的立即首轮被跳过，然后 tick 要等满一个间隔才再看一眼。生产
// assetsEvery=30m —— 首次部署后头 30 分钟没有任何资产，flows 全程答「还没有
// 可用的采集数据」；leader 中途换手也一样，新 leader 静默空窗最多 30 分钟。
//
// 实测复现：worker 13:22:15 抢到租约，首次资产采集 13:24:16 = 正好 +2m。
//
// 修法是让「当选」这件事本身触发一次采集，而不是等下一个 tick。changed
// 通道就是那个信号。
func TestANewlyElectedLeaderCollectsWithoutWaitingAFullInterval(t *testing.T) {
	flows, assets := &fakeJob{}, &fakeJob{}
	gate := &flipLeader{} // 起步不是 leader
	changed := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := runLoops(ctx, loops{
		flow:          flows.run,
		flowEvery:     time.Millisecond,
		assets:        assets.run,
		assetsEvery:   time.Hour, // 大到「等一个间隔」等于永远不采
		leaderFor:     gate.is,
		leaderChanged: changed,
		logger:        quietLogger(t),
	})

	// 还不是 leader：立即首轮必须被跳过，一次都不该采。
	waitFor(t, "the flow loop to prove the loops are running", func() bool { return flows.count() > 3 })
	if assets.count() != 0 {
		t.Fatalf("a non-leader collected assets %d times before winning the election", assets.count())
	}

	// 当选。信号一到，必须**立刻**采一次，而不是等满一个 assetsEvery（一小时）。
	gate.set(true)
	changed <- struct{}{}
	waitFor(t, "the freshly-elected leader to collect at once", func() bool { return assets.count() == 1 })

	cancel()
	<-done
}

// 已经是 leader 时，一次 changed 信号不该再多采一份。
//
// 两次相邻的采集会撞 observed_at 主键（实测报过 CodeConcurrentCollection）。
// 只有 false→true 那一次跳变才触发立即采集，重复的 changed 不触发。
func TestAChangedSignalWhileAlreadyLeaderDoesNotDoubleCollect(t *testing.T) {
	flows, assets := &fakeJob{}, &fakeJob{}
	gate := &flipLeader{leader: true} // 起步就是 leader
	changed := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := runLoops(ctx, loops{
		flow:          flows.run,
		flowEvery:     time.Millisecond,
		assets:        assets.run,
		assetsEvery:   time.Hour,
		leaderFor:     gate.is,
		leaderChanged: changed,
		logger:        quietLogger(t),
	})

	// 立即首轮采一次。
	waitFor(t, "the immediate first collection", func() bool { return assets.count() == 1 })

	// 再来一个 changed（还是 leader，没有跳变）——不该再采。
	changed <- struct{}{}
	waitFor(t, "the flow loop to advance so we know time passed", func() bool { return flows.count() > 5 })
	if got := assets.count(); got != 1 {
		t.Errorf("assets collected %d times; a redundant changed signal while already leader "+
			"must not trigger a second collection (they collide on observed_at)", got)
	}

	cancel()
	<-done
}

// flow 循环每转一圈都打一次心跳；资产循环不打。
//
// 心跳要能代表「这个 Pod 的进程还在转」，而资产只有 leader 采 —— 把心跳挂在
// 资产上，非 leader 的 Pod 会永远显示卡死、被 liveness 反复重启。
func TestTheFlowLoopBeatsTheHeartbeatButAssetsDoNot(t *testing.T) {
	flows, assets := &fakeJob{}, &fakeJob{}
	var beats atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := runLoops(ctx, loops{
		flow:          flows.run,
		flowEvery:     time.Millisecond,
		assets:        assets.run,
		assetsEvery:   time.Hour, // 资产基本不转，好证明心跳不是它打的
		leaderFor:     neverLeader,
		flowHeartbeat: func() { beats.Add(1) },
		logger:        quietLogger(t),
	})

	waitFor(t, "the flow loop to beat several times", func() bool {
		return beats.Load() > 3 && flows.count() > 3
	})
	// 非 leader 一次资产都没采，而心跳照打 —— 证明心跳不依赖资产、不依赖 leader。
	if assets.count() != 0 {
		t.Errorf("a non-leader collected assets %d times", assets.count())
	}
	cancel()
	<-done
}
