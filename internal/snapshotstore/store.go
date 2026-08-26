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

// ErrObservationExists 表示这个集群在这一刻已经有一份观测了。
//
// observed_* 系列是时序表，主键为 (cluster_id, name, observed_at)，不含
// run_id：同一时刻同一个对象只能有一份观测。两次**不同**的运行撞上同一个
// observed_at，说明这个集群同时跑着两个采集器。
//
// 与 ErrRunExists 分开是必需的，不是细分：那一条说的是「这一次运行已经交付
// 过了」，处置是什么都不用做；这一条说的是「另一次运行占了这个时刻」，处置
// 是去查为什么有两个采集器在跑。塌成一个通用失败，调用方只能回一句「服务
// 内部错误」，而操作者会去查平台 —— 平台什么问题都没有。
var ErrObservationExists = errors.New("snapshotstore: another collection already covers this instant")

// ErrIngestRunExists 表示这个 (cluster_id, run_id) 的流量摄入已经落过库。
//
// 与 ErrRunExists 同一条理由、不同的对象：摄入也走 CronJob，也会重试。
// 分成两个哨兵而不是共用一个：调用方要能在日志里说清"重复的是哪一件事"，
// 而共用一个会让一次资产重推与一次流量重推在排查时长得一样。
var ErrIngestRunExists = errors.New("snapshotstore: this flow ingest is already stored")

// ErrTooManyConnections 表示一次摄入带的连接数超过了 maxIngestConnections。
//
// 单独一个哨兵，是为了让边界层分辨得出它与一次数据库故障：前者是调用方
// 一次要得太多，处置是缩短窗口；后者是平台坏了。塌成一个通用失败，agent
// 会拿到 500 然后原样重试，而每一次都会得到同样的结果。
//
// **它是一次拒绝，不是一次截断。** 截断会让一部分连接凭空消失，而那批连接
// 的缺席看起来与"这段时间它们没发生"一模一样 —— 一个写入上限伪装成关于
// 集群的观测结论。
var ErrTooManyConnections = errors.New("snapshotstore: this flow ingest carries too many connections")

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
		// 观测撞主键 = 另一次运行已经占了这个时刻。翻译成一个说得出成因的
		// 哨兵，而不是让驱动的错误文本一路走到 API 边界。
		if isDuplicateKey(err) {
			return fmt.Errorf("%w: cluster %s at %s",
				ErrObservationExists, run.Observation.ClusterID,
				run.Observation.ObservedAt.Format(time.RFC3339Nano))
		}
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
		   (cluster_id, run_id, observed_at, started_at, finished_at, status, error_reason,
		    foreign_scopes_complete)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		obs.ClusterID, obs.RunID, obs.ObservedAt, run.StartedAt, run.FinishedAt,
		string(run.Status), string(run.ErrorReason), obs.ForeignScopesComplete)
	if err != nil {
		// 唯一键冲突就是「这一次已经落过了」。用数据库的约束判重，而不是
		// 先 SELECT 再 INSERT：后者在两次并发重试之间有窗口，而 CronJob
		// 的重试恰恰可能同时到达。
		if isDuplicateKey(err) {
			return fmt.Errorf("%w: cluster %s run %s", ErrRunExists, obs.ClusterID, obs.RunID)
		}
		return fmt.Errorf("snapshotstore: insert run: %w", err)
	}

	// 第二平面的覆盖范围与运行元数据同一个事务：完整度标志写在 run 上、
	// 范围写在这里，两者分开落会出现"读到了范围、没读到完整度"的窗口，
	// 而那时最自然的写法（当作完整）恰好是危险的那一个。
	for i, sc := range obs.ForeignScopes {
		labels, err := json.Marshal(sc.MatchLabels)
		if err != nil {
			return fmt.Errorf("snapshotstore: encode foreign scope labels: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO collection_foreign_scope
			   (cluster_id, run_id, seq, namespace, match_labels)
			 VALUES (?, ?, ?, ?, ?)`,
			obs.ClusterID, obs.RunID, i, sc.Namespace, string(labels),
		); err != nil {
			return fmt.Errorf("snapshotstore: insert foreign scope: %w", err)
		}
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
	if err := insertAdminPolicies(ctx, tx, obs); err != nil {
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
		    ip_scope, ip_scope_reason, extra_ips, labels, scrape_annotations,
		    host_network, node_name, service_account,
		    owner_kind, owner_name, workload_kind, workload_name,
		    in_mesh, mesh_source, mesh_detail)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("snapshotstore: prepare pod: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, p := range obs.Pods {
		labels, err := jsonObject(p.Labels)
		if err != nil {
			return fmt.Errorf("snapshotstore: marshal pod labels %s/%s: %w", p.Namespace, p.Name, err)
		}
		// 抓取声明与 labels 走同一个序列化：白名单已经在采集侧执行过
		// （collect.ScrapeAnnotationKeys），这里不再过滤一遍 —— 两处各过滤
		// 一次意味着两份白名单，而漏改的那一份不会报错。
		scrape, err := jsonObject(p.ScrapeAnnotations)
		if err != nil {
			return fmt.Errorf("snapshotstore: marshal pod scrape annotations %s/%s: %w",
				p.Namespace, p.Name, err)
		}
		// 额外地址恒写成数组，空时是 []：一个 NULL 与一个空数组在读回时
		// 分不出"这行是加这一列之前写的"与"这个 Pod 只有一个地址"，
		// 而前者其实也只有一个地址 —— 但那是靠推断，不是靠事实。
		extra, err := json.Marshal(extraAddressRows(p.ExtraIPs))
		if err != nil {
			return fmt.Errorf("snapshotstore: marshal pod extra ips %s/%s: %w",
				p.Namespace, p.Name, err)
		}
		if _, err := stmt.ExecContext(ctx,
			obs.ClusterID, p.Namespace, p.Name, obs.ObservedAt, obs.RunID,
			p.UID, p.Phase, p.IP, string(p.IPScope), string(p.IPScopeReason),
			string(extra), labels, scrape, p.HostNetwork, p.NodeName, p.ServiceAccount,
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

// insertAdminPolicies 落 ANP 与 BANP 的原文。
func insertAdminPolicies(ctx context.Context, tx *sql.Tx, obs snapshot.Observation) error {
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO observed_admin_policy
		   (cluster_id, policy_kind, name, observed_at, run_id, uid, priority, priority_known, manifest)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("snapshotstore: prepare admin policy: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, p := range obs.AdminPolicies {
		if _, err := stmt.ExecContext(ctx,
			obs.ClusterID, string(p.Kind), p.Name, obs.ObservedAt, obs.RunID,
			p.UID, p.Priority, p.PriorityKnown, p.Manifest); err != nil {
			return fmt.Errorf("snapshotstore: insert admin policy %s %s: %w", p.Kind, p.Name, err)
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

// ForeignScopesAt 读出某一次采集观测到的第二平面覆盖范围。
//
// 按**观测时刻**取，与其余按锚点读的资产同一条路：判定要用的是"那一刻
// 集群是什么样"，而 CNP 的覆盖范围会变（CLAUDE.md §4）。
//
// 第二个返回值是那一次采集算出来的范围完不完整。**为 false 时调用方必须
// 整片降级**，不得只降返回的那些：范围不完整意味着有主体被覆盖而我们不知道
// 是哪些，漏掉一个就是把一条真的被管着的连接判成可信。
//
// 那一刻没有任何采集时返回 (nil, false, nil) —— 空范围配上"不完整"，
// 于是调用方走整片降级。**这是刻意的**：读不到就是不知道，而不知道时
// 唯一安全的说法是"整片都不可信"。
func (s *Store) ForeignScopesAt(
	ctx context.Context, clusterID string, at time.Time,
) ([]snapshot.ForeignScope, bool, error) {
	var complete sql.NullBool
	err := s.db.QueryRowContext(ctx,
		`SELECT foreign_scopes_complete FROM collection_run
		  WHERE cluster_id = ? AND observed_at = ? LIMIT 1`, clusterID, at.UTC()).Scan(&complete)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("snapshotstore: read foreign scope completeness: %w", err)
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT sc.namespace, sc.match_labels
		   FROM collection_foreign_scope AS sc
		   JOIN collection_run AS run
		     ON run.cluster_id = sc.cluster_id AND run.run_id = sc.run_id
		  WHERE sc.cluster_id = ? AND run.observed_at = ?
		  ORDER BY sc.seq`, clusterID, at.UTC())
	if err != nil {
		return nil, false, fmt.Errorf("snapshotstore: read foreign scopes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []snapshot.ForeignScope
	for rows.Next() {
		var (
			sc  snapshot.ForeignScope
			raw []byte
		)
		if err := rows.Scan(&sc.Namespace, &raw); err != nil {
			return nil, false, fmt.Errorf("snapshotstore: scan foreign scope: %w", err)
		}
		if err := json.Unmarshal(raw, &sc.MatchLabels); err != nil {
			// 存进去的是我们自己编码的 JSON，解不开说明那一行坏了。
			// **整份作废**，不是跳过这一条：少一条范围就是漏掉一批被覆盖的
			// 主体，而漏掉的那些会被判成可信。
			return nil, false, nil //nolint:nilerr // 见上：坏行即"范围不完整"，走整片降级。
		}
		out = append(out, sc)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("snapshotstore: iterate foreign scopes: %w", err)
	}
	return out, complete.Valid && complete.Bool, nil
}

// podAddressRow 是额外地址在 JSON 里的形状。
//
// 单独一个类型而不是直接序列化 snapshot.PodAddress：那个结构体的字段名一改，
// 落库格式就跟着变，而库里已经写下的行不会跟着改 —— 一次重命名会让旧行读不
// 回来，且只在读到旧数据时才表现出来。
type podAddressRow struct {
	IP     string `json:"ip"`
	Scope  string `json:"scope"`
	Reason string `json:"reason,omitempty"`
}

// extraAddressRows 把额外地址翻成落库形状；**空时返回空切片，不是 nil**。
//
// json.Marshal(nil 切片) 给出 "null"，而这一列声明为 NOT NULL JSON。
// 更要紧的是读回时 "null" 与 "[]" 分不出"没有额外地址"与"这一列坏了"。
func extraAddressRows(in []snapshot.PodAddress) []podAddressRow {
	out := make([]podAddressRow, 0, len(in))
	for _, a := range in {
		out = append(out, podAddressRow{
			IP: a.IP, Scope: string(a.Scope), Reason: string(a.Reason),
		})
	}
	return out
}
