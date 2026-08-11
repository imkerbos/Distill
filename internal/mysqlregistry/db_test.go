package mysqlregistry_test

import (
	"os"
	"testing"

	"github.com/imkerbos/Distill/internal/config"
	"github.com/imkerbos/Distill/internal/mysqlregistry"
)

// testDSN 返回集成测试用的 DSN；未设置时跳过。
//
// 用真实 MySQL 而非 mock：本包要验证的正是事务与外键的真实行为，
// 而这两样恰恰是 mock 唯一伪造不了的部分。
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("DISTILL_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("DISTILL_TEST_MYSQL_DSN not set; skipping MySQL integration test")
	}
	return dsn
}

func TestMigrateUpDownUpIsRepeatable(t *testing.T) {
	cfg := config.DatabaseConfig{DSN: testDSN(t), MaxOpenConns: 5, MaxIdleConns: 2}

	db, err := mysqlregistry.Open(cfg)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = db.Close() }()

	const dir = "../../migrations"
	if err := mysqlregistry.Migrate(cfg, dir); err != nil {
		t.Fatalf("first Migrate() error = %v", err)
	}
	if err := mysqlregistry.Rollback(cfg, dir); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	// 回滚后再迁一次：一个只能跑一次的迁移在真实运维里等于不能回滚。
	if err := mysqlregistry.Migrate(cfg, dir); err != nil {
		t.Fatalf("second Migrate() error = %v", err)
	}

	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM information_schema.tables
		 WHERE table_schema = DATABASE() AND table_name = 'cluster'`,
	).Scan(&n); err != nil {
		t.Fatalf("query information_schema: %v", err)
	}
	if n != 1 {
		t.Errorf("cluster table count = %d, want 1", n)
	}
}

// 运行时连接池必须拒绝一次请求里的多条语句：这是唯一区分"注入影响一条
// 语句"和"注入影响任意语句链"的边界，而这个库里存着审计记录。
// 一旦 multiStatements 漂回运行时 DSN，这个测试会失败——它是回退闸门。
func TestOpenDoesNotAllowMultiStatements(t *testing.T) {
	db, err := mysqlregistry.Open(config.DatabaseConfig{
		DSN: testDSN(t), MaxOpenConns: 5, MaxIdleConns: 2,
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.Exec("SELECT 1; SELECT 2;"); err == nil {
		t.Fatal("Exec() with two statements succeeded, want the runtime pool to reject multi-statement queries")
	}
}
