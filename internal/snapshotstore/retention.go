package snapshotstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// purgeBatch 是一次删除的行数上限。
//
// 分批而不是一条 DELETE：UAT 上这张表 2870 万行，单条删除会长时间持锁，
// 而它同时正在被摄入写入 —— 2026-08-29 那次事故的形态就是清理动作自己
// 把库拖住。批之间返回调用方，让摄入有机会插进来。
const purgeBatch = 5000

// RetainedFrom 读出一个集群的流量保留水位。
//
// 第二个返回值为 false 表示从未清理过。**与"清理到了纪元 0"分得开**：
// 前者的含义是"这个集群的连接一条都没被删过"，后者会让读取端把每一次
// 查询都判成落在保留期内。
func (s *Store) RetainedFrom(ctx context.Context, clusterID string) (time.Time, bool, error) {
	var at sql.NullTime
	err := s.db.QueryRowContext(ctx,
		`SELECT retained_from FROM flow_retention WHERE cluster_id = ?`, clusterID).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("snapshotstore: read flow retention: %w", err)
	}
	if !at.Valid {
		return time.Time{}, false, nil
	}
	return at.Time, true, nil
}

// PurgeConnectionsBefore 删掉水位之前的连接，一次最多 purgeBatch 行。
//
// 返回这一批删了多少行；调用方据此决定要不要再来一次。删到 0 行表示
// 水位之前已经没有剩余。
//
// **水位在删除之后才推进，且只推进到真正删干净的位置。** 反过来（先记水位
// 再删）在中途失败时会留下一个说谎的水位：读取端据此报"这段已清理"，
// 而那段数据其实还在，于是一次可以正常回答的查询被拒绝。而清理是分批跑的，
// 中途停下（进程重启、记账停摆、磁盘告急）是常态不是异常。
func (s *Store) PurgeConnectionsBefore(
	ctx context.Context, clusterID string, before time.Time,
) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM observed_connection
		  WHERE cluster_id = ? AND window_end < ?
		  LIMIT ?`,
		clusterID, before.UTC(), purgeBatch)
	if err != nil {
		return 0, fmt.Errorf("snapshotstore: purge connections: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("snapshotstore: purge connections rows: %w", err)
	}
	return int(n), nil
}

// AdvanceRetainedFrom 把保留水位推进到 at。
//
// **只前进，不后退。** 一个后退的水位会让读取端把一段已经删掉的时间重新
// 判成"还留着"，于是那次查询答出零条而不是"已清理" —— 正是这套机制存在
// 的理由（见 migrations/000038）。并发的两轮清理、或一次回放旧配置，
// 都可能带来一个更早的值。
func (s *Store) AdvanceRetainedFrom(ctx context.Context, clusterID string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO flow_retention (cluster_id, retained_from, updated_at)
		 VALUES (?, ?, UTC_TIMESTAMP(6))
		 ON DUPLICATE KEY UPDATE
		   retained_from = GREATEST(COALESCE(retained_from, ?), ?),
		   updated_at = UTC_TIMESTAMP(6)`,
		clusterID, at.UTC(), at.UTC(), at.UTC())
	if err != nil {
		return fmt.Errorf("snapshotstore: advance flow retention: %w", err)
	}
	return nil
}
