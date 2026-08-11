package mysqlregistry_test

import (
	"context"
	"database/sql"
	"errors"
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
	if audits < 2 {
		t.Errorf("audit rows for the deleted cluster = %d, want at least 2 (create + delete)", audits)
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
