package mysqlregistry_test

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/imkerbos/Distill/internal/config"
	"github.com/imkerbos/Distill/internal/mysqlregistry"
	"github.com/imkerbos/Distill/internal/registry"
)

// newTestStore 建库、迁移到最新、清空数据，返回可用的 Store。
func newTestStore(t *testing.T) (*mysqlregistry.Store, *sql.DB) {
	t.Helper()
	cfg := config.DatabaseConfig{DSN: testDSN(t), MaxOpenConns: 5, MaxIdleConns: 2}
	db, err := mysqlregistry.Open(cfg)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := mysqlregistry.Migrate(cfg, "../../migrations"); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	// 每个测试从干净状态开始。删除顺序与外键依赖相反。
	for _, tbl := range []string{
		"audit_log", "policy_import", "cluster_git_binding",
		"cluster_health_check_source", "cluster_apiserver", "cluster",
	} {
		//nolint:gosec // G202: tbl comes from the fixed literal slice above, not external input
		if _, err := db.Exec("DELETE FROM " + tbl); err != nil {
			t.Fatalf("clean %s: %v", tbl, err)
		}
	}
	t.Cleanup(func() { _ = db.Close() })
	return mysqlregistry.New(db), db
}

func sampleCluster() registry.Cluster {
	return registry.Cluster{
		ID: "prod-asia-1", DisplayName: "Asia Prod",
		PodCIDR: "10.4.0.0/14", NodeCIDR: "10.128.0.0/20",
		CCNPPresent: false, State: registry.StateRegistered,
		APIServers:         []registry.APIServer{{Host: "10.9.0.2", CIDR: "10.9.0.0/28", Port: 443}},
		HealthCheckSources: []string{"35.191.0.0/16", "130.211.0.0/22"},
		Git: &registry.GitBinding{
			RepoURL: "https://gitlab.example.com/net/policies.git",
			Branch:  "main", PolicyPath: "clusters/prod-asia-1",
		},
	}
}

func TestCreateAndReadBackCluster(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "admin"}

	if err := s.CreateCluster(ctx, actor, sampleCluster()); err != nil {
		t.Fatalf("CreateCluster() error = %v", err)
	}
	got, ok, err := s.Cluster(ctx, "prod-asia-1")
	if err != nil || !ok {
		t.Fatalf("Cluster() = %v, %v, %v", got, ok, err)
	}
	if got.NodeCIDR != "10.128.0.0/20" {
		t.Errorf("NodeCIDR = %q, want 10.128.0.0/20", got.NodeCIDR)
	}
	if len(got.APIServers) != 1 || got.APIServers[0].Port != 443 {
		t.Errorf("APIServers = %+v, want one entry on 443", got.APIServers)
	}
	if len(got.HealthCheckSources) != 2 {
		t.Errorf("HealthCheckSources = %v, want 2 entries", got.HealthCheckSources)
	}
	if got.Git == nil || got.Git.PolicyPath != "clusters/prod-asia-1" {
		t.Errorf("Git = %+v, want the registered binding", got.Git)
	}
}

// 这是本包存在的理由：审计与业务写入必须同生共死。
// 业务写入失败时若审计行留了下来，审计就在记录从未发生过的事。
func TestAuditRollsBackWithTheBusinessWrite(t *testing.T) {
	s, db := newTestStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "admin"}

	if err := s.CreateCluster(ctx, actor, sampleCluster()); err != nil {
		t.Fatalf("first CreateCluster() error = %v", err)
	}
	// 同一个 ID 再插一次：主键冲突，业务写入必然失败。
	if err := s.CreateCluster(ctx, actor, sampleCluster()); err == nil {
		t.Fatal("duplicate CreateCluster() succeeded, want an error")
	}

	var audits int
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_log`).Scan(&audits); err != nil {
		t.Fatalf("count audit_log: %v", err)
	}
	if audits != 1 {
		t.Errorf("audit_log rows = %d, want 1 — the failed write must not leave an audit row", audits)
	}
}

func TestUpdateClusterWritesAudit(t *testing.T) {
	s, db := newTestStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "admin"}

	if err := s.CreateCluster(ctx, actor, sampleCluster()); err != nil {
		t.Fatalf("CreateCluster() error = %v", err)
	}
	c := sampleCluster()
	c.DisplayName = "Asia Production"
	if err := s.UpdateCluster(ctx, actor, c); err != nil {
		t.Fatalf("UpdateCluster() error = %v", err)
	}

	got, _, _ := s.Cluster(ctx, "prod-asia-1")
	if got.DisplayName != "Asia Production" {
		t.Errorf("DisplayName = %q, want the updated value", got.DisplayName)
	}
	var actions int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM audit_log WHERE action = 'UPDATE_CLUSTER'`).Scan(&actions); err != nil {
		t.Fatalf("count: %v", err)
	}
	if actions != 1 {
		t.Errorf("UPDATE_CLUSTER audit rows = %d, want 1", actions)
	}
}

func TestUpdateMissingClusterReturnsNotFound(t *testing.T) {
	s, _ := newTestStore(t)
	err := s.UpdateCluster(context.Background(),
		registry.Actor{Username: "admin"}, sampleCluster())
	if !errors.Is(err, mysqlregistry.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// 软删除之后集群不再出现在列表里，但它的审计记录必须仍然查得到 ——
// 一次下线操作不该让平台失忆。
func TestSoftDeleteHidesClusterButKeepsAudit(t *testing.T) {
	s, db := newTestStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "admin"}

	if err := s.CreateCluster(ctx, actor, sampleCluster()); err != nil {
		t.Fatalf("CreateCluster() error = %v", err)
	}
	if err := s.SoftDeleteCluster(ctx, actor, "prod-asia-1"); err != nil {
		t.Fatalf("SoftDeleteCluster() error = %v", err)
	}

	if _, ok, _ := s.Cluster(ctx, "prod-asia-1"); ok {
		t.Error("Cluster() still returns a soft-deleted cluster")
	}
	list, err := s.Clusters(ctx)
	if err != nil {
		t.Fatalf("Clusters() error = %v", err)
	}
	if len(list) != 0 {
		t.Errorf("Clusters() = %d entries, want 0", len(list))
	}

	var audits int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM audit_log WHERE cluster_id = 'prod-asia-1'`).Scan(&audits); err != nil {
		t.Fatalf("count: %v", err)
	}
	// 恰好两条，不是「至少两条」：多出来的审计行意味着某条写路径重复
	// 记账，而复盘时一条被记了两次的操作与两次真实操作无法区分。
	if audits != 2 {
		t.Errorf("audit rows for the deleted cluster = %d, want exactly 2 (create + delete)", audits)
	}
}

func TestCreateClusterRejectsInvalidInput(t *testing.T) {
	s, _ := newTestStore(t)
	c := sampleCluster()
	c.PodCIDR = "10.4.0/14"
	err := s.CreateCluster(context.Background(), registry.Actor{Username: "admin"}, c)
	if !errors.Is(err, registry.ErrInvalid) {
		t.Errorf("err = %v, want ErrInvalid", err)
	}
}

// 重复注册一个已有的集群 ID 是操作者的输入问题，不是服务故障。
//
// 判据是「该不该计入服务错误率」：翻译之前，1062 会一路冒泡成
// CodeInternal + HTTP 500，让注册页在一次正常的手滑上显示「服务器错误」，
// 并把它计进可用性指标。翻译落在捕获驱动错误的这一层，HTTP 层因此
// 只需要认识 registry.ErrInvalid，不必按 MySQL 错误号分支。
func TestDuplicateClusterIDIsAnInputError(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "admin"}

	if err := s.CreateCluster(ctx, actor, sampleCluster()); err != nil {
		t.Fatalf("first CreateCluster() error = %v", err)
	}
	err := s.CreateCluster(ctx, actor, sampleCluster())
	if !errors.Is(err, registry.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid — a duplicate cluster id is the caller's problem", err)
	}
	// 回传通道只读 InvalidError.Detail，所以文案必须点名冲突的是什么；
	// 同时它必须是我们自己写的话，不能是驱动那句带表名与键名的原文。
	var ie *registry.InvalidError
	if !errors.As(err, &ie) {
		t.Fatalf("err = %v, want an *InvalidError carrying a returnable Detail", err)
	}
	if !strings.Contains(ie.Detail, "prod-asia-1") {
		t.Errorf("Detail = %q, want it to name the conflicting cluster id", ie.Detail)
	}
	for _, leaked := range []string{"Duplicate entry", "PRIMARY", "Error 1062"} {
		if strings.Contains(ie.Detail, leaked) {
			t.Errorf("Detail = %q leaked driver text %q", ie.Detail, leaked)
		}
	}
}

// registry.Cluster → MySQL → registry.Cluster 的整体比对。
//
// 逐字段挑着断言挡不住写错值：审阅时的实证是，把 insertChildren 里的
// apiserver cidr 写成 "0.0.0.0/0"、把 CreateCluster 的 onboard_state 写成
// "READY"，本包全部测试依旧通过。这两个字段一个是 control-plane Baseline
// 的推导依据、一个决定集群能不能出候选策略。
func TestClusterSurvivesAFullRoundTripThroughMySQL(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	in := sampleCluster()
	// 两个 apiserver：HA 控制面通常不止一个端点，只写一条测不出
	// insertChildren 少插了后面几行这种缺口。
	in.APIServers = []registry.APIServer{
		{Host: "10.9.0.3", CIDR: "10.9.0.16/28", Port: 6443},
		{Host: "10.9.0.2", CIDR: "10.9.0.0/28", Port: 443},
	}
	if err := s.CreateCluster(ctx, registry.Actor{Username: "admin"}, in); err != nil {
		t.Fatalf("CreateCluster() error = %v", err)
	}
	got, ok, err := s.Cluster(ctx, in.ID)
	if err != nil || !ok {
		t.Fatalf("Cluster() = %+v, %v, %v", got, ok, err)
	}

	// 期望值里子表按 host / cidr 升序，与写入顺序不同：读路径带
	// ORDER BY，图的是两次读同一个集群必然得到同一份结果 —— 缺了它，
	// 一份候选策略会因为 Baseline 输入的顺序抖动而在两次预览之间变样。
	// 顺序对推导本身无意义（这些网段最终展开成一组规则），但「稳定」有。
	want := in
	want.APIServers = []registry.APIServer{
		{Host: "10.9.0.2", CIDR: "10.9.0.0/28", Port: 443},
		{Host: "10.9.0.3", CIDR: "10.9.0.16/28", Port: 6443},
	}
	want.HealthCheckSources = []string{"130.211.0.0/22", "35.191.0.0/16"}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("round-tripped cluster =\n%+v\nwant\n%+v", got, want)
	}
	if got.Git == nil || *got.Git != *want.Git {
		t.Errorf("git binding = %+v, want %+v", got.Git, want.Git)
	}
}
