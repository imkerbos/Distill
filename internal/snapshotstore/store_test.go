package snapshotstore_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/cluster"
	"github.com/imkerbos/Distill/internal/config"
	"github.com/imkerbos/Distill/internal/mysqlregistry"
	"github.com/imkerbos/Distill/internal/snapshot"
	"github.com/imkerbos/Distill/internal/snapshotstore"
)

// 两个固定的测试集群。两个而非一个：CLAUDE.md §4 要求身份类表的主键含
// cluster_id，而这条要求只有在两个集群带着同名对象同时在库里时才受力。
const (
	clusterA = "col-test-a"
	clusterB = "col-test-b"
)

// 时刻取到微秒：列是 DATETIME(6)，秒级的时刻会让"两次运行的 observed_at
// 是否真的不同"这件事被列精度而不是被代码决定。
var (
	runOneAt = time.Date(2026, 8, 16, 9, 0, 0, 123456000, time.UTC)
	runTwoAt = time.Date(2026, 8, 16, 9, 10, 0, 654321000, time.UTC)
)

// testDSN 返回集成测试用的 DSN；未设置时跳过。
//
// 用真实 MySQL 而非 mock：本包要验证的是事务回滚、主键约束与 JSON 列的
// 真实行为，这三样恰恰是 mock 唯一伪造不了的部分。
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("DISTILL_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("DISTILL_TEST_MYSQL_DSN not set; skipping MySQL integration test")
	}
	return dsn
}

// cleanupStatements 清空本包写入的全部表，顺序按外键反向。
//
// 写成完整语句而非表名再拼接：拼接 SQL 需要一条 //nolint 才能过 gosec，
// 而那条注释在后来的人眼里与"这里的拼接是安全的"无法区分。
var cleanupStatements = []string{
	"DELETE FROM observed_connection",
	"DELETE FROM flow_ingest_run",
	"DELETE FROM pod_identity_interval",
	"DELETE FROM observed_gateway",
	"DELETE FROM observed_network_policy",
	"DELETE FROM observed_endpoints",
	"DELETE FROM observed_service",
	"DELETE FROM observed_node",
	"DELETE FROM observed_pod",
	"DELETE FROM observed_namespace",
	"DELETE FROM collection_warning",
	"DELETE FROM collection_run_failure",
	"DELETE FROM collection_run_resource",
	"DELETE FROM collection_run",
}

// newTestStore 迁移到最新、清空采集相关表、登记两个测试集群。
//
// 测试结束时把这些行删干净：collection_run 有指向 cluster 的外键，
// 留下的行会让 mysqlregistry 的测试在 DELETE FROM cluster 时撞上外键，
// 表现为另一个包的测试无故失败。
func newTestStore(t *testing.T) (*snapshotstore.Store, *sql.DB) {
	t.Helper()
	cfg := config.DatabaseConfig{DSN: testDSN(t), MaxOpenConns: 5, MaxIdleConns: 2}
	db, err := mysqlregistry.Open(cfg)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := mysqlregistry.Migrate(cfg, "../../migrations"); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}

	clean := func() {
		for _, stmt := range cleanupStatements {
			if _, err := db.Exec(stmt); err != nil {
				t.Fatalf("clean: %q: %v", stmt, err)
			}
		}
		for _, id := range []string{clusterA, clusterB} {
			if _, err := db.Exec(`DELETE FROM cluster WHERE cluster_id = ?`, id); err != nil {
				t.Fatalf("clean cluster %s: %v", id, err)
			}
		}
	}
	clean()

	for _, id := range []string{clusterA, clusterB} {
		if _, err := db.Exec(
			`INSERT INTO cluster
			   (cluster_id, display_name, pod_cidr, node_cidr, ccnp_present,
			    onboard_state, created_at, updated_at)
			 VALUES (?, ?, '10.4.0.0/14', '10.128.0.0/20', 0, 'REGISTERED', NOW(6), NOW(6))`,
			id, id); err != nil {
			t.Fatalf("seed cluster %s: %v", id, err)
		}
	}

	t.Cleanup(func() {
		clean()
		_ = db.Close()
	})
	return snapshotstore.New(db), db
}

// sampleRun 是一次全部资源都采到的运行，每类资源各一条记录。
func sampleRun(clusterID, runID string, observedAt time.Time) snapshot.Run {
	return snapshot.Run{
		Status:     snapshot.RunOK,
		StartedAt:  observedAt.Add(-30 * time.Second),
		FinishedAt: observedAt.Add(5 * time.Second),
		Observation: snapshot.Observation{
			ClusterID:  clusterID,
			RunID:      runID,
			ObservedAt: observedAt,
			Namespaces: []snapshot.Namespace{{
				ClusterID: clusterID, Name: "shop",
				Labels:     map[string]string{"team": "checkout"},
				InMesh:     true,
				MeshSource: cluster.MeshSourceNamespaceInjection,
				MeshDetail: "istio-injection=enabled",
			}},
			Pods: []snapshot.Pod{samplePod(clusterID, "web-1", "10.4.0.9")},
			Nodes: []snapshot.Node{{
				ClusterID: clusterID, Name: "node-a",
				PodCIDRs: []string{"10.4.1.0/24"}, InternalIPs: []string{"10.128.0.5"},
			}},
			Services: []snapshot.Service{{
				ClusterID: clusterID, Namespace: "shop", Name: "web",
				Type:      "ClusterIP",
				Selector:  map[string]string{"app": "web"},
				ClusterIP: "10.8.0.7",
				Ports: []snapshot.ServicePort{
					{Name: "http", Port: 80, TargetPort: 8080, Protocol: "TCP"},
				},
			}},
			Endpoints: []snapshot.Endpoints{{
				ClusterID: clusterID, Namespace: "shop", Name: "web",
				Addresses: []string{"10.4.0.9"}, Ports: []int32{8080},
			}},
			Policies: []snapshot.NetworkPolicy{{
				ClusterID: clusterID, Namespace: "shop", Name: "deny-all",
				UID:      "8f14e45f-ceea-467a-9ba5-7b5f0f1f0f01",
				Manifest: "apiVersion: networking.k8s.io/v1\nkind: NetworkPolicy\n",
			}},
			Gateways: []snapshot.Gateway{{
				ClusterID: clusterID, Namespace: "shop", Name: "public",
				Kind: "Ingress", BackendService: "web",
			}},
		},
	}
}

func samplePod(clusterID, name, ip string) snapshot.Pod {
	return snapshot.Pod{
		ClusterID: clusterID, Namespace: "shop", Name: name,
		UID:           "3c9d2b1a-0000-4000-8000-00000000000a",
		Phase:         "Running",
		IP:            ip,
		IPScope:       cluster.ScopePod,
		IPScopeReason: "",
		Labels:        map[string]string{"app": "web"},
		HostNetwork:   false,
		NodeName:      "node-a", ServiceAccount: "web",
		OwnerKind: "ReplicaSet", OwnerName: "web-6d4f",
		WorkloadKind: "Deployment", WorkloadName: "web",
		InMesh: false, MeshSource: "", MeshDetail: "",
	}
}

func mustSave(t *testing.T, s *snapshotstore.Store, run snapshot.Run) {
	t.Helper()
	if err := s.Save(context.Background(), run); err != nil {
		t.Fatalf("Save(%s) error = %v", run.Observation.RunID, err)
	}
}

func scanInt(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return n
}

func scanString(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	var s string
	if err := db.QueryRow(query, args...).Scan(&s); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return s
}

// 落库是 append-only（design doc 2026-08-16 §4，决定 3）。第二次采集必须
// 是新增一批带新 observed_at 的行，而不是覆盖上一批。
//
// 覆盖会让"这条三周前的流量当时的 Pod 标签是什么"用今天的标签回答，
// 且答得出、不报错 —— 这正是 CLAUDE.md §4 禁止的那种失败。
func TestSecondRunAppendsRatherThanReplaces(t *testing.T) {
	s, db := newTestStore(t)

	first := sampleRun(clusterA, "run-1", runOneAt)
	mustSave(t, s, first)

	// 第二次运行看到同一个 Pod，但标签变了、IP 换了。
	second := sampleRun(clusterA, "run-2", runTwoAt)
	second.Observation.Pods[0].IP = "10.4.0.21"
	second.Observation.Pods[0].Labels = map[string]string{"app": "web", "version": "v2"}
	mustSave(t, s, second)

	if n := scanInt(t, db,
		`SELECT COUNT(*) FROM observed_pod
		  WHERE cluster_id = ? AND namespace = 'shop' AND name = 'web-1'`,
		clusterA); n != 2 {
		t.Errorf("observed_pod rows for web-1 = %d, want 2 (one per run)", n)
	}

	// 第一次那一行必须原样还在：IP 与标签都还是当时的值。
	if ip := scanString(t, db,
		`SELECT ip FROM observed_pod
		  WHERE cluster_id = ? AND namespace = 'shop' AND name = 'web-1' AND observed_at = ?`,
		clusterA, runOneAt); ip != "10.4.0.9" {
		t.Errorf("first run pod ip = %q, want 10.4.0.9 (the value observed at that time)", ip)
	}
	if has := scanInt(t, db,
		`SELECT JSON_CONTAINS_PATH(labels, 'one', '$.version') FROM observed_pod
		  WHERE cluster_id = ? AND namespace = 'shop' AND name = 'web-1' AND observed_at = ?`,
		clusterA, runOneAt); has != 0 {
		t.Error("first run pod labels carry version; that label belongs to the second run")
	}

	// 其余各表同样是两行而非一行。
	for _, tc := range []struct {
		table string
		query string
	}{
		{"observed_namespace", `SELECT COUNT(*) FROM observed_namespace WHERE cluster_id = ?`},
		{"observed_node", `SELECT COUNT(*) FROM observed_node WHERE cluster_id = ?`},
		{"observed_service", `SELECT COUNT(*) FROM observed_service WHERE cluster_id = ?`},
		{"observed_endpoints", `SELECT COUNT(*) FROM observed_endpoints WHERE cluster_id = ?`},
		{"observed_network_policy", `SELECT COUNT(*) FROM observed_network_policy WHERE cluster_id = ?`},
		{"observed_gateway", `SELECT COUNT(*) FROM observed_gateway WHERE cluster_id = ?`},
	} {
		if n := scanInt(t, db, tc.query, clusterA); n != 2 {
			t.Errorf("%s rows = %d, want 2 (one per run)", tc.table, n)
		}
	}
}

// Latest 取最近一次运行，判据是 observed_at 而非写入顺序或 run_id。
//
// 按写入顺序取会在补采一份历史快照之后指向那份历史；run_id 是随机串，
// 根本没有顺序。这里刻意先写晚的那次、后写早的那次。
func TestLatestReturnsTheMostRecentRunNotTheLastWritten(t *testing.T) {
	s, _ := newTestStore(t)

	mustSave(t, s, sampleRun(clusterA, "run-late", runTwoAt))
	mustSave(t, s, sampleRun(clusterA, "run-early", runOneAt))

	got, err := s.Latest(context.Background(), clusterA)
	if err != nil {
		t.Fatalf("Latest() error = %v", err)
	}
	if got.RunID != "run-late" {
		t.Errorf("Latest().RunID = %q, want run-late", got.RunID)
	}
	if !got.ObservedAt.Equal(runTwoAt) {
		t.Errorf("Latest().ObservedAt = %v, want %v", got.ObservedAt, runTwoAt)
	}
}

// 摘要里的计数与告警必须只属于它自己那次运行。
//
// 跨运行汇总会让一次干净的采集继承上一次的告警，于是"这次采集有没有
// 发现注册表填错"这个问题永远答"有"。
func TestLatestScopesCountsAndWarningsToItsOwnRun(t *testing.T) {
	s, _ := newTestStore(t)

	// 早的那次：两个 Pod、一条告警。
	early := sampleRun(clusterA, "run-early", runOneAt)
	early.Observation.Pods = append(early.Observation.Pods, samplePod(clusterA, "web-2", "10.99.0.4"))
	early.Observation.Warnings = []snapshot.Warning{{
		Kind:    snapshot.WarningPodIPOutsideCluster,
		Subject: "shop/web-2",
		Detail:  "10.99.0.4 is outside the registered pod CIDR",
	}}
	mustSave(t, s, early)

	// 晚的那次：一个 Pod、零告警。
	mustSave(t, s, sampleRun(clusterA, "run-late", runTwoAt))

	got, err := s.Latest(context.Background(), clusterA)
	if err != nil {
		t.Fatalf("Latest() error = %v", err)
	}
	if got.RunID != "run-late" {
		t.Fatalf("Latest().RunID = %q, want run-late", got.RunID)
	}

	// 每类资源恰好一条 —— 混进上一次的结果会让 POD 出现两次。
	seen := map[string]int{}
	for _, o := range got.Resources {
		seen[o.Resource]++
	}
	for resource, n := range seen {
		if n != 1 {
			t.Errorf("Resources contains %s %d times, want 1 (results from other runs leaked in)", resource, n)
		}
	}
	n, observed := observedCount(got.Resources, "POD")
	if !observed || n != 1 {
		t.Errorf("POD count = %d (observed=%v), want 1 (the late run saw one pod; the early run saw two)",
			n, observed)
	}

	if got.WarningTotal != 0 {
		t.Errorf("WarningTotal = %d, want 0 (the only warning belongs to run-early)", got.WarningTotal)
	}
	if len(got.Warnings) != 0 {
		t.Errorf("Warnings = %+v, want empty (they belong to run-early)", got.Warnings)
	}
}

// observedCount 返回摘要里某类资源观测到的条数。
//
// observed 为 false 有两种情形：这一类根本没出现在摘要里，或者出现了
// 但带着一条失败记录。两者都不该被当成一个数字读，所以合成同一个 false。
func observedCount(outcomes []snapshotstore.ResourceOutcome, resource string) (count int, observed bool) {
	for _, o := range outcomes {
		if o.Resource == resource {
			return o.Count()
		}
	}
	return 0, false
}

// outcomeOf 返回摘要里某类资源的结果，找不到时 t.Fatal。
func outcomeOf(t *testing.T, outcomes []snapshotstore.ResourceOutcome, resource string) snapshotstore.ResourceOutcome {
	t.Helper()
	for _, o := range outcomes {
		if o.Resource == resource {
			return o
		}
	}
	t.Fatalf("Resources has no entry for %s; got %+v", resource, outcomes)
	return snapshotstore.ResourceOutcome{}
}

// 一次中途失败的运行不得留下任何行。
//
// 留下 collection_run 而没留下明细，会在可见面上报出一个"采到了 N 个
// Pod、一个也查不到"的状态，而这个状态与采集器挂掉无法区分。
// 留下明细而没留下 run，则这批行永远连不回任何一次运行。
func TestFailedRunLeavesNoRowsAtAll(t *testing.T) {
	s, db := newTestStore(t)

	// 同一次观测里出现两个同名 Pod：命中 observed_pod 主键冲突，
	// 而 collection_run 与 observed_namespace 此时已经插进事务了。
	broken := sampleRun(clusterA, "run-broken", runOneAt)
	broken.Observation.Pods = append(broken.Observation.Pods, samplePod(clusterA, "web-1", "10.4.0.9"))

	if err := s.Save(context.Background(), broken); err == nil {
		t.Fatal("Save() with a duplicate pod succeeded, want a primary key violation")
	}

	for _, tc := range []struct {
		table string
		query string
	}{
		{"collection_run", `SELECT COUNT(*) FROM collection_run WHERE cluster_id = ?`},
		{"collection_run_resource", `SELECT COUNT(*) FROM collection_run_resource WHERE cluster_id = ?`},
		{"observed_namespace", `SELECT COUNT(*) FROM observed_namespace WHERE cluster_id = ?`},
		{"observed_pod", `SELECT COUNT(*) FROM observed_pod WHERE cluster_id = ?`},
	} {
		if n := scanInt(t, db, tc.query, clusterA); n != 0 {
			t.Errorf("%s rows after a failed run = %d, want 0", tc.table, n)
		}
	}

	// 可见面必须说"这个集群从未采集过"，而不是拿半份数据当一次运行。
	// 断言具体的 ErrNoRun 而非任意错误：一个连接错误也能让 err != nil，
	// 而那种"通过"什么都没证明。
	if _, err := s.Latest(context.Background(), clusterA); !errors.Is(err, snapshotstore.ErrNoRun) {
		t.Errorf("Latest() after a failed run error = %v, want ErrNoRun", err)
	}
}

// PARTIAL 必须活着走完落库与读回，且带着失败的是哪一类、因为什么。
//
// 报成 OK 会让下游把一份缺了 NetworkPolicy 的快照当作完整事实使用，
// 于是"这个集群没有任何策略"与"我们没被授权看策略"变得无法区分 ——
// 而前者会让平台推荐一份 default-deny。
func TestPartialRunIsDistinguishableFromACompleteOne(t *testing.T) {
	s, _ := newTestStore(t)

	partial := sampleRun(clusterA, "run-partial", runOneAt)
	partial.Status = snapshot.RunPartial
	partial.Observation.Policies = nil // 没被授权看，所以一条也没采到
	partial.Failures = []snapshot.Failure{{
		Resource: snapshot.ResourceNetworkPolicy,
		Reason:   snapshot.FailureForbidden,
		Detail:   `networkpolicies.networking.k8s.io is forbidden`,
	}}
	mustSave(t, s, partial)

	got, err := s.Latest(context.Background(), clusterA)
	if err != nil {
		t.Fatalf("Latest() error = %v", err)
	}
	if got.Status != string(snapshot.RunPartial) {
		t.Errorf("Status = %q, want PARTIAL", got.Status)
	}

	policies := outcomeOf(t, got.Resources, string(snapshot.ResourceNetworkPolicy))
	record, failed := policies.Failure()
	if !failed {
		t.Fatalf("NETWORKPOLICY outcome = %+v, want a failure record", policies)
	}
	if record.Reason != string(snapshot.FailureForbidden) {
		t.Errorf("NETWORKPOLICY failure reason = %q, want FORBIDDEN", record.Reason)
	}

	// 采到的那部分照常报出来：PARTIAL 不是"整次运行作废"。
	if n, observed := observedCount(got.Resources, "POD"); !observed || n != 1 {
		t.Errorf("POD count = %d (observed=%v), want 1", n, observed)
	}
}

// CLAUDE.md §4：身份类表的主键必须含 cluster_id。
//
// 不同集群的 Pod CIDR 可以重叠。缺了 cluster_id，两个集群里同名同 IP 的
// Pod 会互相覆盖 —— 覆盖是静默的，join 会命中错误集群的那一条且不报错。
func TestIdenticalPodsInTwoClustersDoNotCollide(t *testing.T) {
	s, db := newTestStore(t)

	a := sampleRun(clusterA, "run-a", runOneAt)
	a.Observation.Pods[0].WorkloadName = "web-in-a"
	mustSave(t, s, a)

	// 同一时刻、同一 namespace、同名、同 IP —— 只有 cluster_id 不同。
	b := sampleRun(clusterB, "run-b", runOneAt)
	b.Observation.Pods[0].WorkloadName = "web-in-b"
	mustSave(t, s, b)

	if n := scanInt(t, db,
		`SELECT COUNT(*) FROM observed_pod WHERE namespace = 'shop' AND name = 'web-1' AND ip = '10.4.0.9'`,
	); n != 2 {
		t.Errorf("observed_pod rows for the same pod in two clusters = %d, want 2", n)
	}
	// 按 (cluster_id, ip, observed_at) 查回来的必须是本集群的那条。
	if w := scanString(t, db,
		`SELECT workload_name FROM observed_pod
		  WHERE cluster_id = ? AND ip = '10.4.0.9' AND observed_at = ?`,
		clusterB, runOneAt); w != "web-in-b" {
		t.Errorf("workload_name for %s = %q, want web-in-b", clusterB, w)
	}

	// 摘要同样不得串集群。
	summary, err := s.Latest(context.Background(), clusterA)
	if err != nil {
		t.Fatalf("Latest(%s) error = %v", clusterA, err)
	}
	if summary.RunID != "run-a" {
		t.Errorf("Latest(%s).RunID = %q, want run-a", clusterA, summary.RunID)
	}
}

// 空标签必须落成 JSON 的 {} / []，不是 JSON null。
//
// json.Marshal 对 nil map 产出字面量 null，那是一个合法的 JSON 值，
// 会被原样存进 JSON 列。读回来时"这个 Pod 没有标签"与"标签这一列坏了"
// 就成了同一个 null。这里从 Save 走完整条路径而不是直接调那两个辅助函数：
// 一个只在原地验证过的守卫，证明不了调用方还在调它。
func TestNilLabelsAreStoredAsEmptyJSONNotNull(t *testing.T) {
	s, db := newTestStore(t)

	run := sampleRun(clusterA, "run-empty", runOneAt)
	run.Observation.Namespaces[0].Labels = nil
	run.Observation.Pods[0].Labels = nil
	run.Observation.Services[0].Selector = nil
	run.Observation.Services[0].Ports = nil
	run.Observation.Nodes[0].PodCIDRs = nil
	run.Observation.Nodes[0].InternalIPs = nil
	run.Observation.Endpoints[0].Addresses = nil
	run.Observation.Endpoints[0].Ports = nil
	mustSave(t, s, run)

	for _, tc := range []struct {
		column string
		want   string
		query  string
	}{
		{"observed_namespace.labels", "OBJECT",
			`SELECT JSON_TYPE(labels) FROM observed_namespace WHERE cluster_id = ?`},
		{"observed_pod.labels", "OBJECT",
			`SELECT JSON_TYPE(labels) FROM observed_pod WHERE cluster_id = ?`},
		{"observed_service.selector", "OBJECT",
			`SELECT JSON_TYPE(selector) FROM observed_service WHERE cluster_id = ?`},
		{"observed_service.ports", "ARRAY",
			`SELECT JSON_TYPE(ports) FROM observed_service WHERE cluster_id = ?`},
		{"observed_node.pod_cidrs", "ARRAY",
			`SELECT JSON_TYPE(pod_cidrs) FROM observed_node WHERE cluster_id = ?`},
		{"observed_node.internal_ips", "ARRAY",
			`SELECT JSON_TYPE(internal_ips) FROM observed_node WHERE cluster_id = ?`},
		{"observed_endpoints.addresses", "ARRAY",
			`SELECT JSON_TYPE(addresses) FROM observed_endpoints WHERE cluster_id = ?`},
		{"observed_endpoints.ports", "ARRAY",
			`SELECT JSON_TYPE(ports) FROM observed_endpoints WHERE cluster_id = ?`},
	} {
		if got := scanString(t, db, tc.query, clusterA); got != tc.want {
			t.Errorf("JSON_TYPE(%s) = %q, want %q (nil must not become JSON null)",
				tc.column, got, tc.want)
		}
	}
}
