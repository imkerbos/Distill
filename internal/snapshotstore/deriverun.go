package snapshotstore

import (
	"context"
	"fmt"
	"time"
)

// DeriveStatus 是一次身份推导的运行状态，封闭枚举。
//
// **没有 PARTIAL**（与 IngestStatus 不同）：推导整个跑在一个事务里，要么整体
// 生效要么整体回滚，"推了一半"这个状态构造不出来。给它留一个取值，等于给
// 后来的人一个填它的位置。零值不合法 —— 一次说不出自己成没成的推导不该落库。
type DeriveStatus string

const (
	// DeriveOK 表示这次推导跑完并提交了。
	DeriveOK DeriveStatus = "OK"
	// DeriveFailed 表示这次推导没有生效，区间表与推导之前逐字节相同。
	DeriveFailed DeriveStatus = "FAILED"
)

// Valid 判断该状态是否已登记。
func (s DeriveStatus) Valid() bool {
	switch s {
	case DeriveOK, DeriveFailed:
		return true
	default:
		return false
	}
}

// DeriveErrorReason 是推导失败的原因，封闭枚举，不用自由文本（CLAUDE.md §3）。
//
// 取值与采集、摄入的失败原因**不共用**：采集失败多半是 RBAC，摄入失败多半是
// relay 或配额，而推导失败要么是这个集群另有一次推导在跑，要么是库本身出了事 ——
// 三者的处置人与处置动作都不同。
type DeriveErrorReason string

const (
	// DeriveErrorNone 表示这次推导没有失败原因，成败看 status。
	DeriveErrorNone DeriveErrorReason = ""
	// DeriveErrorLockUnavailable 表示这个集群另有一次推导正在跑，本次没能取得互斥。
	//
	// 单独成项：处置是重跑，不是排查数据。它恰恰是最容易被当成数据问题去查的
	// 那一种 —— 表象是"区间没更新"，而库本身完全正常。
	DeriveErrorLockUnavailable DeriveErrorReason = "LOCK_UNAVAILABLE"
	// DeriveErrorTimeout 表示在推导完成之前上下文就到期了。
	DeriveErrorTimeout DeriveErrorReason = "TIMEOUT"
	// DeriveErrorOther 是上面几种之外的失败。新增一类具体原因时要同步改这个
	// 枚举、迁移里的注释与统计口径。
	DeriveErrorOther DeriveErrorReason = "OTHER"
)

// Valid 判断该原因是否已登记。空串合法，它表示"没有失败原因"。
func (r DeriveErrorReason) Valid() bool {
	switch r {
	case DeriveErrorNone, DeriveErrorLockUnavailable, DeriveErrorTimeout, DeriveErrorOther:
		return true
	default:
		return false
	}
}

// DeriveRun 是一次身份推导的运行记录：推的是哪次采集、什么时候跑的、成没成。
//
// RunID 是**被推导的那次采集运行**，不是一个新的推导 ID。这张表要回答的是
// "这次采集的身份推出来了吗"，两行答同一个问题就等于答不出。
type DeriveRun struct {
	// ClusterID 是这次推导针对的集群。
	ClusterID string
	// RunID 是被推导的那次采集运行。
	RunID string
	// StartedAt / FinishedAt 是运维时间线上的两个点。
	StartedAt  time.Time
	FinishedAt time.Time
	// Status 是这次推导的状态。
	Status DeriveStatus
	// ErrorReason 仅在这次推导失败时非空。
	ErrorReason DeriveErrorReason
}

// SaveDeriveRun 记下一次身份推导的成败。
//
// **与采集运行分开落库**（spec §2.2）：资产已经落库是既成事实，一次推导失败
// 不得把它抹掉；反过来，推导失败也不得被吞掉 —— 那会让一次"资产有了、身份
// 没有"的运行看起来完全正常，而下游的表现是这个集群的每一条连接都归属不了。
//
// 走 upsert 而不是纯 INSERT：重试与补跑会对同一次采集运行再推一遍，而推导
// 本身就是幂等的，它的记账没有理由在第二遍变成一次主键冲突。同一次采集运行
// 恒定只有一行，最新一次推导的结论覆盖上一次。
func (s *Store) SaveDeriveRun(ctx context.Context, run DeriveRun) error {
	if err := validateDeriveRun(run); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO identity_derive_run
		   (cluster_id, run_id, started_at, finished_at, status, error_reason)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE
		   started_at = ?, finished_at = ?, status = ?, error_reason = ?`,
		run.ClusterID, run.RunID, run.StartedAt, run.FinishedAt,
		string(run.Status), string(run.ErrorReason),
		run.StartedAt, run.FinishedAt, string(run.Status), string(run.ErrorReason),
	); err != nil {
		return fmt.Errorf("snapshotstore: record the derive run for cluster %s: %w", run.ClusterID, err)
	}
	return nil
}

// validateDeriveRun 拒绝那些落库之后就再也分辨不出来的运行。
//
// 与 validateIngestRun 逐条同源：一次 FAILED 而没有原因的推导，在库里与一次
// 成功的推导只差一个字段，而读它的人要靠这个字段决定去重跑还是去查库；
// 一次 OK 而带着原因的推导，是调用方自己都没想清楚这次到底成没成。
func validateDeriveRun(run DeriveRun) error {
	if run.ClusterID == "" {
		return fmt.Errorf("snapshotstore: derive run has no cluster id")
	}
	if run.RunID == "" {
		return fmt.Errorf("snapshotstore: derive run for cluster %s has no collection run id", run.ClusterID)
	}
	if !run.Status.Valid() {
		return fmt.Errorf("snapshotstore: derive run %s carries an unregistered status %q",
			run.RunID, string(run.Status))
	}
	if !run.ErrorReason.Valid() {
		return fmt.Errorf("snapshotstore: derive run %s carries an unregistered error reason %q",
			run.RunID, string(run.ErrorReason))
	}
	if run.Status == DeriveFailed && run.ErrorReason == DeriveErrorNone {
		return fmt.Errorf(
			"snapshotstore: refusing to record a failed derivation for %s with no reason; "+
				"the operator cannot tell a contended run from a broken one",
			run.ClusterID)
	}
	if run.Status == DeriveOK && run.ErrorReason != DeriveErrorNone {
		return fmt.Errorf("snapshotstore: derive run %s reports OK but carries error reason %q",
			run.RunID, string(run.ErrorReason))
	}
	return nil
}
