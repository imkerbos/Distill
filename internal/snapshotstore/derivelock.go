package snapshotstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
)

// ErrDeriveInProgress 表示这个集群的身份推导已经被另一个进程持有。
//
// 这是一个**明确的失败**，不是"稍后再说"：调用方必须把它记成一次失败的推导
// 并让这一轮以非零码退出（spec §2.1）。静默跳过会让一次"资产有了、身份没有"
// 的运行看起来完全正常。
var ErrDeriveInProgress = errors.New("snapshotstore: another identity derivation is already running for this cluster")

// deriveLockPrefix 把本平台的用户级锁与同一个 MySQL 实例上别人的锁分开。
//
// **锁名的作用域是整个服务器实例，不是当前 schema。** 于是 distill 与
// distill_test 会共用同一把锁：一次集成测试持锁期间，真实采集会拿不到锁并
// 明确失败。方向是多余的互斥而不是漏掉的互斥，这是刻意选的 —— 反过来（按库
// 分开而漏算一种并发）没有任何症状。
const deriveLockPrefix = "distill-derive-"

// deriveLockWaitSeconds 是取锁的等待秒数：0 表示拿不到立刻返回。
//
// 不排队等待：采集器是一次性进程，一次拿不到锁意味着这个集群另有一次运行正在
// 推导。排队等下去会让"两次运行撞上了"这件事表现成一次莫名变慢的采集，而它
// 该表现成一行 LOCK_UNAVAILABLE —— 那正是操作者需要看见的东西。
const deriveLockWaitSeconds = 0

// LockCluster 取得这个集群身份推导的互斥，返回释放它的函数。
//
// **为什么这一层必须存在。** DeriveIdentityIntervals 的事务只保证单次推导的
// 原子性，挡不住并发的两次：同一集群的两次**不同**运行并发推导时走的是 UPDATE
// 路径，它们只在行锁上排队，后写的赢，且不报任何错。结果是一个仍在运行的 Pod
// 的区间被关在错的时刻，之后它的地址解析成 NOT_COVERED，或者更糟 —— 解析成
// 后来复用这个地址的另一个工作负载。归属到错误的主体，且不报错（spec §2.1）。
//
// **为什么用 MySQL 的用户级锁，而不是库里一张锁表。** 锁表需要租约与过期时间，
// 而租约的长度是一个没人能填对的配置：填短了会在一次慢推导中途把锁让给别人
// （正是这一层要挡住的那件事），填长了会让一次进程崩溃把这个集群锁到租约到期。
// 用户级锁跟着**会话**走：持有者进程被 kill、容器被驱逐、网络断开，MySQL 在
// 会话断开时立即释放它，不留任何需要人工清理的状态，也不需要任何超时配置。
// 这一层不引入新依赖 —— GET_LOCK 是本项目已经在用的 MySQL 自己的能力。
//
// 锁必须在一条**固定的连接**上取：用户级锁的作用域是会话，而 database/sql 的
// 连接池每条语句都可能换一条连接。因此这里 pin 一条 *sql.Conn，直到释放为止。
func (s *Store) LockCluster(ctx context.Context, clusterID string) (func(context.Context) error, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("snapshotstore: take a connection for the derive lock: %w", err)
	}
	name := deriveLockName(clusterID)

	var got sql.NullInt64
	if err := conn.QueryRowContext(ctx,
		`SELECT GET_LOCK(?, ?)`, name, deriveLockWaitSeconds).Scan(&got); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("snapshotstore: acquire the derive lock for cluster %s: %w", clusterID, err)
	}
	if !got.Valid {
		// GET_LOCK 答 NULL 是"出错了"，既不是拿到也不是没拿到（例如会话被 kill）。
		// 当成没拿到会让一次真正的故障读起来像一次寻常的并发。
		_ = conn.Close()
		return nil, fmt.Errorf(
			"snapshotstore: the derive lock for cluster %s answered NULL; "+
				"the server could neither grant nor refuse it", clusterID)
	}
	if got.Int64 != 1 {
		_ = conn.Close()
		return nil, fmt.Errorf("snapshotstore: cluster %s: %w", clusterID, ErrDeriveInProgress)
	}

	return func(releaseCtx context.Context) error {
		// 先显式释放，再把连接还给池。**只 Close 不 RELEASE 是不行的**：
		// Close 只是把这条连接还回池子，会话还活着，锁跟着会话，于是这把锁
		// 会留在池里一条谁也不知道正持有它的连接上，直到进程退出。
		var released sql.NullInt64
		err := conn.QueryRowContext(releaseCtx, `SELECT RELEASE_LOCK(?)`, name).Scan(&released)
		if cerr := conn.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fmt.Errorf("snapshotstore: release the derive lock for cluster %s: %w", clusterID, err)
		}
		return nil
	}, nil
}

// deriveLockName 把集群 ID 映射成一个 MySQL 用户级锁名。
//
// 取哈希而不是直接拼集群 ID：锁名上限是 64 字符，而 cluster_id 一列就有 64，
// 拼上任何前缀都可能越界。哈希碰撞的方向是**多余的互斥**（两个集群互相排斥，
// 各自仍然正确），不是漏掉的互斥 —— 后者才是这一层存在的理由。
//
// 用 SHA-256 而不是更短的摘要：这里不需要密码学强度，但也没有理由挑一个
// 碰撞更容易的；截到 16 字节是为了让锁名连前缀一起停在 47 个字符。
func deriveLockName(clusterID string) string {
	sum := sha256.Sum256([]byte(clusterID))
	return deriveLockPrefix + hex.EncodeToString(sum[:16])
}
