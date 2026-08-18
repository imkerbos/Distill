package snapshotstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/snapshotstore"
)

func TestSaveRefusesToStoreTheSameRunTwice(t *testing.T) {
	ctx := context.Background()
	s, db := newTestStore(t)
	at := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)

	run := sampleRun(clusterA, "run-dup", at)
	if err := s.Save(ctx, run); err != nil {
		t.Fatalf("first Save() error = %v", err)
	}

	// agent 跑在 CronJob 里，网络抖动重试是常态：第二次推同一个 run_id
	// 不是错误，是同一次采集又说了一遍。必须能被调用方识别出来，而不是
	// 塌成一个「写库失败」——后者会让重试变成一次 500，agent 于是接着重试。
	err := s.Save(ctx, run)
	if !errors.Is(err, snapshotstore.ErrRunExists) {
		t.Fatalf("second Save() error = %v, want ErrRunExists", err)
	}

	// **第一份留着，不被覆盖。** 覆盖等于让后到的一次推送改写历史，而
	// 历史正是这个平台用来解释「那时候是什么样」的东西。
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM collection_run WHERE cluster_id = ? AND run_id = ?`,
		clusterA, "run-dup").Scan(&n); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if n != 1 {
		t.Errorf("collection_run rows = %d, want 1", n)
	}
	var pods int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM observed_pod WHERE cluster_id = ? AND run_id = ?`,
		clusterA, "run-dup").Scan(&pods); err != nil {
		t.Fatalf("count pods: %v", err)
	}
	if pods != 1 {
		t.Errorf("observed_pod rows = %d, want 1 — 第二次推送把明细又写了一遍", pods)
	}
}

func TestSaveStillAcceptsTheSameRunIDInAnotherCluster(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)
	at := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)

	// run_id 只在集群内唯一：两个集群的 agent 各自生成 ID，撞车是正常的。
	// 判重少了 cluster_id 那一半，一个集群的采集会把另一个集群的挡下来。
	if err := s.Save(ctx, sampleRun(clusterA, "run-same", at)); err != nil {
		t.Fatalf("Save(clusterA) error = %v", err)
	}
	if err := s.Save(ctx, sampleRun(clusterB, "run-same", at)); err != nil {
		t.Errorf("Save(clusterB, same run id) error = %v, want nil", err)
	}
}
