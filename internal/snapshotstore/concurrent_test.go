package snapshotstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/snapshotstore"
)

// 两个采集器在同一时刻采同一个集群会怎样。
//
// **这条用例来自一次真实的并发演练**：两个 agent 同时装进 kind 打同一个
// 集群，其中一个整份被拒，agent 收到的是一个裸的「服务内部错误」——
// 操作者会去查平台，而真正的成因是这个集群同时跑了两个采集器。
//
// observed_* 系列表的主键是 (cluster_id, name, observed_at)，**不含 run_id**：
// 它们是时序表，同一时刻同一个对象只能有一份观测。两次运行撞上同一个
// observed_at 是一次真实的冲突，回滚是对的 —— 要修的是它说不出成因。
func TestASecondObservationAtTheSameInstantIsNamedNotSwallowed(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)
	at := time.Date(2026, 8, 18, 13, 0, 0, 0, time.UTC)

	if err := s.Save(ctx, sampleRun(clusterA, "run-first", at)); err != nil {
		t.Fatalf("first Save() error = %v", err)
	}

	// 不同的 run_id、同一个 observed_at：collection_run 那张表不会撞
	// （主键是 cluster_id + run_id），撞的是观测本身。
	err := s.Save(ctx, sampleRun(clusterA, "run-second", at))
	if err == nil {
		t.Fatal("second Save() = nil — 同一时刻的第二份观测被收下了，" +
			"那两份数据里有一份不是那一刻的事实")
	}
	if !errors.Is(err, snapshotstore.ErrObservationExists) {
		t.Errorf("second Save() error = %v, want ErrObservationExists — "+
			"塌成一个通用失败，调用方就说不出这是两个采集器撞车", err)
	}
	// 与 ErrRunExists 分开：那一条说的是「这一次运行已经交付过了」，
	// 处置是什么都不用做；这一条说的是「另一次运行占了这个时刻」，
	// 处置是查为什么有两个采集器。
	if errors.Is(err, snapshotstore.ErrRunExists) {
		t.Error("the conflict was reported as an already-delivered run")
	}
}
