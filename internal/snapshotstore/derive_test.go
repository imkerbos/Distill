package snapshotstore_test

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/config"
	"github.com/imkerbos/Distill/internal/mysqlregistry"
	"github.com/imkerbos/Distill/internal/snapshotstore"
)

// deriveRunRow 是 identity_derive_run 里的一行，读回来比对用。
type deriveRunRow struct {
	status      string
	errorReason string
	startedAt   time.Time
	finishedAt  time.Time
}

func readDeriveRun(t *testing.T, db *sql.DB, clusterID, runID string) (deriveRunRow, bool) {
	t.Helper()
	var row deriveRunRow
	err := db.QueryRow(
		`SELECT status, error_reason, started_at, finished_at
		   FROM identity_derive_run WHERE cluster_id = ? AND run_id = ?`,
		clusterID, runID).Scan(&row.status, &row.errorReason, &row.startedAt, &row.finishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return deriveRunRow{}, false
	}
	if err != nil {
		t.Fatalf("read derive run (%s, %s): %v", clusterID, runID, err)
	}
	return row, true
}

// 同一集群的第二次推导必须拿不到互斥，另一个集群的必须拿得到。
//
// **这条用例是串行化的真实受力面。** 两次并发推导在 Go 这一侧看不出问题：
// DeriveIdentityIntervals 的事务各自都是原子的，走 UPDATE 时它们只在行锁上
// 排队，后写的赢且不报任何错（spec §2.1）。互斥必须由数据库自己给出，因此
// 这条只能对真实 MySQL 跑 —— mock 出来的锁是自己证明自己。
func TestTheDeriveLockExcludesASecondDerivationOfTheSameCluster(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := t.Context()

	release, err := s.LockCluster(ctx, clusterA)
	if err != nil {
		t.Fatalf("LockCluster(%s) error = %v", clusterA, err)
	}

	// 第二次取同一个集群：必须是一次明确的拒绝，不是排队等着。
	if _, err := s.LockCluster(ctx, clusterA); !errors.Is(err, snapshotstore.ErrDeriveInProgress) {
		t.Fatalf("second LockCluster(%s) error = %v, want %v — two derivations of one cluster "+
			"must never overlap: the later writer wins silently and closes an interval at the "+
			"wrong moment", clusterA, err, snapshotstore.ErrDeriveInProgress)
	}

	// 另一个集群不受影响：互斥是按集群的，一把全局锁会让 fleet 采集串成一条线。
	releaseB, err := s.LockCluster(ctx, clusterB)
	if err != nil {
		t.Fatalf("LockCluster(%s) error = %v, want it to be unaffected by %s", clusterB, err, clusterA)
	}
	if err := releaseB(ctx); err != nil {
		t.Fatalf("release(%s) error = %v", clusterB, err)
	}

	if err := release(ctx); err != nil {
		t.Fatalf("release(%s) error = %v", clusterA, err)
	}
	// 释放之后必须能再取到：一把放不掉的锁与并发一样致命，只是失败方向相反 ——
	// 这个集群从此再也推导不了，而症状同样是"区间不更新"。
	again, err := s.LockCluster(ctx, clusterA)
	if err != nil {
		t.Fatalf("LockCluster(%s) after release error = %v", clusterA, err)
	}
	if err := again(ctx); err != nil {
		t.Fatalf("release(%s) error = %v", clusterA, err)
	}
}

// 释放必须真的还掉**会话**上的锁，而不只是把连接还回池子。
//
// 断言从另一个连接池发起，这是这条用例唯一能受力的形式：MySQL 的用户级锁对
// 同一个会话是可重入的，于是"取—放—再取"若落回同一条连接，即便一次
// RELEASE_LOCK 都没发也照样成功 —— 那样的用例是绿的，而锁已经被留在池里
// 一条谁也不知道正持有它的连接上，直到进程退出。换一个池就换了一个会话，
// 泄漏立刻表现为拿不到锁。
func TestReleasingTheDeriveLockFreesItForOtherSessions(t *testing.T) {
	s, _ := newTestStore(t)
	other := newSecondPoolStore(t)
	ctx := t.Context()

	release, err := s.LockCluster(ctx, clusterA)
	if err != nil {
		t.Fatalf("LockCluster(%s) error = %v", clusterA, err)
	}
	if err := release(ctx); err != nil {
		t.Fatalf("release(%s) error = %v", clusterA, err)
	}

	again, err := other.LockCluster(ctx, clusterA)
	if err != nil {
		t.Fatalf("LockCluster(%s) from another session after release error = %v — the lock is still "+
			"held by a pooled connection nobody knows is holding it, and this cluster cannot be "+
			"derived again until the process exits", clusterA, err)
	}
	if err := again(ctx); err != nil {
		t.Fatalf("release(%s) error = %v", clusterA, err)
	}
}

// newSecondPoolStore 开第二个连接池：独立的池就是独立的会话，而用户级锁的
// 作用域正是会话。
func newSecondPoolStore(t *testing.T) *snapshotstore.Store {
	t.Helper()
	db, err := mysqlregistry.Open(config.DatabaseConfig{DSN: testDSN(t), MaxOpenConns: 2, MaxIdleConns: 1})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return snapshotstore.New(db)
}

// 推导记录与采集运行分开一行，且同一次采集恒定只有一行。
//
// 重跑与补跑会对同一次采集再推一遍，推导本身是幂等的，它的记账没有理由在
// 第二遍变成一次主键冲突。两行答同一个问题就等于答不出。
func TestSaveDeriveRunKeepsOneRowPerCollectionRun(t *testing.T) {
	s, db := newTestStore(t)
	ctx := t.Context()

	const runID = "run-derived"
	if err := s.Save(ctx, podRun(clusterA, runID, runOneAt)); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	failed := snapshotstore.DeriveRun{
		ClusterID: clusterA, RunID: runID,
		StartedAt: runOneAt, FinishedAt: runOneAt.Add(time.Second),
		Status: snapshotstore.DeriveFailed, ErrorReason: snapshotstore.DeriveErrorLockUnavailable,
	}
	if err := s.SaveDeriveRun(ctx, failed); err != nil {
		t.Fatalf("SaveDeriveRun() error = %v", err)
	}
	got, ok := readDeriveRun(t, db, clusterA, runID)
	if !ok {
		t.Fatal("no derive run was recorded; a swallowed derivation failure leaves a run that " +
			"looks entirely normal and attributes nothing")
	}
	if got.status != string(snapshotstore.DeriveFailed) ||
		got.errorReason != string(snapshotstore.DeriveErrorLockUnavailable) {
		t.Errorf("recorded (%q, %q), want (%q, %q)", got.status, got.errorReason,
			snapshotstore.DeriveFailed, snapshotstore.DeriveErrorLockUnavailable)
	}

	// 补跑成功：同一行被覆盖，原因清空。
	okRun := failed
	okRun.Status = snapshotstore.DeriveOK
	okRun.ErrorReason = snapshotstore.DeriveErrorNone
	okRun.FinishedAt = runOneAt.Add(2 * time.Second)
	if err := s.SaveDeriveRun(ctx, okRun); err != nil {
		t.Fatalf("second SaveDeriveRun() error = %v", err)
	}

	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM identity_derive_run WHERE cluster_id = ? AND run_id = ?`,
		clusterA, runID).Scan(&n); err != nil {
		t.Fatalf("count derive runs: %v", err)
	}
	if n != 1 {
		t.Errorf("identity_derive_run holds %d rows for one collection run, want 1", n)
	}
	got, _ = readDeriveRun(t, db, clusterA, runID)
	if got.status != string(snapshotstore.DeriveOK) || got.errorReason != "" {
		t.Errorf("after a successful rerun the row reads (%q, %q), want (%q, \"\")",
			got.status, got.errorReason, snapshotstore.DeriveOK)
	}
}

// 落库之后分辨不出来的推导记录必须被拒绝。
func TestSaveDeriveRunRefusesUnreadableOutcomes(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := t.Context()

	const runID = "run-guarded"
	if err := s.Save(ctx, podRun(clusterA, runID, runOneAt)); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	base := snapshotstore.DeriveRun{
		ClusterID: clusterA, RunID: runID,
		StartedAt: runOneAt, FinishedAt: runOneAt.Add(time.Second),
	}

	cases := []struct {
		name string
		run  snapshotstore.DeriveRun
	}{
		{"no status at all", base},
		{"a failure with no reason", withOutcome(base, snapshotstore.DeriveFailed, snapshotstore.DeriveErrorNone)},
		{"an OK carrying a reason", withOutcome(base, snapshotstore.DeriveOK, snapshotstore.DeriveErrorOther)},
		{"an unregistered reason", withOutcome(base, snapshotstore.DeriveFailed, "SOMETHING")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.SaveDeriveRun(ctx, tc.run); err == nil {
				t.Errorf("SaveDeriveRun() = nil error for %s, want a refusal", tc.name)
			}
		})
	}
}

func withOutcome(
	run snapshotstore.DeriveRun, status snapshotstore.DeriveStatus, reason snapshotstore.DeriveErrorReason,
) snapshotstore.DeriveRun {
	run.Status = status
	run.ErrorReason = reason
	return run
}
