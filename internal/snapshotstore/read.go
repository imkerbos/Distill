package snapshotstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ResourceCount 是一类资源在某次运行中的记录条数。
type ResourceCount struct {
	Resource string `json:"resource"`
	Count    int    `json:"count"`
}

// FailureRecord 是一类资源在某次运行中的失败。
type FailureRecord struct {
	Resource string `json:"resource"`
	Reason   string `json:"reason"`
	// Detail 是原始错误文本，只供操作者阅读。
	Detail string `json:"detail"`
}

// WarningCount 是一类告警的条数。
type WarningCount struct {
	Kind  string `json:"kind"`
	Count int    `json:"count"`
}

// CollectionSummary 是一个集群最近一次采集运行的摘要。
//
// Counts、Failures 与 Warnings 三者都给出而非只报一个总体状态：
// 一次 PARTIAL 运行只说"部分失败"是没用的，操作者需要知道失败的是哪一类、
// 因为什么，才知道该去改 RBAC 还是去查网络。
type CollectionSummary struct {
	ClusterID  string    `json:"clusterId"`
	RunID      string    `json:"runId"`
	ObservedAt time.Time `json:"observedAt"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`
	// Status 取值 OK / PARTIAL / FAILED。
	Status string `json:"status"`
	// Counts 是各类资源的条数，含条数为 0 的类型。
	Counts []ResourceCount `json:"counts"`
	// Failures 是各类资源的失败记录，为空表示没有资源采集失败。
	Failures []FailureRecord `json:"failures"`
	// Warnings 是各类告警的条数。
	//
	// 与 Failures 分开：告警说的是采到的事实与注册表登记不符，
	// 采集本身是成功的。合并会让一次成功的采集看起来像是出了故障。
	Warnings []WarningCount `json:"warnings"`
	// WarningTotal 是告警总条数。
	WarningTotal int `json:"warningTotal"`
}

// ErrNoRun 表示这个集群还没有过任何一次采集运行。
//
// 与"运行过但什么都没采到"必须区分：前者是这个集群从未被采集，
// 后者是采集失败。把两者都渲染成一张空表会让一次持续的采集故障
// 看起来和一个刚注册的集群完全一样。
var ErrNoRun = errors.New("snapshotstore: cluster has no collection run")

// Latest 返回一个集群最近一次采集运行的摘要。
//
// 按 observed_at 取最新而非按 run_id：run_id 是随机串，没有顺序。
func (s *Store) Latest(ctx context.Context, clusterID string) (CollectionSummary, error) {
	var out CollectionSummary
	out.ClusterID = clusterID

	err := s.db.QueryRowContext(ctx,
		`SELECT run_id, observed_at, started_at, finished_at, status
		   FROM collection_run
		  WHERE cluster_id = ?
		  ORDER BY observed_at DESC
		  LIMIT 1`, clusterID).
		Scan(&out.RunID, &out.ObservedAt, &out.StartedAt, &out.FinishedAt, &out.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return CollectionSummary{}, ErrNoRun
	}
	if err != nil {
		return CollectionSummary{}, fmt.Errorf("snapshotstore: read latest run: %w", err)
	}

	if out.Counts, err = s.readCounts(ctx, clusterID, out.RunID); err != nil {
		return CollectionSummary{}, err
	}
	if out.Failures, err = s.readFailures(ctx, clusterID, out.RunID); err != nil {
		return CollectionSummary{}, err
	}
	if out.Warnings, out.WarningTotal, err = s.readWarnings(ctx, clusterID, out.RunID); err != nil {
		return CollectionSummary{}, err
	}
	return out, nil
}

func (s *Store) readCounts(ctx context.Context, clusterID, runID string) ([]ResourceCount, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT resource, item_count FROM collection_run_resource
		  WHERE cluster_id = ? AND run_id = ? ORDER BY resource`, clusterID, runID)
	if err != nil {
		return nil, fmt.Errorf("snapshotstore: read resource counts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []ResourceCount{}
	for rows.Next() {
		var c ResourceCount
		if err := rows.Scan(&c.Resource, &c.Count); err != nil {
			return nil, fmt.Errorf("snapshotstore: scan resource count: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("snapshotstore: iterate resource counts: %w", err)
	}
	return out, nil
}

func (s *Store) readFailures(ctx context.Context, clusterID, runID string) ([]FailureRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT resource, reason, detail FROM collection_run_failure
		  WHERE cluster_id = ? AND run_id = ? ORDER BY resource`, clusterID, runID)
	if err != nil {
		return nil, fmt.Errorf("snapshotstore: read failures: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []FailureRecord{}
	for rows.Next() {
		var f FailureRecord
		if err := rows.Scan(&f.Resource, &f.Reason, &f.Detail); err != nil {
			return nil, fmt.Errorf("snapshotstore: scan failure: %w", err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("snapshotstore: iterate failures: %w", err)
	}
	return out, nil
}

// readWarnings 按类别聚合告警，并返回总条数。
//
// 总数单独查询而非把聚合结果相加：两者对不上时说明有告警的 kind 落在
// 封闭枚举之外，而那正是"新增了一个原因却忘了同步统计口径"的迹象
// （CLAUDE.md §3）。相加会让这种不一致永远看不出来。
func (s *Store) readWarnings(ctx context.Context, clusterID, runID string) ([]WarningCount, int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT kind, COUNT(*) FROM collection_warning
		  WHERE cluster_id = ? AND run_id = ? GROUP BY kind ORDER BY kind`, clusterID, runID)
	if err != nil {
		return nil, 0, fmt.Errorf("snapshotstore: read warnings: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []WarningCount{}
	for rows.Next() {
		var w WarningCount
		if err := rows.Scan(&w.Kind, &w.Count); err != nil {
			return nil, 0, fmt.Errorf("snapshotstore: scan warning: %w", err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("snapshotstore: iterate warnings: %w", err)
	}

	var total int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM collection_warning WHERE cluster_id = ? AND run_id = ?`,
		clusterID, runID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("snapshotstore: count warnings: %w", err)
	}
	return out, total, nil
}
