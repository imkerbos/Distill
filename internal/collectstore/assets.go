package collectstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/imkerbos/Distill/internal/flow"
	"github.com/imkerbos/Distill/internal/identity"
	"github.com/imkerbos/Distill/internal/replay"
)

// observedPod 是一次快照里与本轮有关的那几列。
//
// 只取用得上的列，不回放整行（安全规范 §20 / §35）。
type observedPod struct {
	namespace   string
	labels      map[string]string
	hostNetwork bool
}

// observedPolicy 是一条落库的 NetworkPolicy：它在哪个命名空间、原文是什么。
type observedPolicy struct {
	namespace string
	manifest  string
}

// readIntervals 读出一个集群的全部身份区间，按地址分组。
//
// 按地址取全部而不是只取覆盖某一刻的那些，理由见 described.intervals：
// identity.Resolve 要靠一个地址的完整区间集合才分得清 NOT_COVERED 与
// NO_DATA，而这两者落在两个不同的 UNKNOWN 原因上。
//
// 每一条查询都带 cluster_id（CLAUDE.md §4）：不同集群 Pod CIDR 可能重叠，
// 漏掉它会把另一个集群的 Pod 解释成本集群的，且不报错。查询走主键前缀
// (cluster_id, pod_ip)，不做全表扫描。
//
// 超出上限报错而不截断：截断会让一部分地址凭空解不出主体，于是那些连接
// 被算进 UNKNOWN —— 一个读取上限伪装成关于集群的观测结论。
func (r *Reader) readIntervals(
	ctx context.Context, clusterID string,
) (map[string][]identity.Interval, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT pod_ip, valid_from, valid_to, namespace, pod_name, pod_uid,
		        workload_kind, workload_name, host_network, in_mesh
		   FROM pod_identity_interval
		  WHERE cluster_id = ?
		  ORDER BY pod_ip, valid_from
		  LIMIT ?`,
		clusterID, maxIntervalRows+1)
	if err != nil {
		return nil, fmt.Errorf("collectstore: read identity intervals: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string][]identity.Interval{}
	total := 0
	for rows.Next() {
		iv := identity.Interval{ClusterID: clusterID}
		var validTo sql.NullTime
		if err := rows.Scan(&iv.PodIP, &iv.ValidFrom, &validTo, &iv.Identity.Namespace,
			&iv.Identity.PodName, &iv.Identity.PodUID, &iv.Identity.WorkloadKind,
			&iv.Identity.WorkloadName, &iv.Identity.HostNetwork, &iv.Identity.InMesh); err != nil {
			return nil, fmt.Errorf("collectstore: scan identity interval: %w", err)
		}
		// NULL 是"至今未关闭"，落回零值时刻 —— identity.Interval.Open() 认的
		// 就是这个。填一个"很远的未来"会让"还没结束"与"结束于 9999 年"
		// 变成同一个值（migrations/000012 的同一条理由）。
		if validTo.Valid {
			iv.ValidTo = validTo.Time
		}
		out[iv.PodIP] = append(out[iv.PodIP], iv)
		total++
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("collectstore: iterate identity intervals: %w", err)
	}
	if total > maxIntervalRows {
		return nil, fmt.Errorf(
			"collectstore: cluster %s holds more than %d identity intervals; "+
				"prune the retention window rather than describing part of it",
			clusterID, maxIntervalRows)
	}
	return out, nil
}

// readPodsAt 读出锚点那一次采集看到的 Pod。
//
// 标签只存在于快照里 —— 区间表刻意不带标签（它存的是主体，不是主体的
// 属性）。策略覆盖率必须拿标签去比 selector，因此这一份从 observed_pod 取，
// 且取的是**覆盖那一刻的那次运行**，不是最新一次。
func (r *Reader) readPodsAt(ctx context.Context, d described) ([]observedPod, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT namespace, labels, host_network
		   FROM observed_pod
		  WHERE cluster_id = ? AND observed_at = ?
		  LIMIT ?`,
		d.clusterID, d.anchor, maxSnapshotRows+1)
	if err != nil {
		return nil, fmt.Errorf("collectstore: read observed pods: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []observedPod
	for rows.Next() {
		var (
			p   observedPod
			raw []byte
		)
		if err := rows.Scan(&p.namespace, &raw, &p.hostNetwork); err != nil {
			return nil, fmt.Errorf("collectstore: scan observed pod: %w", err)
		}
		if err := json.Unmarshal(raw, &p.labels); err != nil {
			// 标签列坏了就是坏了，不当成"这个 Pod 没有标签"：后者会让它被
			// 判成没有被任何 selector 覆盖，于是一次列损坏显示成一个裸 Pod。
			return nil, fmt.Errorf("collectstore: decode pod labels of cluster %s: %w", d.clusterID, err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("collectstore: iterate observed pods: %w", err)
	}
	if len(out) > maxSnapshotRows {
		return nil, fmt.Errorf(
			"collectstore: cluster %s observed more than %d pods at %s",
			d.clusterID, maxSnapshotRows, d.anchor.UTC().Format("2006-01-02T15:04:05Z07:00"))
	}
	return out, nil
}

// readPoliciesAt 读出锚点那一次采集看到的 NetworkPolicy 原文。
func (r *Reader) readPoliciesAt(ctx context.Context, d described) ([]observedPolicy, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT namespace, manifest
		   FROM observed_network_policy
		  WHERE cluster_id = ? AND observed_at = ?
		  LIMIT ?`,
		d.clusterID, d.anchor, maxSnapshotRows+1)
	if err != nil {
		return nil, fmt.Errorf("collectstore: read observed policies: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []observedPolicy
	for rows.Next() {
		var p observedPolicy
		if err := rows.Scan(&p.namespace, &p.manifest); err != nil {
			return nil, fmt.Errorf("collectstore: scan observed policy: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("collectstore: iterate observed policies: %w", err)
	}
	if len(out) > maxSnapshotRows {
		return nil, fmt.Errorf(
			"collectstore: cluster %s observed more than %d network policies at %s",
			d.clusterID, maxSnapshotRows, d.anchor.UTC().Format("2006-01-02T15:04:05Z07:00"))
	}
	return out, nil
}

// subjectAt 回答一条连接的一端在被描述的那个窗口里属于谁。
//
// 主体一律由区间表按时刻解出，**不用来源自己报的那份身份**：Hubble 的流量
// 自带 Pod 标签而 VPC flow logs 只有地址，区间表正是为了让两者得到同一种
// 答案才建的（design doc §5）。来源报的那份留在事实层里做出处，不参与这里
// 的判断。
//
// 解出来之后再问一句"这个归属在整个窗口里稳不稳"：事实层按窗口存连接、
// 不带逐条时间戳，因此这一批连接只能用窗口起点解释。地址在窗口中途换过
// 主体时，起点那个答案对窗口后半段是错的 —— 而它答得出、不报错。这时
// 一律降级成 AMBIGUOUS：归属在这个窗口里本来就不唯一，与解析器对重叠
// 区间的处置是同一句话（identity.Resolve）。
func (d described) subjectAt(ep flow.Endpoint) (identity.Identity, identity.Outcome) {
	ivs := d.intervals[ep.IP]
	subject, outcome := identity.Resolve(ivs, d.at)
	if outcome == identity.OutcomeResolved && !stableAcross(ivs, d.window) {
		return identity.Identity{}, identity.OutcomeAmbiguous
	}
	return subject, outcome
}

// stableAcross 报告这个地址的归属在窗口内有没有换过手。
//
// 判据是"有没有任何区间的端点落在窗口内部"：一次交接必然在窗口中间留下
// 一个起点或一个终点。落在边界上的不算 —— 区间是左闭右开的，窗口也是，
// 恰好在 window.From 开始的区间覆盖的正是整个窗口的开头。
func stableAcross(intervals []identity.Interval, w flow.Window) bool {
	for _, iv := range intervals {
		if iv.ValidFrom.After(w.From) && iv.ValidFrom.Before(w.To) {
			return false
		}
		if iv.Open() {
			continue
		}
		if iv.ValidTo.After(w.From) && iv.ValidTo.Before(w.To) {
			return false
		}
	}
	return true
}

// unknownReasonFor 把两端的解析结果与窗口完整度映射成一个封闭枚举里的原因。
//
// 取值与顺序都来自 design doc §3 那张表，**一个都不新增**：这些原因当初就是
// 为这一刻设计的，新增取值要同步改枚举、前端文案与统计口径。
//
// 身份类的原因压过完整度：两端都解不出来时，"这条连接归属不明"是比"这段
// 观测可能漏了"更靠前的一条线索，而排查方向完全不同。
func unknownReasonFor(src, dst identity.Outcome, c flow.Completeness) replay.UnknownReason {
	switch {
	case src == identity.OutcomeAmbiguous || dst == identity.OutcomeAmbiguous:
		return replay.ReasonIPAmbiguous
	case src == identity.OutcomeNotCovered || dst == identity.OutcomeNotCovered:
		// 锚点那一刻确实有快照（describe 拿不到锚点就不会走到这里），
		// 因此"那时这个地址上没有 Pod"是一个结论，不是一次弃权。
		return replay.ReasonExternalNoIdentity
	case src != identity.OutcomeResolved || dst != identity.OutcomeResolved:
		return replay.ReasonSnapshotMissing
	case c != flow.CompletenessComplete:
		return replay.ReasonLogSampledOut
	default:
		return replay.ReasonNone
	}
}

// confidenceOf 把窗口完整度传导成判定的可信度（design doc §4）。
//
// 完整度不是 COMPLETE 就是 DEGRADED：观测不全时给出的每一句话，与观测完整
// 时的同一句话含义不同。verdict 与 confidence 始终是两个字段，这里只填后者。
func confidenceOf(c flow.Completeness) string {
	if c == flow.CompletenessComplete {
		return string(replay.ConfidenceTrusted)
	}
	return string(replay.ConfidenceDegraded)
}
