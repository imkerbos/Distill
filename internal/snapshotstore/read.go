package snapshotstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

// FailureRecord 说明一类资源为什么没有被采到。
//
// Reason 与 Detail 分开的理由与 unknown_reason 相同（CLAUDE.md §3）：
// 统计口径只认封闭枚举，自由文本进了统计就再也对不上账。
type FailureRecord struct {
	// Reason 取值 FORBIDDEN / NOT_FOUND / TIMEOUT / UNAVAILABLE / OTHER。
	Reason string `json:"reason"`
	// Detail 是原始错误文本，只供操作者判断该改 RBAC 还是该查网络。
	Detail string `json:"detail"`
}

// ResourceOutcome 是一类资源在一次采集运行中的结果：要么是一个条数，
// 要么是一条失败记录，不会两者都是。
//
// 计数与失败合成一个类型，而不是摘要上两张并列的表 —— 分开时，
// 一个"只渲染计数"的可见面是随手就能写出来的，而计数表里
// 「NetworkPolicy = 0」与「NetworkPolicy 因权限不足根本没采到」
// 长得一模一样（spec §4.2）。那个可见面会把「我们没被授权看策略」
// 显示成「这个集群没有任何策略」，而后者会让平台推荐一份 default-deny。
//
// 因此条数字段不导出：包外唯一取到数字的途径是 Count()，它的第二个
// 返回值就是"这一类到底采到没有"。MarshalJSON 同样二选一 —— 采集失败时
// payload 里根本没有 count 这个键，下游即使想渲染也拿不到数字。
type ResourceOutcome struct {
	// Resource 是资源类型，取值来自 snapshot.ResourceKind 的封闭枚举。
	Resource string

	count   int
	failure *FailureRecord
}

// NewObservedResource 构造一条"采到了、共 count 条"的结果。
//
// 与 NewFailedResource 分成两个构造器，而不是一个带可选失败参数的：
// 两个构造器各自只收得下一种事实，于是"既报了条数又报了失败"这种取值
// 在包外根本构造不出来 —— 与 MarshalJSON 二选一同一条约束的另一半。
//
// 导出的理由是消费方要能在没有数据库的情况下测自己（比如
// internal/httpapi 的可见面），而字段不导出是这个类型的核心约束，
// 不会为了测试放开。
func NewObservedResource(resource string, count int) ResourceOutcome {
	return ResourceOutcome{Resource: resource, count: count}
}

// NewFailedResource 构造一条"这一类没采到"的结果。
//
// 没有 count 参数：失败的资源没有条数可言，给它一个参数就是给调用方
// 一个填 0 的位置，而那个 0 会被读成"这个集群没有任何这类资源"。
func NewFailedResource(resource string, record FailureRecord) ResourceOutcome {
	return ResourceOutcome{Resource: resource, failure: &record}
}

// Count 返回这类资源观测到的条数。
//
// observed 为 false 表示这一类没有采到，此时返回的数字没有任何含义，
// 调用方必须改看 Failure()。
func (o ResourceOutcome) Count() (count int, observed bool) {
	if o.failure != nil {
		return 0, false
	}
	return o.count, true
}

// Failure 返回这类资源没被采到的原因，failed 为 false 表示采集成功。
func (o ResourceOutcome) Failure() (record FailureRecord, failed bool) {
	if o.failure == nil {
		return FailureRecord{}, false
	}
	return *o.failure, true
}

// MarshalJSON 输出 count 与 failure 中的恰好一个。
//
// 不是风格选择：这是上面那条约束在网络边界上的落实。采集失败的资源
// 在 JSON 里没有 count 键，于是"渲染一个数字却不看失败"在前端也
// 写不出来 —— 数字根本不在报文里。
func (o ResourceOutcome) MarshalJSON() ([]byte, error) {
	if o.failure != nil {
		return json.Marshal(struct {
			Resource string        `json:"resource"`
			Failure  FailureRecord `json:"failure"`
		}{Resource: o.Resource, Failure: *o.failure})
	}
	return json.Marshal(struct {
		Resource string `json:"resource"`
		Count    int    `json:"count"`
	}{Resource: o.Resource, Count: o.count})
}

// WarningCount 是一类告警的条数。
type WarningCount struct {
	Kind  string `json:"kind"`
	Count int    `json:"count"`
}

// CollectionSummary 是一个集群最近一次采集运行的摘要：跑在什么时候、
// 判定是什么、看见了什么、以及有什么没看见。
//
// 只给可见面需要的这几项，不回放十一张表的内容（安全规范 §20 / §35）。
type CollectionSummary struct {
	ClusterID string `json:"clusterId"`
	RunID     string `json:"runId"`
	// ObservedAt 是这批记录共用的观测时刻，也是各 observed_* 表的 join 键。
	ObservedAt time.Time `json:"observedAt"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`
	// Status 取值 OK / PARTIAL / FAILED。
	Status string `json:"status"`
	// Resources 是各类资源的结果，按资源名排序，一类至多一条。
	//
	// 只列这次运行留下了记录的资源类型，不按枚举补齐：ResourceReplicaSet
	// 在枚举里但从不计数（它只用于解 owner 链，不是被观测的资产），
	// 补齐会把它的缺席变成一条凭空捏造的失败（spec §4.2）。
	Resources []ResourceOutcome `json:"resources"`
	// Warnings 是各类告警的条数。
	//
	// 与资源失败分开：告警说的是采到的事实与注册表登记不符，采集本身是
	// 成功的。合并会让一次成功的采集看起来像是出了故障。
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
// 每一条查询都带 cluster_id：不同集群的 Pod CIDR 可以重叠，
// 漏掉它会让摘要落到另一个集群的运行上且不报错（CLAUDE.md §4）。
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

	if out.Resources, err = s.readResources(ctx, clusterID, out.RunID); err != nil {
		return CollectionSummary{}, err
	}
	if out.Warnings, out.WarningTotal, err = s.readWarnings(ctx, clusterID, out.RunID); err != nil {
		return CollectionSummary{}, err
	}
	return out, nil
}

// readResources 把计数表与失败表合成每类资源一条结果。
//
// 两张表取并集而不是以计数表为准：一类资源可能只在失败表里出现
// （采集器根本没能列出它，也就没有计数可写）。以计数表为准会让这种
// 失败在摘要上完全消失 —— 缺席被读成"这一类没有任何东西"。
func (s *Store) readResources(ctx context.Context, clusterID, runID string) ([]ResourceOutcome, error) {
	counts, err := s.readCounts(ctx, clusterID, runID)
	if err != nil {
		return nil, err
	}
	failures, err := s.readFailures(ctx, clusterID, runID)
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(counts)+len(failures))
	for resource := range counts {
		names = append(names, resource)
	}
	for resource := range failures {
		if _, ok := counts[resource]; !ok {
			names = append(names, resource)
		}
	}
	slices.Sort(names)

	out := make([]ResourceOutcome, 0, len(names))
	for _, resource := range names {
		outcome := ResourceOutcome{Resource: resource, count: counts[resource]}
		// 失败压过计数：一次 FORBIDDEN 的 NetworkPolicy 同样留下一行
		// item_count = 0，把那个 0 交出去就是把"没被授权"说成"没有策略"。
		if failure, ok := failures[resource]; ok {
			outcome.count = 0
			outcome.failure = &failure
		}
		out = append(out, outcome)
	}
	return out, nil
}

func (s *Store) readCounts(ctx context.Context, clusterID, runID string) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT resource, item_count FROM collection_run_resource
		  WHERE cluster_id = ? AND run_id = ?`, clusterID, runID)
	if err != nil {
		return nil, fmt.Errorf("snapshotstore: read resource counts: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]int{}
	for rows.Next() {
		var (
			resource string
			count    int
		)
		if err := rows.Scan(&resource, &count); err != nil {
			return nil, fmt.Errorf("snapshotstore: scan resource count: %w", err)
		}
		out[resource] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("snapshotstore: iterate resource counts: %w", err)
	}
	return out, nil
}

func (s *Store) readFailures(ctx context.Context, clusterID, runID string) (map[string]FailureRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT resource, reason, detail FROM collection_run_failure
		  WHERE cluster_id = ? AND run_id = ?`, clusterID, runID)
	if err != nil {
		return nil, fmt.Errorf("snapshotstore: read failures: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]FailureRecord{}
	for rows.Next() {
		var (
			resource string
			record   FailureRecord
		)
		if err := rows.Scan(&resource, &record.Reason, &record.Detail); err != nil {
			return nil, fmt.Errorf("snapshotstore: scan failure: %w", err)
		}
		out[resource] = record
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("snapshotstore: iterate failures: %w", err)
	}
	return out, nil
}

// readWarnings 按类别聚合告警，并返回总条数。
//
// 聚合而非逐条返回：告警条数没有上界（一个网段填错的集群会给每个 Pod
// 记一条），逐条返回会让一次读取拖回几千行（安全规范 §23 / §24）。
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
