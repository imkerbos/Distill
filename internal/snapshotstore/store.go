// Package snapshotstore 把采集到的资产快照落进 MySQL。
//
// 与 internal/mysqlregistry 分开：那个包存的是注册表 —— 人填进来的、
// 描述我们打算怎么看待集群的配置；这个包存的是观测到的事实。
// 两者的写入方、生命周期与保留期都不同，合在一起只会让那个包继续变大。
//
// 写入一律 append-only：每次采集写一批带 observed_at 的新行，不覆盖旧行。
// 用当前状态解释历史数据会得出答得出、又不报错的错误结论（CLAUDE.md §4）。
package snapshotstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"

	"github.com/imkerbos/Distill/internal/snapshot"
)

// Store 是资产快照的 MySQL 读写口。
type Store struct {
	db *sql.DB
}

// New 用已建立的连接池构造存储。
func New(db *sql.DB) *Store { return &Store{db: db} }

// ErrRunExists 表示这个 (cluster_id, run_id) 已经落过库。
//
// 推送式接入下 agent 跑在 CronJob 里，网络抖动重试是常态：第二次推同一个
// run_id 不是错误，是同一次采集又说了一遍（design doc 2026-08-18 §4）。
// 调用方据此答成功而不是失败 —— 塌成一个「写库失败」会让重试变成 500，
// agent 于是接着重试，而每一次都会得到同样的结果。
//
// **重复的一次不覆盖已存的那份。** 覆盖等于让后到的推送改写历史，而历史
// 正是这个平台用来解释「那时候是什么样」的东西（CLAUDE.md §4：禁止用当前
// 状态解释历史数据）。
var ErrRunExists = errors.New("snapshotstore: this collection run is already stored")

// Save 在单个事务里写入一次采集运行的全部产物。
//
// 单事务而非逐表提交：一次运行的 collection_run 与它的各 observed_* 行
// 必须同时可见。先提交计数、后提交明细会让可见面在两次提交之间报出一个
// "采到了 800 个 Pod，但一个也查不到"的状态，而这个状态与"采集器挂了"
// 无法区分。
//
// 这个 (cluster_id, run_id) 已经落过库时返回 ErrRunExists，**不覆盖**。
func (s *Store) Save(ctx context.Context, run snapshot.Run) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("snapshotstore: begin: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	if err = insertRun(ctx, tx, run); err != nil {
		return err
	}
	if err = insertObservation(ctx, tx, run.Observation); err != nil {
		return err
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("snapshotstore: commit: %w", err)
	}
	return nil
}

// SaveAbortedRun 记下一次在读到集群之前就失败的运行。
//
// 与 Save 分开而不是给它传一个空 Observation：Save 会为**每一类**资源写
// 一行计数，值为 0。那些 0 是真话时（集群里确实没有 Ingress）它们必须写，
// 而这里它们全是假话 —— 一个资源都没被尝试过。落成一排 0 会让这一屏显示
// 「采到了零个 Pod、零个 Service」，读起来像一个空集群，而事实是我们根本
// 没看过它。
//
// 因此这里只写 collection_run 一行：没有计数、没有资源失败、没有告警。
// 可见面据此渲染成"这一轮没有开始"，而不是"这一轮什么都没有"。
//
// reason 为空时拒绝：一次没有原因的失败运行，在界面上与一次采到零资源的
// 成功运行无法区分，而那正是这个方法存在的全部理由。
func (s *Store) SaveAbortedRun(
	ctx context.Context,
	clusterID, runID string,
	startedAt, finishedAt time.Time,
	reason snapshot.RunErrorReason,
) error {
	if reason == snapshot.RunErrorNone {
		return fmt.Errorf(
			"snapshotstore: refusing to record an aborted run for %s with no reason; "+
				"it would be indistinguishable from a run that observed nothing", clusterID)
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO collection_run
		   (cluster_id, run_id, observed_at, started_at, finished_at, status, error_reason)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		clusterID, runID, startedAt, startedAt, finishedAt,
		string(snapshot.RunFailed), string(reason))
	if err != nil {
		return fmt.Errorf("snapshotstore: insert aborted run: %w", err)
	}
	return nil
}

// insertRun 写入运行元数据、各资源计数、失败记录与告警。
func insertRun(ctx context.Context, tx *sql.Tx, run snapshot.Run) error {
	obs := run.Observation

	_, err := tx.ExecContext(ctx,
		`INSERT INTO collection_run
		   (cluster_id, run_id, observed_at, started_at, finished_at, status, error_reason)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		obs.ClusterID, obs.RunID, obs.ObservedAt, run.StartedAt, run.FinishedAt,
		string(run.Status), string(run.ErrorReason))
	if err != nil {
		// 唯一键冲突就是「这一次已经落过了」。用数据库的约束判重，而不是
		// 先 SELECT 再 INSERT：后者在两次并发重试之间有窗口，而 CronJob
		// 的重试恰恰可能同时到达。
		if isDuplicateKey(err) {
			return fmt.Errorf("%w: cluster %s run %s", ErrRunExists, obs.ClusterID, obs.RunID)
		}
		return fmt.Errorf("snapshotstore: insert run: %w", err)
	}

	// 计数逐类写入，包括为 0 的那些。写 0 而非跳过：一个缺行的资源类型
	// 与一个计数为 0 的资源类型，在可见面上必须能区分 ——
	// 前者是"这一类根本没被尝试"，后者是"尝试了，集群里就是没有"。
	for kind, n := range obs.Counts() {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO collection_run_resource (cluster_id, run_id, resource, item_count)
			 VALUES (?, ?, ?, ?)`,
			obs.ClusterID, obs.RunID, string(kind), n); err != nil {
			return fmt.Errorf("snapshotstore: insert resource count %s: %w", kind, err)
		}
	}

	for _, f := range run.Failures {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO collection_run_failure (cluster_id, run_id, resource, reason, detail)
			 VALUES (?, ?, ?, ?, ?)`,
			obs.ClusterID, obs.RunID, string(f.Resource), string(f.Reason), f.Detail); err != nil {
			return fmt.Errorf("snapshotstore: insert failure %s: %w", f.Resource, err)
		}
	}

	for i, w := range obs.Warnings {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO collection_warning (cluster_id, run_id, seq, kind, subject, detail)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			obs.ClusterID, obs.RunID, i, string(w.Kind), w.Subject, w.Detail); err != nil {
			return fmt.Errorf("snapshotstore: insert warning %d: %w", i, err)
		}
	}
	return nil
}

// insertObservation 写入各类资产记录。
func insertObservation(ctx context.Context, tx *sql.Tx, obs snapshot.Observation) error {
	if err := insertNamespaces(ctx, tx, obs); err != nil {
		return err
	}
	if err := insertPods(ctx, tx, obs); err != nil {
		return err
	}
	if err := insertNodes(ctx, tx, obs); err != nil {
		return err
	}
	if err := insertServices(ctx, tx, obs); err != nil {
		return err
	}
	if err := insertEndpoints(ctx, tx, obs); err != nil {
		return err
	}
	if err := insertPolicies(ctx, tx, obs); err != nil {
		return err
	}
	return insertGateways(ctx, tx, obs)
}

func insertNamespaces(ctx context.Context, tx *sql.Tx, obs snapshot.Observation) error {
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO observed_namespace
		   (cluster_id, name, observed_at, run_id, labels, in_mesh, mesh_source, mesh_detail)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("snapshotstore: prepare namespace: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, ns := range obs.Namespaces {
		labels, err := jsonObject(ns.Labels)
		if err != nil {
			return fmt.Errorf("snapshotstore: marshal namespace labels %s: %w", ns.Name, err)
		}
		if _, err := stmt.ExecContext(ctx,
			obs.ClusterID, ns.Name, obs.ObservedAt, obs.RunID,
			labels, ns.InMesh, string(ns.MeshSource), ns.MeshDetail); err != nil {
			return fmt.Errorf("snapshotstore: insert namespace %s: %w", ns.Name, err)
		}
	}
	return nil
}

func insertPods(ctx context.Context, tx *sql.Tx, obs snapshot.Observation) error {
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO observed_pod
		   (cluster_id, namespace, name, observed_at, run_id, uid, phase, ip,
		    ip_scope, ip_scope_reason, labels, host_network, node_name, service_account,
		    owner_kind, owner_name, workload_kind, workload_name,
		    in_mesh, mesh_source, mesh_detail)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("snapshotstore: prepare pod: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, p := range obs.Pods {
		labels, err := jsonObject(p.Labels)
		if err != nil {
			return fmt.Errorf("snapshotstore: marshal pod labels %s/%s: %w", p.Namespace, p.Name, err)
		}
		if _, err := stmt.ExecContext(ctx,
			obs.ClusterID, p.Namespace, p.Name, obs.ObservedAt, obs.RunID,
			p.UID, p.Phase, p.IP, string(p.IPScope), string(p.IPScopeReason),
			labels, p.HostNetwork, p.NodeName, p.ServiceAccount,
			p.OwnerKind, p.OwnerName, p.WorkloadKind, p.WorkloadName,
			p.InMesh, string(p.MeshSource), p.MeshDetail); err != nil {
			return fmt.Errorf("snapshotstore: insert pod %s/%s: %w", p.Namespace, p.Name, err)
		}
	}
	return nil
}

func insertNodes(ctx context.Context, tx *sql.Tx, obs snapshot.Observation) error {
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO observed_node (cluster_id, name, observed_at, run_id, pod_cidrs, internal_ips)
		 VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("snapshotstore: prepare node: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, n := range obs.Nodes {
		cidrs, err := jsonArray(n.PodCIDRs)
		if err != nil {
			return fmt.Errorf("snapshotstore: marshal node cidrs %s: %w", n.Name, err)
		}
		ips, err := jsonArray(n.InternalIPs)
		if err != nil {
			return fmt.Errorf("snapshotstore: marshal node ips %s: %w", n.Name, err)
		}
		if _, err := stmt.ExecContext(ctx,
			obs.ClusterID, n.Name, obs.ObservedAt, obs.RunID, cidrs, ips); err != nil {
			return fmt.Errorf("snapshotstore: insert node %s: %w", n.Name, err)
		}
	}
	return nil
}

func insertServices(ctx context.Context, tx *sql.Tx, obs snapshot.Observation) error {
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO observed_service
		   (cluster_id, namespace, name, observed_at, run_id, service_type, selector, cluster_ip, ports)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("snapshotstore: prepare service: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, s := range obs.Services {
		selector, err := jsonObject(s.Selector)
		if err != nil {
			return fmt.Errorf("snapshotstore: marshal service selector %s/%s: %w", s.Namespace, s.Name, err)
		}
		ports, err := jsonArray(s.Ports)
		if err != nil {
			return fmt.Errorf("snapshotstore: marshal service ports %s/%s: %w", s.Namespace, s.Name, err)
		}
		if _, err := stmt.ExecContext(ctx,
			obs.ClusterID, s.Namespace, s.Name, obs.ObservedAt, obs.RunID,
			s.Type, selector, s.ClusterIP, ports); err != nil {
			return fmt.Errorf("snapshotstore: insert service %s/%s: %w", s.Namespace, s.Name, err)
		}
	}
	return nil
}

func insertEndpoints(ctx context.Context, tx *sql.Tx, obs snapshot.Observation) error {
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO observed_endpoints
		   (cluster_id, namespace, name, observed_at, run_id, addresses, ports)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("snapshotstore: prepare endpoints: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, e := range obs.Endpoints {
		addrs, err := jsonArray(e.Addresses)
		if err != nil {
			return fmt.Errorf("snapshotstore: marshal endpoint addresses %s/%s: %w", e.Namespace, e.Name, err)
		}
		ports, err := jsonArray(e.Ports)
		if err != nil {
			return fmt.Errorf("snapshotstore: marshal endpoint ports %s/%s: %w", e.Namespace, e.Name, err)
		}
		if _, err := stmt.ExecContext(ctx,
			obs.ClusterID, e.Namespace, e.Name, obs.ObservedAt, obs.RunID, addrs, ports); err != nil {
			return fmt.Errorf("snapshotstore: insert endpoints %s/%s: %w", e.Namespace, e.Name, err)
		}
	}
	return nil
}

func insertPolicies(ctx context.Context, tx *sql.Tx, obs snapshot.Observation) error {
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO observed_network_policy
		   (cluster_id, namespace, name, observed_at, run_id, uid, manifest)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("snapshotstore: prepare policy: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, p := range obs.Policies {
		if _, err := stmt.ExecContext(ctx,
			obs.ClusterID, p.Namespace, p.Name, obs.ObservedAt, obs.RunID,
			p.UID, p.Manifest); err != nil {
			return fmt.Errorf("snapshotstore: insert policy %s/%s: %w", p.Namespace, p.Name, err)
		}
	}
	return nil
}

func insertGateways(ctx context.Context, tx *sql.Tx, obs snapshot.Observation) error {
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO observed_gateway
		   (cluster_id, namespace, name, backend_service, observed_at, run_id, gateway_kind)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("snapshotstore: prepare gateway: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, g := range obs.Gateways {
		if _, err := stmt.ExecContext(ctx,
			obs.ClusterID, g.Namespace, g.Name, g.BackendService,
			obs.ObservedAt, obs.RunID, g.Kind); err != nil {
			return fmt.Errorf("snapshotstore: insert gateway %s/%s: %w", g.Namespace, g.Name, err)
		}
	}
	return nil
}

// jsonObject 把标签类 map 序列化成 JSON 对象。
//
// nil 也要写成 {}：json.Marshal 对 nil map 产出字面量 null，
// 那是一个合法的 JSON 值，会被原样存进 JSON 列。读回来时"没有标签"
// 与"标签这一列坏了"就变成了同一个 null。
func jsonObject(m map[string]string) ([]byte, error) {
	if m == nil {
		m = map[string]string{}
	}
	return json.Marshal(m)
}

// jsonArray 把切片序列化成 JSON 数组，nil 写成 []，理由同 jsonObject。
func jsonArray[T any](s []T) ([]byte, error) {
	if s == nil {
		s = []T{}
	}
	return json.Marshal(s)
}

// errDuplicateEntry 是 MySQL 的唯一键冲突错误号（ER_DUP_ENTRY）。
//
// 数字写成常量而非裸写在判断里：一个裸的 1062 在 review 时与任何别的
// 数字没有区别。
const errDuplicateEntry uint16 = 1062

// isDuplicateKey 判断一次写入是不是撞上了唯一键。
//
// 与 internal/mysqlregistry 的 writeFailure 各写一份、刻意不共用：那一个
// 把错误翻译成**面向操作者**的校验失败文案，这一个翻译成一个供调用方分支
// 的哨兵。合成一个会让「这条写路径该不该回传文案」多出一个参数，而那个
// 参数填错了不会有任何症状。
func isDuplicateKey(err error) bool {
	var me *mysqldriver.MySQLError
	return errors.As(err, &me) && me.Number == errDuplicateEntry
}
