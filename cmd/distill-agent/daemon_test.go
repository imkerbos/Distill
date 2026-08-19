package main

import (
	"context"
	"errors"
	"sync"
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
