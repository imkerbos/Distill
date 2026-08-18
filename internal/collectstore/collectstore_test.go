package collectstore_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/cluster"
	"github.com/imkerbos/Distill/internal/collectstore"
	"github.com/imkerbos/Distill/internal/config"
	"github.com/imkerbos/Distill/internal/flow"
	"github.com/imkerbos/Distill/internal/mysqlregistry"
	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/snapshot"
	"github.com/imkerbos/Distill/internal/snapshotstore"
	"github.com/imkerbos/Distill/internal/store"
)

// 两个测试集群：一个有采集，一个一条记录都没有。
//
// 两个而非一个：本包要守的第一条性质就是"没有采集"与"采集过、什么都没有"
// 必须给出不同的答案，而只有一个集群时，一个永远报错的 Reader 也能让
// 断言通过。
const (
	collectedID = "col-read-a"
	silentID    = "col-read-b"
)

// 时刻取到微秒：列是 DATETIME(6)，秒级的时刻会让"两次运行的 observed_at
// 是否真的不同"由列精度而不是由代码决定。
var (
	firstRunAt  = time.Date(2026, 8, 16, 9, 0, 0, 123456000, time.UTC)
	windowStart = firstRunAt.Add(5 * time.Minute)
	windowEnd   = firstRunAt.Add(10 * time.Minute)
	// recycleAt 是 Pod 地址换主体的那一刻，落在被描述的窗口**之后**。
	recycleAt = firstRunAt.Add(time.Hour)
)

// recycledIP 是先后属于两个工作负载的地址，本包全部时序断言的支点。
const recycledIP = "10.4.0.5"

// peerIP 是一个从头到尾没换过主体的地址，用作对照。
const peerIP = "10.4.0.6"

// testDSN 返回集成测试用的 DSN；未设置时跳过。
//
// 用真实 MySQL 而非 mock：本包要验证的正是"按时刻 join 区间表"这件事，
// 而按时刻取哪一行恰恰是 mock 唯一伪造不了的部分。
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("DISTILL_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("DISTILL_TEST_MYSQL_DSN not set; skipping MySQL integration test")
	}
	return dsn
}

// cleanupStatements 清空本包读到的全部表，顺序按外键反向。
//
// 写成完整语句而非表名再拼接：拼接 SQL 需要一条 //nolint 才能过 gosec，
// 而那条注释在后来的人眼里与"这里的拼接是安全的"无法区分。
var cleanupStatements = []string{
	// 本包不写这张表，但它有指向 collection_run 的外键：snapshotstore 的
	// 测试留下的行会让下面那条 DELETE FROM collection_run 撞上外键，
	// 表现为另一个包的测试无故失败。
	"DELETE FROM identity_derive_run",
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

// stubSource 是一份写死的注册表。
//
// 不连库：本包要验证的是"按登记为 COLLECTED 的集群读采集数据"，而集群登记
// 本身从哪张表读出来与此无关，那一半由 internal/mysqlregistry 的集成测试守。
type stubSource struct {
	clusters []registry.Cluster
}

func (s stubSource) Clusters(context.Context) ([]registry.Cluster, error) { return s.clusters, nil }

func (s stubSource) Cluster(_ context.Context, id string) (registry.Cluster, bool, error) {
	for _, c := range s.clusters {
		if c.ID == id {
			return c, true, nil
		}
	}
	return registry.Cluster{}, false, nil
}

func (s stubSource) RuleOverrides(context.Context, string) ([]registry.RuleOverride, error) {
	return nil, nil
}

// testSource 把两个测试集群都登记为 COLLECTED。
//
// 两个都登记，是为了让"没有采集"这个答案不可能来自"这个集群没登记"。
func testSource() stubSource {
	return stubSource{clusters: []registry.Cluster{
		{ID: collectedID, DataSource: registry.DataSourceCollected,
			PodCIDR: "10.4.0.0/14", NodeCIDR: "10.128.0.0/20"},
		{ID: silentID, DataSource: registry.DataSourceCollected,
			PodCIDR: "10.6.0.0/14", NodeCIDR: "10.130.0.0/20"},
	}}
}

// ccnpSource 与 testSource 只差一件事：被采集的那个集群登记了 CCNP。
//
// 用来造出"求值引擎自己把可信度降下来"这个情形。集群装没装 CCNP 是登记出来
// 的（registry.Cluster.CCNPPresent），不由数据推断 —— 与数据来源同一条理由。
func ccnpSource() stubSource {
	src := testSource()
	for i := range src.clusters {
		if src.clusters[i].ID == collectedID {
			src.clusters[i].CCNPPresent = true
		}
	}
	return src
}

// newTestReader 迁移到最新、清空采集相关表、登记两个测试集群。
func newTestReader(t *testing.T) (*collectstore.Reader, *snapshotstore.Store) {
	t.Helper()
	return newTestReaderWithSource(t, testSource())
}

// newTestReaderWithSource 同上，但由调用方决定注册表里登记了什么。
func newTestReaderWithSource(t *testing.T, src stubSource) (*collectstore.Reader, *snapshotstore.Store) {
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
		for _, id := range []string{collectedID, silentID} {
			if _, err := db.Exec(`DELETE FROM cluster WHERE cluster_id = ?`, id); err != nil {
				t.Fatalf("clean cluster %s: %v", id, err)
			}
		}
	}
	clean()
	seedClusters(t, db)

	t.Cleanup(func() {
		clean()
		_ = db.Close()
	})
	return collectstore.New(db, src), snapshotstore.New(db)
}

// seedClusters 写入两行 cluster：collection_run 有指向它的外键。
func seedClusters(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, id := range []string{collectedID, silentID} {
		if _, err := db.Exec(
			`INSERT INTO cluster
			   (cluster_id, display_name, pod_cidr, node_cidr, ccnp_present,
			    onboard_state, created_at, updated_at)
			 VALUES (?, ?, '10.4.0.0/14', '10.128.0.0/20', 0, 'REGISTERED', NOW(6), NOW(6))`,
			id, id); err != nil {
			t.Fatalf("seed cluster %s: %v", id, err)
		}
	}
}

// podAt 构造一条 Pod 观测。
func podAt(namespace, name, uid, ip, workload string) snapshot.Pod {
	return snapshot.Pod{
		ClusterID: collectedID, Namespace: namespace, Name: name,
		UID:      uid,
		Phase:    "Running",
		IP:       ip,
		IPScope:  cluster.ScopePod,
		Labels:   map[string]string{"app": workload},
		NodeName: "node-a", ServiceAccount: workload,
		OwnerKind: "ReplicaSet", OwnerName: workload + "-6d4f",
		WorkloadKind: "Deployment", WorkloadName: workload,
	}
}

// apiPolicy 是一条只选中 payment/api 的策略，用来验证覆盖率算的是标签比对。
const apiPolicy = `apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-api
  namespace: payment
spec:
  podSelector:
    matchLabels:
      app: api
`

// saveRun 落一次采集运行并推导身份区间。
func saveRun(t *testing.T, s *snapshotstore.Store, runID string, at time.Time, pods []snapshot.Pod) {
	t.Helper()
	run := snapshot.Run{
		Status:     snapshot.RunOK,
		StartedAt:  at.Add(-30 * time.Second),
		FinishedAt: at.Add(5 * time.Second),
		Observation: snapshot.Observation{
			ClusterID:  collectedID,
			RunID:      runID,
			ObservedAt: at,
			Namespaces: []snapshot.Namespace{
				{ClusterID: collectedID, Name: "payment"},
				{ClusterID: collectedID, Name: "shop"},
				{ClusterID: collectedID, Name: "batch"},
			},
			Pods: pods,
			Policies: []snapshot.NetworkPolicy{{
				ClusterID: collectedID, Namespace: "payment", Name: "allow-api",
				UID:      "8f14e45f-ceea-467a-9ba5-7b5f0f1f0f01",
				Manifest: apiPolicy,
			}},
		},
	}
	ctx := context.Background()
	if err := s.Save(ctx, run); err != nil {
		t.Fatalf("Save(%s) error = %v", runID, err)
	}
	if err := s.DeriveIdentityIntervals(ctx, collectedID, runID); err != nil {
		t.Fatalf("DeriveIdentityIntervals(%s) error = %v", runID, err)
	}
}

// saveIngest 落一次覆盖 [windowStart, windowEnd) 的流量摄入。
//
// 两端都**不带身份**：来源只报了地址，与 VPC flow logs 一样。于是拓扑上那条
// 边的归属只可能来自区间表 —— 摄入时顺手记下的那份身份在这里不存在，
// 想蒙混过关也没有东西可蒙。
func saveIngest(t *testing.T, s *snapshotstore.Store, conns []flow.Connection) {
	t.Helper()
	w := flow.Window{From: windowStart, To: windowEnd}
	res, err := flow.NewIngestResult(flow.SourceHubble, w, w, conns)
	if err != nil {
		t.Fatalf("NewIngestResult() error = %v", err)
	}
	res = res.WithSampleRate(1).WithDropped(0)
	if err := s.SaveIngest(context.Background(), snapshotstore.IngestRun{
		ClusterID:  collectedID,
		RunID:      "ingest-1",
		StartedAt:  windowEnd,
		FinishedAt: windowEnd.Add(time.Second),
		Status:     snapshotstore.IngestOK,
		Result:     res,
	}); err != nil {
		t.Fatalf("SaveIngest() error = %v", err)
	}
}

// seedRecycledAddress 建出本包的基准场景：
//
//	T0        一次采集，10.4.0.5 属于 payment/api，10.4.0.6 属于 shop/web
//	T0+5m..   一次流量摄入，两者之间有一条连接
//	T0+1h     再一次采集，10.4.0.5 已经换给了 batch/job
//
// 被描述的窗口落在两次采集之间，因此"按时刻取"与"取最新快照"给出的是两个
// 不同的答案 —— 而后者会把一条 payment 的连接算到 batch 头上，且不报错。
func seedRecycledAddress(t *testing.T, s *snapshotstore.Store) {
	t.Helper()
	saveRun(t, s, "run-1", firstRunAt, []snapshot.Pod{
		podAt("payment", "api-1", "3c9d2b1a-0000-4000-8000-00000000000a", recycledIP, "api"),
		podAt("shop", "web-1", "3c9d2b1a-0000-4000-8000-00000000000b", peerIP, "web"),
	})
	saveIngest(t, s, []flow.Connection{{
		Source:        flow.Endpoint{IP: recycledIP},
		Dest:          flow.Endpoint{IP: peerIP},
		Protocol:      flow.ProtocolTCP,
		Port:          8080,
		ObservedCount: 12,
	}})
	saveRun(t, s, "run-2", recycleAt, []snapshot.Pod{
		podAt("batch", "job-1", "3c9d2b1a-0000-4000-8000-00000000000c", recycledIP, "job"),
		podAt("shop", "web-1", "3c9d2b1a-0000-4000-8000-00000000000b", peerIP, "web"),
	})
}

// 一无所知时必须给出明确的「没有数据」，而不是一份空结果。
//
// 空拓扑读起来是"这个集群里什么都没有"，那是一句没有人确认过的话，而且它是
// 让人放心的那个方向（design doc 2026-08-17 §2）。
//
// **2026-08-18 收窄**：原先「只采了资产、没摄入流量」也落在这里，因为拓扑与
// 流量被绑在一起。那条挡住的东西比它该挡的多 —— 「一个 Pod 都没有」与「有
// 652 个 Pod、一条流量都没观测过」是两件完全不同的事，而当时它们给出同一个
// 答案。现在前者仍然整份拒绝（本用例），后者按资产作答并把「没有观测」写进
// 结构里（assets_only_test.go）。**Quality 三种情形都仍然拒绝**：它数的是每条
// 连接的判定构成，没有连接就没有分母。
//
// 对照组是同一个 Reader 对一个真有采集的集群照常供数 —— 少了它，一个
// 什么都答不出来的 Reader 也能让上面全部通过。
func TestNoCollectionIsNotAnEmptyCluster(t *testing.T) {
	r, s := newTestReader(t)
	ctx := context.Background()

	// topologyRefused 只在「这个集群一无所知」那一档成立。
	topologyRefused := func(t *testing.T, stage string) {
		t.Helper()
		topo, err := r.Topology(ctx, silentID, store.LevelNamespace)
		if !errors.Is(err, collectstore.ErrNoCollection) {
			t.Errorf("%s: Topology() error = %v, want ErrNoCollection", stage, err)
		}
		if len(topo.Nodes) != 0 || len(topo.Edges) != 0 {
			t.Errorf("%s: Topology() returned %d nodes / %d edges alongside the refusal",
				stage, len(topo.Nodes), len(topo.Edges))
		}
	}

	// qualityRefused 在三种情形下都成立：这一屏问的就是流量。
	qualityRefused := func(t *testing.T, stage string) {
		t.Helper()
		q, err := r.Quality(ctx, silentID)
		if !errors.Is(err, collectstore.ErrNoCollection) {
			t.Errorf("%s: Quality() error = %v, want ErrNoCollection", stage, err)
		}
		if q.Cluster != "" || q.TotalFlows != 0 {
			t.Errorf("%s: Quality() returned %+v alongside the refusal", stage, q)
		}
	}

	topologyRefused(t, "从未采集过")
	qualityRefused(t, "从未采集过")

	// 采了资产、没摄入过流量：仍然不是一个安静的集群，是一个我们没看过
	// 流量的集群。
	if err := s.Save(ctx, snapshot.Run{
		Status:     snapshot.RunOK,
		StartedAt:  firstRunAt,
		FinishedAt: firstRunAt,
		Observation: snapshot.Observation{
			ClusterID: silentID, RunID: "silent-run-1", ObservedAt: firstRunAt,
			Namespaces: []snapshot.Namespace{{ClusterID: silentID, Name: "payment"}},
		},
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	// **这一档变了**：资产在库里，拓扑的节点就算得出来。仍然要拒绝的是
	// Quality —— 它数的是判定构成，没有连接就没有分母。
	qualityRefused(t, "采了资产、没有流量")

	// 唯一一次摄入是失败的：它带着一个窗口，但一条连接都没拿到。就着它的
	// 窗口去查会得到零条连接，而零条会被读成"这段时间没有流量"。
	w := flow.Window{From: windowStart, To: windowEnd}
	failed, err := flow.NewIngestResult(flow.SourceHubble, w, flow.Window{}, nil)
	if err != nil {
		t.Fatalf("NewIngestResult() error = %v", err)
	}
	if err := s.SaveIngest(ctx, snapshotstore.IngestRun{
		ClusterID: silentID, RunID: "silent-ingest-1",
		StartedAt: windowEnd, FinishedAt: windowEnd,
		Status:      snapshotstore.IngestFailed,
		ErrorReason: snapshotstore.IngestErrorUnreachable,
		Result:      failed,
	}); err != nil {
		t.Fatalf("SaveIngest() error = %v", err)
	}
	qualityRefused(t, "唯一一次摄入失败了")

	// 对照组：一个真有采集的集群必须照常被描述出来。
	seedRecycledAddress(t, s)
	topo, err := r.Topology(ctx, collectedID, store.LevelNamespace)
	if err != nil {
		t.Fatalf("Topology(%s) error = %v, want a described cluster", collectedID, err)
	}
	if len(topo.Nodes) == 0 || len(topo.Edges) == 0 {
		t.Fatalf("Topology(%s) = %d nodes / %d edges; a collected cluster must still be described",
			collectedID, len(topo.Nodes), len(topo.Edges))
	}
}

// 资产按被问到的那个时刻从区间表取，不取最新快照（design doc §5）。
//
// 场景里 10.4.0.5 先属于 payment/api，一小时后被 batch/job 接手，而被描述的
// 流量窗口落在两者之间。取最新快照会把那条连接算到 batch 头上 —— 答得出、
// 不报错，只是错了。
//
// 三处一起断言，因为它们各自都能被单独绕过：边的源节点、节点清单、以及
// 那一刻活着的 Pod 计数。
func TestAssetsComeFromTheIntervalAtThatMoment(t *testing.T) {
	r, s := newTestReader(t)
	ctx := context.Background()
	seedRecycledAddress(t, s)

	topo, err := r.Topology(ctx, collectedID, store.LevelNamespace)
	if err != nil {
		t.Fatalf("Topology() error = %v", err)
	}

	const (
		wantSource = collectedID + "/payment"
		wantTarget = collectedID + "/shop"
		staleNode  = collectedID + "/batch"
	)

	if len(topo.Edges) != 1 {
		t.Fatalf("Topology() edges = %+v, want exactly the one observed connection", topo.Edges)
	}
	edge := topo.Edges[0]
	if edge.Source != wantSource {
		t.Errorf("edge source = %s, want %s: the address belonged to payment during the described "+
			"window and only moved to batch an hour later", edge.Source, wantSource)
	}
	if edge.Target != wantTarget {
		t.Errorf("edge target = %s, want %s", edge.Target, wantTarget)
	}
	if topo.UnplaceableFlowCount != 0 {
		t.Errorf("UnplaceableFlowCount = %d, want 0: both ends resolve from the intervals",
			topo.UnplaceableFlowCount)
	}

	nodes := map[string]store.TopologyNode{}
	for _, n := range topo.Nodes {
		nodes[n.ID] = n
	}
	if _, stale := nodes[staleNode]; stale {
		t.Errorf("Topology() carries node %s; that workload only took the address over at %s, "+
			"after the window being described", staleNode, recycleAt)
	}
	payment, ok := nodes[wantSource]
	if !ok {
		t.Fatalf("Topology() nodes = %v, want one for %s", topo.Nodes, wantSource)
	}
	if payment.PodCount != 1 {
		t.Errorf("%s PodCount = %d, want 1", wantSource, payment.PodCount)
	}
	if !payment.HasPolicy {
		t.Errorf("%s HasPolicy = false; the snapshot covering that moment holds one policy", wantSource)
	}

	// Quality 描述同一个窗口，因此它也必须解释同一批主体：两端都解得出来，
	// 于是没有一条连接落进归属类的 UNKNOWN 原因。
	q, err := r.Quality(ctx, collectedID)
	if err != nil {
		t.Fatalf("Quality() error = %v", err)
	}
	if q.TotalFlows != 1 {
		t.Fatalf("Quality().TotalFlows = %d, want 1", q.TotalFlows)
	}
	for _, unattributable := range []string{"IP_AMBIGUOUS", "EXTERNAL_NO_IDENTITY", "SNAPSHOT_MISSING"} {
		if n := q.UnknownComposition[unattributable]; n != 0 {
			t.Errorf("UnknownComposition[%s] = %d, want 0: the address resolves from the interval "+
				"covering that moment", unattributable, n)
		}
	}
}
