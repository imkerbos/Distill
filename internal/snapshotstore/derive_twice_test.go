package snapshotstore_test

import (
	"context"
	"testing"
	"time"
)

// 顺序地把同一次采集运行推导两遍是幂等的。
//
// **推送式接入的重试语义完全建立在这条上**（design doc 2026-08-18）：agent 在
// CronJob 里跑，一次推导失败会让平台答错误、agent 重推，而重推会再推导一次。
// 如果第二遍会失败或者会写出第二份区间，那条自愈路径就变成了一个死循环。
//
// identity.go 里那段「两边 INSERT 撞主键」说的是**并发**推同一次运行，与这里
// 不是一回事 —— 并发那条由 LockCluster 挡着。
func TestDerivingTheSameRunTwiceIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s, db := newTestStore(t)
	at := time.Date(2026, 8, 18, 11, 0, 0, 0, time.UTC)

	if err := s.Save(ctx, sampleRun(clusterA, "run-twice", at)); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	release, err := s.LockCluster(ctx, clusterA)
	if err != nil {
		t.Fatalf("LockCluster() error = %v", err)
	}
	first := s.DeriveIdentityIntervals(ctx, clusterA, "run-twice")
	second := s.DeriveIdentityIntervals(ctx, clusterA, "run-twice")
	if err := release(ctx); err != nil {
		t.Fatalf("release: %v", err)
	}

	var intervals int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM pod_identity_interval WHERE cluster_id = ?`, clusterA).
		Scan(&intervals); err != nil {
		t.Fatalf("count intervals: %v", err)
	}

	if first != nil {
		t.Fatalf("the first derivation failed, so this test says nothing: %v", first)
	}
	if second != nil {
		t.Errorf("the second derivation failed: %v — 重推自愈会变成死循环", second)
	}
	if intervals != 1 {
		t.Errorf("intervals = %d, want 1 — 第二遍又写了一份区间，"+
			"同一个 Pod 会有两条互相矛盾的身份记录", intervals)
	}
}
