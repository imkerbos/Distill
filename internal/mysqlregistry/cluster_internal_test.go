package mysqlregistry

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/config"
	"github.com/imkerbos/Distill/internal/registry"
)

// 本文件是白盒测试：需要直接构造 Store 并替换其 now 钩子，
// 才能不靠 goroutine 就精确命中"预检已过、事务内 UPDATE 还没执行"
// 这个窗口，因此单独放在 package mysqlregistry 而非 _test 包里
// （与 internal/store/severity_internal_test.go 是同一惯例）。

// internalTestDSN 复刻 cluster_test.go 里 testDSN 的行为：未设置时跳过。
func internalTestDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("DISTILL_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("DISTILL_TEST_MYSQL_DSN not set; skipping MySQL integration test")
	}
	return dsn
}

// newRaceTestStore 建库、迁移、清空数据，返回一个可以自由替换 now 的 Store。
func newRaceTestStore(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	cfg := config.DatabaseConfig{DSN: internalTestDSN(t), MaxOpenConns: 5, MaxIdleConns: 2}
	db, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := Migrate(cfg, "../../migrations"); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
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
	return New(db), db
}

func raceSampleCluster() registry.Cluster {
	return registry.Cluster{
		ID: "prod-asia-1", DisplayName: "Asia Prod",
		PodCIDR: "10.4.0.0/14", NodeCIDR: "10.128.0.0/20",
		CCNPPresent: false, State: registry.StateRegistered,
	}
}

// countAuditRows 是个小助手，避免下面两个测试重复写同一段 SQL。
func countAuditRows(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_log`).Scan(&n); err != nil {
		t.Fatalf("count audit_log: %v", err)
	}
	return n
}

// armConcurrentDelete 返回一个 now() 替身：第一次被调用时，先在一条独立的
// 自动提交连接上把目标集群标记为已删除并提交，再返回当前时间。
//
// s.now() 是 UpdateCluster/SoftDeleteCluster 的事务闭包里，预检结束之后、
// 真正执行 UPDATE 之前唯一必然会被求值一次的东西：Go 对函数调用参数按顺序
// 求值，把并发删除的副作用挂在这里，就能保证它在 tx.ExecContext 真正把
// UPDATE 发给数据库之前已经提交——不必用 goroutine 猜时序，这个先后关系
// 由 Go 的求值顺序保证，是确定性的。
func armConcurrentDelete(t *testing.T, db *sql.DB, clusterID string) func() time.Time {
	t.Helper()
	done := false
	return func() time.Time {
		if !done {
			done = true
			if _, err := db.Exec(
				`UPDATE cluster SET deleted_at = NOW(6) WHERE cluster_id = ?`, clusterID,
			); err != nil {
				t.Fatalf("simulate concurrent delete: %v", err)
			}
		}
		return time.Now().UTC()
	}
}

// 复盘这个包存在的理由的另一半：并发下线让 UpdateCluster 的存在性预检
// 通过之后，事务内的 UPDATE 才发现行已经不在了。若事务内的写入不检查
// RowsAffected，这次调用会返回 nil 并留下一条描述"更新成功"这件从未
// 发生过的事的审计行。
func TestUpdateClusterDetectsConcurrentDelete(t *testing.T) {
	s, db := newRaceTestStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "admin"}
	c := raceSampleCluster()

	if err := s.CreateCluster(ctx, actor, c); err != nil {
		t.Fatalf("CreateCluster() error = %v", err)
	}
	auditsBefore := countAuditRows(t, db)

	s.now = armConcurrentDelete(t, db, c.ID)
	c.DisplayName = "Asia Production"
	err := s.UpdateCluster(ctx, actor, c)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateCluster() err = %v, want ErrNotFound", err)
	}

	if auditsAfter := countAuditRows(t, db); auditsAfter != auditsBefore {
		t.Errorf("audit_log rows = %d, want unchanged from %d — "+
			"the lost update must not leave an audit row describing it", auditsAfter, auditsBefore)
	}
}

// 同一类竞态在 SoftDeleteCluster 里的样子：某个并发写入抢先把这一行
// 标记为已删除，SoftDeleteCluster 的存在性预检读到的仍是那之前的旧状态。
func TestSoftDeleteClusterDetectsConcurrentDelete(t *testing.T) {
	s, db := newRaceTestStore(t)
	ctx := context.Background()
	actor := registry.Actor{Username: "admin"}
	c := raceSampleCluster()

	if err := s.CreateCluster(ctx, actor, c); err != nil {
		t.Fatalf("CreateCluster() error = %v", err)
	}
	auditsBefore := countAuditRows(t, db)

	s.now = armConcurrentDelete(t, db, c.ID)
	err := s.SoftDeleteCluster(ctx, actor, c.ID)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("SoftDeleteCluster() err = %v, want ErrNotFound", err)
	}

	if auditsAfter := countAuditRows(t, db); auditsAfter != auditsBefore {
		t.Errorf("audit_log rows = %d, want unchanged from %d — "+
			"the lost delete must not leave an audit row describing it", auditsAfter, auditsBefore)
	}
}
