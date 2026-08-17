package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/imkerbos/Distill/internal/snapshotstore"
)

// deriveStore 是一次身份推导要的落库口与串行化口。*snapshotstore.Store 满足它。
//
// 与 runStore / ingestStore 分成第三个接口，理由与那两个分开同源（spec §2.2）：
// 推导的成败必须落在自己的记录上。分开的接口让"顺手把推导结果塞进采集那张表"
// 这件事在类型层面就没有落脚点。
type deriveStore interface {
	// LockCluster 取得这个集群推导的互斥，返回释放它的函数。
	//
	// 拿不到时返回包着 snapshotstore.ErrDeriveInProgress 的错误 —— 那是一次
	// 明确的失败，不是"跳过"。
	LockCluster(ctx context.Context, clusterID string) (func(context.Context) error, error)
	// DeriveIdentityIntervals 由一次已落库的采集运行推导 Pod 身份区间。
	DeriveIdentityIntervals(ctx context.Context, clusterID, runID string) error
	// SaveDeriveRun 记下这次推导的成败。
	SaveDeriveRun(ctx context.Context, run snapshotstore.DeriveRun) error
}

// deriveOnce 为一次已落库的采集运行推导身份区间，并单独记下这次推导的成败。
//
// **互斥先于推导，这是这个函数存在的一半理由。** DeriveIdentityIntervals 的
// 事务只保证单次推导的原子性：同一集群的两次**不同**运行并发推导时走 UPDATE
// 路径，只在行锁上排队，后写的赢且不报任何错，于是一个仍在运行的 Pod 的区间
// 被关在错的时刻 —— 之后它的地址解析成 NOT_COVERED，或者更糟，解析成后来复用
// 这个地址的另一个工作负载。归属到错误的主体，且不报错（spec §2.1）。
// 采集器是一次性进程，但没有任何东西拦住定时任务与一次手动运行撞上。
//
// **不加自己的状态前置条件。** 一次不完整的采集本来就不该关闭任何区间，
// 而 identity.RunFacts.closesIntervals() 已经守着这条；在这里再判一次状态，
// 等于把那个判断复制到一个没有测试盯着的地方，两处口径迟早分叉。
func deriveOnce(
	ctx context.Context,
	clusterID, runID string,
	store deriveStore,
	logger *slog.Logger,
) error {
	startedAt := time.Now()

	release, err := store.LockCluster(ctx, clusterID)
	if err != nil {
		// 拿不到锁不是跳过：不留下这一行，一次"资产有了、身份没有"的运行在
		// 界面上完全正常，而下游的表现是这个集群的每一条连接都归属不了。
		reason := deriveErrorReason(err)
		logger.Error("identity derivation did not run", "cluster", clusterID,
			"runId", runID, "reason", string(reason))
		recordDeriveRun(ctx, store, snapshotstore.DeriveRun{
			ClusterID: clusterID, RunID: runID,
			StartedAt: startedAt, FinishedAt: time.Now(),
			Status: snapshotstore.DeriveFailed, ErrorReason: reason,
		}, logger)
		return fmt.Errorf("derive identity intervals for cluster %s: %w", clusterID, err)
	}
	defer func() {
		// 释放另起一个不随上游一起被取消的上下文：超时或 Ctrl-C 之后这把锁
		// 更该被放掉，而不是留到进程退出。
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), saveTimeout)
		defer cancel()
		if err := release(releaseCtx); err != nil {
			// 只记日志、不改变这一轮的成败：这是个跑完就退出的进程，进程一退
			// MySQL 会随会话断开自行释放这把锁。把释放失败变成整轮失败，等于
			// 用一次收尾的失败盖掉一次真的推导成功。
			logger.Error("cannot release the derivation lock", "cluster", clusterID, "runId", runID)
		}
	}()

	deriveErr := store.DeriveIdentityIntervals(ctx, clusterID, runID)
	run := snapshotstore.DeriveRun{
		ClusterID: clusterID, RunID: runID,
		StartedAt: startedAt, FinishedAt: time.Now(),
		Status: snapshotstore.DeriveOK,
	}
	if deriveErr != nil {
		run.Status = snapshotstore.DeriveFailed
		run.ErrorReason = deriveErrorReason(deriveErr)
	}

	saveErr := saveDeriveRun(ctx, store, run)
	if deriveErr != nil {
		// 只记封闭枚举的原因，不记底层错误文本：这个进程的输出终点是集群日志
		// （安全规范 §19、§22）。落库成功不等于推导成功 —— 错误照样往上走，
		// 于是编排系统看到的退出码不是 0。
		logger.Error("identity derivation failed", "cluster", clusterID,
			"runId", runID, "reason", string(run.ErrorReason))
		if saveErr != nil {
			logger.Error("cannot record the failed derivation", "cluster", clusterID, "runId", runID)
		}
		return fmt.Errorf("derive identity intervals for cluster %s: %w", clusterID, deriveErr)
	}
	return saveErr
}

// recordDeriveRun 记下一次根本没跑起来的推导，落不下只记日志。
//
// 落库失败不改变调用方要返回的那个错误：没能取得互斥才是操作者要看的东西，
// 把它换成一句"落库失败"等于用记账的失败盖住被记的那件事（同 recordAbortedRun）。
func recordDeriveRun(
	ctx context.Context, store deriveStore, run snapshotstore.DeriveRun, logger *slog.Logger,
) {
	if err := saveDeriveRun(ctx, store, run); err != nil {
		logger.Error("cannot record the derivation outcome",
			"cluster", run.ClusterID, "runId", run.RunID, "reason", string(run.ErrorReason))
	}
}

// saveDeriveRun 落一行推导记录，用一个不随上游一起被取消的上下文。
//
// 理由同 collectOnce 的落库：超时或 Ctrl-C 之后，"这一轮的身份到底推出来没有"
// 正是最该留下来的东西，而复用那个已经取消的 ctx 会让它随之消失。
func saveDeriveRun(ctx context.Context, store deriveStore, run snapshotstore.DeriveRun) error {
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), saveTimeout)
	defer cancel()
	if err := store.SaveDeriveRun(saveCtx, run); err != nil {
		return fmt.Errorf("store the identity derive run: %w", err)
	}
	return nil
}

// deriveErrorReason 把推导失败翻译成封闭枚举的原因（CLAUDE.md §3）。
//
// 一律用 errors.Is 判定，**不匹配错误文本**。认不出的形态落 OTHER —— 这一层的
// 失败方向是"说不出来"，不是猜一个看起来合理的原因，因为原因决定的是谁去查：
// LOCK_UNAVAILABLE 的处置是重跑，其余的处置是去查库。
func deriveErrorReason(err error) snapshotstore.DeriveErrorReason {
	switch {
	case errors.Is(err, snapshotstore.ErrDeriveInProgress):
		return snapshotstore.DeriveErrorLockUnavailable
	case errors.Is(err, context.DeadlineExceeded):
		return snapshotstore.DeriveErrorTimeout
	default:
		return snapshotstore.DeriveErrorOther
	}
}
