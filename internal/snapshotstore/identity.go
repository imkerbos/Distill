package snapshotstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/imkerbos/Distill/internal/identity"
	"github.com/imkerbos/Distill/internal/snapshot"
)

// DeriveIdentityIntervals 由一次已落库的采集运行推导 Pod 身份区间。
//
// 范围限于该集群、该次运行（spec §6）：读的是这次运行的 Pod 观测，加上本
// 集群当前还开着的区间，不做全表重算。全表重算的成本随历史无上限增长，
// 而它要解决的问题只发生在最新那一段。
//
// 幂等：重试、补跑与并发都会让同一次运行被推导多次，第二遍必须让表逐字节
// 不变，而不是再追加一行。这里的做法是让第二遍**一条写语句都不发** ——
// 已存在的行只在可变列真的变了时才 UPDATE。
//
// 不写审计行（spec §8）：审计记的是人做了什么，一次自动推导写一行只会稀释
// 审计的信噪比。可追溯性由 first_run_id / last_run_id 承担。
//
// 整个过程在单个事务里：一次推导要么整体生效要么整体回滚，不会留下推了一半
// 的区间表（安全规范 §25）。事务给到的只有这一条 —— **它挡不住并发的两次
// 推导**：它们都会读到同一批开区间，各自以为看到的是全部。同一次运行被推导
// 两遍时，两边的 INSERT 撞主键，后到的整体回滚，失败方向是"没推导成"；而
// 同一集群两次**不同**运行并发时走的是 UPDATE，它只在行锁上排队，随后
// 后写的赢，不报任何错 —— 一个仍在运行的 Pod 的区间被关在错的时刻，之后它的
// 地址解析成 NOT_COVERED，或者更糟，解析成后来复用这个地址的另一个工作负载。
// **归属到错误的主体，且不报错。**
//
// 因此**互斥由调用方持有，任何调用方都必须先取到它才准调这个函数**：
// 同包的 LockCluster（derivelock.go）按集群做这一层，生产上的调用点是
// cmd/distill-collector 的 deriveOnce。这个函数自己不取锁 —— 锁要横跨推导
// 与它的记账（identity_derive_run 那一行），而记账不在本函数的范围里，
// 在这里取就会在提交与记账之间留下一段没被互斥盖住的缝。
// 新加一个调用方（补跑 CLI、界面上的"重推这次采集"）时，跳过 LockCluster
// 就是把上面那段静默归属错主体的路径重新打开。
func (s *Store) DeriveIdentityIntervals(ctx context.Context, clusterID, runID string) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("snapshotstore: begin derive: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	facts, err := readRunFacts(ctx, tx, clusterID, runID)
	if err != nil {
		return err
	}
	observed, err := readObservedPods(ctx, tx, clusterID, runID, facts.ObservedAt)
	if err != nil {
		return err
	}
	prev, err := readOpenIntervals(ctx, tx, clusterID)
	if err != nil {
		return err
	}
	settled, err := readSettledIntervalsAt(ctx, tx, clusterID, facts.ObservedAt)
	if err != nil {
		return err
	}

	if err = writeIntervals(ctx, tx, prev, settled, identity.Derive(prev, observed, facts)); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("snapshotstore: commit derive: %w", err)
	}
	return nil
}

// readRunFacts 把落库的运行元数据还原成推导需要的事实。
//
// Status 与 PodsCollected 分开读、分开传，不在这里合成一个布尔：
// "这次运行可不可信"这个判断属于 identity 包，合成在这里会让那个守卫
// 被测到、却没有任何东西证明调用方还在按它的口径喂参数。
func readRunFacts(ctx context.Context, tx *sql.Tx, clusterID, runID string) (identity.RunFacts, error) {
	var (
		observedAt time.Time
		stored     string
	)
	err := tx.QueryRowContext(ctx,
		`SELECT observed_at, status FROM collection_run
		  WHERE cluster_id = ? AND run_id = ?`, clusterID, runID).
		Scan(&observedAt, &stored)
	if errors.Is(err, sql.ErrNoRows) {
		return identity.RunFacts{}, fmt.Errorf(
			"snapshotstore: cannot derive intervals for run %s of cluster %s: %w",
			runID, clusterID, ErrNoRun)
	}
	if err != nil {
		return identity.RunFacts{}, fmt.Errorf("snapshotstore: read run facts: %w", err)
	}

	status, ok := runStatusOf(stored)
	if !ok {
		// 拒绝而不是当成"不可信运行"继续：不可信只挡住关闭，仍然会**开出**
		// 一批区间，并把它们记在一次这段代码读不懂的运行头上。一个读不懂的
		// 状态是"枚举加了取值、这里没跟上"的迹象，那种时候不推导才是对的。
		return identity.RunFacts{}, fmt.Errorf(
			"snapshotstore: run %s of cluster %s carries a status this code has not registered; "+
				"refusing to derive intervals from a run whose trustworthiness cannot be judged",
			runID, clusterID)
	}

	collected, err := podsCollected(ctx, tx, clusterID, runID)
	if err != nil {
		return identity.RunFacts{}, err
	}
	return identity.RunFacts{
		ClusterID:     clusterID,
		RunID:         runID,
		ObservedAt:    observedAt,
		Status:        status,
		PodsCollected: collected,
	}, nil
}

// runStatusOf 把落库的运行状态映射进 identity 的封闭枚举。
//
// 显式映射而非 identity.RunStatus(stored)：两个枚举眼下逐字相同，而一次
// 强制转换会让它们各自演化时静默失配 —— 其中一个新增取值、另一个把它当成
// 未登记值，于是"这次运行能不能关区间"由一个谁也没写过的分支决定。
// 转换写起来更短，但它恰好在两个封闭枚举之间取消了唯一的对账点。
//
// 显式 switch 而非查表：漏掉一个取值的那一行缺失在评审里看得见。
func runStatusOf(stored string) (identity.RunStatus, bool) {
	switch snapshot.RunStatus(stored) {
	case snapshot.RunOK:
		return identity.RunOK, true
	case snapshot.RunPartial:
		return identity.RunPartial, true
	case snapshot.RunFailed:
		return identity.RunFailed, true
	default:
		return "", false
	}
}

// podsCollected 判断 Pod 这一类在这次运行里是不是真的采到了。
//
// 计数存在、且没有失败记录，才算采到。失败压过计数：一次 FORBIDDEN 的 Pod
// 采集同样留下一行 item_count = 0，把那个 0 读成"这个集群没有 Pod"，
// 就会让这次运行去关掉每一个开区间 —— 一次权限故障变成一批工作负载下线，
// 于是它们的策略被判成"无流量、可收紧"（spec §3）。
//
// 缺行也算没采到：一次中止的运行（SaveAbortedRun）两张表都不写，
// 而它恰恰是最不该关区间的那种运行。
func podsCollected(ctx context.Context, tx *sql.Tx, clusterID, runID string) (bool, error) {
	var counted, failed bool
	if err := tx.QueryRowContext(ctx,
		`SELECT
		   EXISTS(SELECT 1 FROM collection_run_resource
		           WHERE cluster_id = ? AND run_id = ? AND resource = ?),
		   EXISTS(SELECT 1 FROM collection_run_failure
		           WHERE cluster_id = ? AND run_id = ? AND resource = ?)`,
		clusterID, runID, string(snapshot.ResourcePod),
		clusterID, runID, string(snapshot.ResourcePod)).
		Scan(&counted, &failed); err != nil {
		return false, fmt.Errorf("snapshotstore: read pod collection outcome: %w", err)
	}
	return counted && !failed, nil
}

// readObservedPods 读出这次运行看到的全部 Pod。
//
// 只取还原身份要用的那几列，不回放整行（安全规范 §20 / §35）。
// 条件带 (cluster_id, observed_at)（CLAUDE.md §4）：run_id 单独用不住，
// 它既不带集群也不带时刻，而这是一条历史查询。
func readObservedPods(
	ctx context.Context,
	tx *sql.Tx,
	clusterID, runID string,
	observedAt time.Time,
) ([]identity.ObservedPod, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT ip, extra_ips, namespace, name, uid,
		        workload_kind, workload_name, host_network, in_mesh
		   FROM observed_pod
		  WHERE cluster_id = ? AND observed_at = ? AND run_id = ?`,
		clusterID, observedAt, runID)
	if err != nil {
		return nil, fmt.Errorf("snapshotstore: read observed pods: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []identity.ObservedPod
	for rows.Next() {
		var (
			p     identity.ObservedPod
			extra []byte
		)
		if err := rows.Scan(&p.PodIP, &extra, &p.Identity.Namespace, &p.Identity.PodName,
			&p.Identity.PodUID, &p.Identity.WorkloadKind, &p.Identity.WorkloadName,
			&p.Identity.HostNetwork, &p.Identity.InMesh); err != nil {
			return nil, fmt.Errorf("snapshotstore: scan observed pod: %w", err)
		}
		out = append(out, p)

		// **每个额外地址各成一条观测。** identity.Derive 按 (地址, 主体) 建
		// 区间，因此双栈 Pod 的第二个地址只有在这里被展开成独立的一条，
		// 区间表里才会有它 —— 否则走那个地址的连接解不出主体、判 UNKNOWN，
		// 覆盖它的规则于是缺席，下发 default-deny 之后会被拦断。
		addrs, err := extraAddressesOf(extra)
		if err != nil {
			return nil, fmt.Errorf("snapshotstore: pod %s/%s: %w",
				p.Identity.Namespace, p.Identity.PodName, err)
		}
		for _, a := range addrs {
			alt := p
			alt.PodIP = a
			out = append(out, alt)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("snapshotstore: iterate observed pods: %w", err)
	}
	return out, nil
}

// readOpenIntervals 读出这个集群当前还开着的区间。
//
// 只读开区间：已关闭的区间不会再被任何一次推导改动，把它们一起捞出来只会
// 让单次推导的读取量随历史线性增长，而开区间的条数与在跑的 Pod 同阶。
// 这就是 spec §6 那句"不做全表重算"在读侧的落点。
//
// 不读 closed_reason：开区间的关闭原因按定义是空的，读回来只是给自己一个
// 把它写回去的机会。
func readOpenIntervals(ctx context.Context, tx *sql.Tx, clusterID string) ([]identity.Interval, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT pod_ip, valid_from, namespace, pod_name, pod_uid,
		        workload_kind, workload_name, host_network, in_mesh,
		        first_run_id, last_run_id
		   FROM pod_identity_interval
		  WHERE cluster_id = ? AND valid_to IS NULL`, clusterID)
	if err != nil {
		return nil, fmt.Errorf("snapshotstore: read open intervals: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []identity.Interval
	for rows.Next() {
		iv := identity.Interval{ClusterID: clusterID}
		if err := rows.Scan(&iv.PodIP, &iv.ValidFrom, &iv.Identity.Namespace,
			&iv.Identity.PodName, &iv.Identity.PodUID, &iv.Identity.WorkloadKind,
			&iv.Identity.WorkloadName, &iv.Identity.HostNetwork, &iv.Identity.InMesh,
			&iv.FirstRunID, &iv.LastRunID); err != nil {
			return nil, fmt.Errorf("snapshotstore: scan interval: %w", err)
		}
		out = append(out, iv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("snapshotstore: iterate intervals: %w", err)
	}
	return out, nil
}

// readSettledIntervalsAt 读出这个集群里起点正好是 at、且已经关闭的区间。
//
// 它们不喂给 Derive —— 已关闭的区间不参与推导。读它们只为补跑：补跑一次
// 旧运行时，那次运行看到的 Pod 会被重新开出一个起点相同的区间，而它早已
// 被后来的运行关掉了。认出它就什么都不写，认不出就撞主键报错 ——
// 于是"补跑一次已经推导过的运行"从一个失败变回一个空操作（spec §6）。
//
// 只取起点等于 at 的那些：能与本次推导产物撞主键的只有它们，范围仍然是
// 该集群、该次运行。
func readSettledIntervalsAt(
	ctx context.Context,
	tx *sql.Tx,
	clusterID string,
	at time.Time,
) (map[intervalKey]identity.Identity, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT pod_ip, namespace, pod_name, pod_uid, workload_kind, workload_name,
		        host_network, in_mesh
		   FROM pod_identity_interval
		  WHERE cluster_id = ? AND valid_from = ? AND valid_to IS NOT NULL`,
		clusterID, at)
	if err != nil {
		return nil, fmt.Errorf("snapshotstore: read settled intervals: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[intervalKey]identity.Identity{}
	for rows.Next() {
		var (
			podIP string
			id    identity.Identity
		)
		if err := rows.Scan(&podIP, &id.Namespace, &id.PodName, &id.PodUID,
			&id.WorkloadKind, &id.WorkloadName, &id.HostNetwork, &id.InMesh); err != nil {
			return nil, fmt.Errorf("snapshotstore: scan settled interval: %w", err)
		}
		out[intervalKey{podIP: podIP, validFrom: at, podUID: id.PodUID}] = id
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("snapshotstore: iterate settled intervals: %w", err)
	}
	return out, nil
}

// intervalKey 是区间表的主键。集群在一次推导里是固定的，只带后三项。
//
// podUID 在键里（spec §2）：同一个节点上的多个 hostNetwork Pod 共用节点
// 地址，同一时刻同一地址上有多个主体是常态而非故障，键必须放得下它们。
type intervalKey struct {
	podIP     string
	validFrom time.Time
	podUID    string
}

func keyOf(iv identity.Interval) intervalKey {
	return intervalKey{podIP: iv.PodIP, validFrom: iv.ValidFrom, podUID: iv.Identity.PodUID}
}

// writeIntervals 把推导结果写回。
//
// prev 是本次推导读到的那批开区间，settled 是起点与本次相同、但已经关闭的
// 那些。拿两者做对照而不是无脑 upsert：已存在的行只在 valid_to /
// last_run_id / closed_reason / workload_* 真的变了时才发一条 UPDATE，
// 已关闭的行一个字都不改。于是"对同一次运行重跑"在第二遍一条写语句都不发
// —— 幂等不是靠事后比对得出的，是靠没有写发生。
func writeIntervals(ctx context.Context, tx *sql.Tx, prev []identity.Interval,
	settled map[intervalKey]identity.Identity, next []identity.Interval) error {
	before := make(map[intervalKey]identity.Interval, len(prev))
	for _, iv := range prev {
		before[keyOf(iv)] = iv
	}

	seen := make(map[intervalKey]identity.Identity, len(next))
	for _, iv := range next {
		key := keyOf(iv)
		if other, dup := seen[key]; dup {
			return conflictingIntervals(iv, other)
		}
		seen[key] = iv.Identity
	}

	for _, iv := range next {
		key := keyOf(iv)
		if was, done := settled[key]; done {
			// 已经关掉的那一段就是这次观测的产物，补跑不带来新事实。
			// 比主体而不比整个 Identity：补跑的那次运行可能正是工作负载归属
			// 退化的那次，而已关闭的区间是历史，不因为补跑而改写。
			// 主体对不上则是另一回事：同一时刻同一地址上的两个主体。
			if !was.SameSubject(iv.Identity) {
				return conflictingIntervals(iv, was)
			}
			continue
		}

		stored, exists := before[key]
		switch {
		case !exists:
			if err := insertInterval(ctx, tx, iv); err != nil {
				return err
			}
		case stored.ValidTo.Equal(iv.ValidTo) &&
			stored.LastRunID == iv.LastRunID &&
			stored.ClosedReason == iv.ClosedReason &&
			stored.Identity.WorkloadKind == iv.Identity.WorkloadKind &&
			stored.Identity.WorkloadName == iv.Identity.WorkloadName:
			// 逐字节相同，不写。
		default:
			if err := updateInterval(ctx, tx, iv); err != nil {
				return err
			}
		}
	}
	return nil
}

// conflictingIntervals 说明同一个 Pod UID 在同一时刻带着两个不同的主体。
//
// 多个主体共用一个地址是常态（hostNetwork），主键里的 pod_uid 把它们分开了。
// 剩下能撞在一起的只有"同一个 UID、同一个地址、同一时刻，namespace 之类
// 不可变项却不一样"—— UID 是 Kubernetes 给出的唯一标识，这种情形意味着
// 观测本身坏了。workload_* 不在此列：它会随采集退化，退化不是主体变了。
// 静默留下其中一个仍然答得出、仍然不报错，因此整次推导在写库之前就拒绝。
func conflictingIntervals(iv identity.Interval, other identity.Identity) error {
	return fmt.Errorf(
		"snapshotstore: refusing to derive intervals for cluster %s: address %s at %s carries pod uid %s "+
			"under two different subjects, %s/%s and %s/%s",
		iv.ClusterID, iv.PodIP, iv.ValidFrom.UTC().Format(time.RFC3339Nano), iv.Identity.PodUID,
		other.Namespace, other.PodName,
		iv.Identity.Namespace, iv.Identity.PodName)
}

func insertInterval(ctx context.Context, tx *sql.Tx, iv identity.Interval) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO pod_identity_interval
		   (cluster_id, pod_ip, valid_from, valid_to, namespace, pod_name, pod_uid,
		    workload_kind, workload_name, host_network, in_mesh,
		    first_run_id, last_run_id, closed_reason)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		iv.ClusterID, iv.PodIP, iv.ValidFrom, validTo(iv),
		iv.Identity.Namespace, iv.Identity.PodName, iv.Identity.PodUID,
		iv.Identity.WorkloadKind, iv.Identity.WorkloadName,
		iv.Identity.HostNetwork, iv.Identity.InMesh,
		iv.FirstRunID, iv.LastRunID, string(iv.ClosedReason)); err != nil {
		return fmt.Errorf("snapshotstore: insert interval for %s: %w", iv.PodIP, err)
	}
	return nil
}

// updateInterval 只改会变的那几列。
//
// 主体列（namespace / pod_name / pod_uid / host_network / in_mesh）与
// first_run_id 一律不动：它们是这个区间开启那一刻的事实，事后重写就是拿当前
// 状态改写历史（CLAUDE.md §4）。一个 IP 换了主体时该开一个新区间，而不是把
// 旧区间的主体改掉。
//
// workload_* 是例外，因为它们不是主体而是主体的标签：ReplicaSet 那一步没采到
// 时它们会退化成 ReplicaSet 自己，一次可信运行把归属解回 Deployment 后就地
// 订正，好过让一次 RBAC 缺口把这个 Pod 的归属钉死一辈子。订正只发生在这一个
// 方向上（identity.Derive 里只让可信运行写），且区间的起止与主体都不动，
// 中途读到的那个答案因此是同一个主体的粗粒度版本，不是另一个答案。
func updateInterval(ctx context.Context, tx *sql.Tx, iv identity.Interval) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE pod_identity_interval
		    SET valid_to = ?, last_run_id = ?, closed_reason = ?,
		        workload_kind = ?, workload_name = ?
		  WHERE cluster_id = ? AND pod_ip = ? AND valid_from = ? AND pod_uid = ?`,
		validTo(iv), iv.LastRunID, string(iv.ClosedReason),
		iv.Identity.WorkloadKind, iv.Identity.WorkloadName,
		iv.ClusterID, iv.PodIP, iv.ValidFrom, iv.Identity.PodUID); err != nil {
		return fmt.Errorf("snapshotstore: update interval for %s: %w", iv.PodIP, err)
	}
	return nil
}

// validTo 把开区间的零值时刻映射成 SQL NULL。
//
// 不用哨兵时刻：一个"很远的未来"在按时刻查询时会被当成一段真实的覆盖，
// 于是"还没结束"与"结束于 9999 年"变成同一个值。
func validTo(iv identity.Interval) any {
	if iv.Open() {
		return nil
	}
	return iv.ValidTo
}

// extraAddressesOf 解出额外地址列里的地址。
//
// **解不开就整次失败，不静默当成"没有额外地址"。** 后者会让一个双栈 Pod 的
// 第二个地址悄悄消失在区间表里，而症状要到某条连接解不出主体时才出现，
// 那时已经追不回成因。这一列由本包自己写入，解不开说明那一行坏了。
func extraAddressesOf(raw []byte) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var rows []podAddressRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("decode extra ips: %w", err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.IP == "" {
			continue
		}
		out = append(out, r.IP)
	}
	return out, nil
}
