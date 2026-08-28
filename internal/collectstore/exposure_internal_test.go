package collectstore

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/config"
	"github.com/imkerbos/Distill/internal/identity"
	"github.com/imkerbos/Distill/internal/mysqlregistry"
	"github.com/imkerbos/Distill/internal/replay"
	"github.com/imkerbos/Distill/internal/snapshot"
	"github.com/imkerbos/Distill/internal/snapshotstore"
)

// exposureClusterID 是本文件专用的测试集群。
//
// 不与 collectstore_test.go 的 collectedID / silentID 共用：那两个常量
// 属于外部测试包（package collectstore_test），而本文件要直接调用
// readServicesAt / readPodsAt / podRefOf —— 三个都不导出，只有内部测试包
// （package collectstore）够得着，因此这里需要一套自己的最小夹具。
const exposureClusterID = "col-exposure-a"

var exposureRunAt = time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)

// exposureTestDB 打开一个连到真实测试库的连接，只清理本文件用到的这个
// 集群，不碰 collectstore_test.go 那一套夹具的数据。
func exposureTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("DISTILL_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("DISTILL_TEST_MYSQL_DSN not set; skipping MySQL integration test")
	}
	cfg := config.DatabaseConfig{DSN: dsn, MaxOpenConns: 5, MaxIdleConns: 2}
	db, err := mysqlregistry.Open(cfg)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := mysqlregistry.Migrate(cfg, "../../migrations"); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}

	clean := func() {
		for _, stmt := range []string{
			"DELETE FROM observed_service WHERE cluster_id = ?",
			"DELETE FROM observed_pod WHERE cluster_id = ?",
			"DELETE FROM collection_run_resource WHERE cluster_id = ?",
			"DELETE FROM collection_run_failure WHERE cluster_id = ?",
			"DELETE FROM collection_warning WHERE cluster_id = ?",
			"DELETE FROM collection_run WHERE cluster_id = ?",
		} {
			if _, err := db.Exec(stmt, exposureClusterID); err != nil {
				t.Fatalf("clean: %q: %v", stmt, err)
			}
		}
		if _, err := db.Exec(`DELETE FROM cluster WHERE cluster_id = ?`, exposureClusterID); err != nil {
			t.Fatalf("clean cluster: %v", err)
		}
	}
	clean()
	if _, err := db.Exec(
		`INSERT INTO cluster
		   (cluster_id, display_name, pod_cidr, node_cidr, ccnp_present,
		    onboard_state, created_at, updated_at)
		 VALUES (?, ?, '172.16.0.0/16', '10.170.48.0/24', 0, 'REGISTERED', NOW(6), NOW(6))`,
		exposureClusterID, exposureClusterID); err != nil {
		t.Fatalf("seed cluster: %v", err)
	}

	t.Cleanup(func() {
		clean()
		_ = db.Close()
	})
	return db
}

// saveExposureRun 落一次带 Service 暴露信息与 Pod 命名端口的采集运行。
func saveExposureRun(t *testing.T, db *sql.DB) {
	t.Helper()
	store := snapshotstore.New(db)
	if err := store.Save(context.Background(), snapshot.Run{
		Status: snapshot.RunOK, StartedAt: exposureRunAt, FinishedAt: exposureRunAt,
		Observation: snapshot.Observation{
			ClusterID: exposureClusterID, RunID: "run-1", ObservedAt: exposureRunAt,
			Services: []snapshot.Service{{
				ClusterID: exposureClusterID, Namespace: "uat-kafka", Name: "kafka-0-external",
				Type: "LoadBalancer", Selector: map[string]string{"app": "kafka"},
				LoadBalancerIngressIPs:   []string{"10.170.48.193"},
				LoadBalancerSourceRanges: []string{"10.0.0.0/8", "172.16.0.0/16"},
			}},
			Pods: []snapshot.Pod{{
				ClusterID: exposureClusterID, Namespace: "uat-kafka", Name: "kafka-0",
				UID: "aaaaaaaa-0000-4000-8000-00000000000a", Phase: "Running", IP: "172.16.5.7",
				NamedPorts: []snapshot.NamedPort{{Name: "kafka-external", Port: 9095, Protocol: "TCP"}},
			}},
		},
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
}

// readServicesAt 必须把暴露信息原样交回来，不是只看非空。
//
// 逐字段比对：一个把 lb_source_ranges 读进 LoadBalancerIngressIPs 的实现，
// 在只看非空的断言下照样绿，而它会让 kafka 的放行范围变成三个入口地址。
func TestReadServicesAtCarriesExposureInformation(t *testing.T) {
	db := exposureTestDB(t)
	saveExposureRun(t, db)

	r := &Reader{db: db}
	d := described{clusterID: exposureClusterID, anchor: exposureRunAt}
	services, err := r.readServicesAt(context.Background(), d)
	if err != nil {
		t.Fatalf("readServicesAt() error = %v", err)
	}
	if len(services) != 1 {
		t.Fatalf("readServicesAt() = %d services, want 1", len(services))
	}
	svc := services[0]
	wantIngress := []string{"10.170.48.193"}
	wantRanges := []string{"10.0.0.0/8", "172.16.0.0/16"}
	if len(svc.LoadBalancerIngressIPs) != 1 || svc.LoadBalancerIngressIPs[0] != wantIngress[0] {
		t.Errorf("LoadBalancerIngressIPs = %v, want %v", svc.LoadBalancerIngressIPs, wantIngress)
	}
	if len(svc.LoadBalancerSourceRanges) != 2 ||
		svc.LoadBalancerSourceRanges[0] != wantRanges[0] || svc.LoadBalancerSourceRanges[1] != wantRanges[1] {
		t.Errorf("LoadBalancerSourceRanges = %v, want %v", svc.LoadBalancerSourceRanges, wantRanges)
	}
}

// 迁移之前写下的行读回来必须是 nil（没采过），不是 []（采过、是空的）。
//
// **今天没有任何消费方分得开这两者**：推导层判的是 len(...) == 0，两条路
// 都报成缺口。这条用例守的不是一个当下的行为差异，而是这份区分在存储层
// 不被抹掉 —— 抹掉是不可逆的（migrations/000032），而它要承载的是还没写
// 的那个判断：把老快照的缺口标成 DEGRADED 而不是缺失，需要的正是
// "这一行到底采没采过"。读回来先变成 [] 的话，那个判断永远无从下手。
func TestReadServicesAtKeepsPreMigrationRowsNil(t *testing.T) {
	db := exposureTestDB(t)
	saveExposureRun(t, db)
	if _, err := db.Exec(
		`UPDATE observed_service SET lb_ingress_ips = NULL, lb_source_ranges = NULL
		  WHERE cluster_id = ? AND name = ?`, exposureClusterID, "kafka-0-external"); err != nil {
		t.Fatalf("update service: %v", err)
	}

	r := &Reader{db: db}
	d := described{clusterID: exposureClusterID, anchor: exposureRunAt}
	services, err := r.readServicesAt(context.Background(), d)
	if err != nil {
		t.Fatalf("readServicesAt() error = %v", err)
	}
	if len(services) != 1 {
		t.Fatalf("readServicesAt() = %d services, want 1", len(services))
	}
	if services[0].LoadBalancerIngressIPs != nil {
		t.Errorf("LoadBalancerIngressIPs = %v, want nil", services[0].LoadBalancerIngressIPs)
	}
	if services[0].LoadBalancerSourceRanges != nil {
		t.Errorf("LoadBalancerSourceRanges = %v, want nil", services[0].LoadBalancerSourceRanges)
	}
}

// 命名端口必须走完整条路：落库 → snapshotstore 写入 → collectstore 读回
// （readPodsAt）→ podRefOf 补进求值引擎认的 PodRef，协议一起带对。
//
// 只测其中一段测不出这份接线：只测写入证明不了读得回来，只测 podRefOf 的
// 转换逻辑（用手搭的 traffic）证明不了数据库里的 JSON 列真的解得开、
// 也证明不了 readPodsAt 真的把它塞进了 observedPod。
func TestNamedPortSurvivesStoreReadAndPodRefConversion(t *testing.T) {
	db := exposureTestDB(t)
	saveExposureRun(t, db)

	r := &Reader{db: db}
	d := described{clusterID: exposureClusterID, anchor: exposureRunAt}
	pods, err := r.readPodsAt(context.Background(), d)
	if err != nil {
		t.Fatalf("readPodsAt() error = %v", err)
	}
	if len(pods) != 1 {
		t.Fatalf("readPodsAt() = %d pods, want 1", len(pods))
	}
	if len(pods[0].namedPorts) != 1 {
		t.Fatalf("readPodsAt() named ports = %+v, want exactly one", pods[0].namedPorts)
	}

	tr := traffic{
		described: d,
		pods: map[podKey]observedPod{
			{namespace: pods[0].namespace, name: pods[0].name}: pods[0],
		},
	}
	ref, ok := tr.podRefOf(identity.Identity{Namespace: "uat-kafka", PodName: "kafka-0"}, "172.16.5.7")
	if !ok {
		t.Fatalf("podRefOf() ok = false, want true")
	}
	want := replay.NamedPort{Name: "kafka-external", Port: 9095, Protocol: replay.ProtocolTCP}
	if len(ref.NamedPorts) != 1 || ref.NamedPorts[0] != want {
		t.Errorf("PodRef.NamedPorts = %+v, want [%+v]", ref.NamedPorts, want)
	}
}

// Pod 侧的 NULL 语义与 Service 侧同一条理由：迁移之前的行没有命名端口
// 可采，读回来必须是 nil，不是靠 podRefOf 悄悄补一个空切片。
func TestReadPodsAtKeepsPreMigrationRowsNil(t *testing.T) {
	db := exposureTestDB(t)
	saveExposureRun(t, db)
	if _, err := db.Exec(
		`UPDATE observed_pod SET named_ports = NULL WHERE cluster_id = ? AND name = ?`,
		exposureClusterID, "kafka-0"); err != nil {
		t.Fatalf("update pod: %v", err)
	}

	r := &Reader{db: db}
	d := described{clusterID: exposureClusterID, anchor: exposureRunAt}
	pods, err := r.readPodsAt(context.Background(), d)
	if err != nil {
		t.Fatalf("readPodsAt() error = %v", err)
	}
	if len(pods) != 1 {
		t.Fatalf("readPodsAt() = %d pods, want 1", len(pods))
	}
	if pods[0].namedPorts != nil {
		t.Errorf("namedPorts = %+v, want nil", pods[0].namedPorts)
	}
}
