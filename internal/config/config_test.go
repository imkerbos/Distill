package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/config"
)

func writeYAML(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
}

func TestLoadReadsYAML(t *testing.T) {
	p := writeYAML(t, `
server:
  addr: ":10100"
  read_timeout: 5s
  write_timeout: 10s
  shutdown_timeout: 15s
auth:
  session_ttl: 8h
  users:
    - username: demo
      password_hash: "$2a$10$abcdefghijklmnopqrstuv"
database:
  dsn: "root:x@tcp(mysql:3306)/distill?parseTime=true&loc=UTC"
log:
  level: INFO
`)

	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Addr != ":10100" {
		t.Errorf("Addr = %q, want :10100", cfg.Server.Addr)
	}
	if cfg.Auth.SessionTTL != 8*time.Hour {
		t.Errorf("SessionTTL = %v, want 8h", cfg.Auth.SessionTTL)
	}
	if len(cfg.Auth.Users) != 1 || cfg.Auth.Users[0].Username != "demo" {
		t.Fatalf("Users = %+v, want one user named demo", cfg.Auth.Users)
	}
}

// 配置错误必须在启动时暴露，而不是等到第一个请求打进来才发现。
func TestLoadRejectsMissingAddr(t *testing.T) {
	p := writeYAML(t, `
auth:
  session_ttl: 8h
  users:
    - username: demo
      password_hash: "x"
`)
	if _, err := config.Load(p); err == nil {
		t.Fatal("Load() = nil error, want error for missing server.addr")
	}
}

func TestLoadRejectsNoUsers(t *testing.T) {
	p := writeYAML(t, "server:\n  addr: \":10100\"\nauth:\n  session_ttl: 8h\n")
	if _, err := config.Load(p); err == nil {
		t.Fatal("Load() = nil error, want error when no users are configured")
	}
}

func TestLoadRejectsIncompleteUser(t *testing.T) {
	p := writeYAML(t, `
server:
  addr: ":10100"
auth:
  session_ttl: 8h
  users:
    - username: demo
`)
	if _, err := config.Load(p); err == nil {
		t.Fatal("Load() = nil error, want error for a user with no password_hash")
	}
}

// 环境变量优先于文件，容器里才能不重建镜像改配置。
func TestEnvOverridesFile(t *testing.T) {
	p := writeYAML(t, `
server:
  addr: ":10100"
auth:
  session_ttl: 8h
  users:
    - username: demo
      password_hash: "x"
database:
  dsn: "root:x@tcp(mysql:3306)/distill?parseTime=true&loc=UTC"
`)
	t.Setenv("DISTILL_SERVER__ADDR", ":19999")

	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Addr != ":19999" {
		t.Errorf("Addr = %q, want env override :19999", cfg.Server.Addr)
	}
}

// 用双下划线做嵌套分隔符，是为了让字段名里的单下划线不被误当成层级。
func TestEnvOverrideKeepsUnderscoresInFieldNames(t *testing.T) {
	p := writeYAML(t, `
server:
  addr: ":10100"
  read_timeout: 5s
auth:
  session_ttl: 8h
  users:
    - username: demo
      password_hash: "x"
database:
  dsn: "root:x@tcp(mysql:3306)/distill?parseTime=true&loc=UTC"
`)
	t.Setenv("DISTILL_SERVER__READ_TIMEOUT", "42s")

	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.ReadTimeout != 42*time.Second {
		t.Errorf("ReadTimeout = %v, want 42s — a single underscore in the field name must survive the transform", cfg.Server.ReadTimeout)
	}
}

func TestDatabaseDSNIsRequired(t *testing.T) {
	// 环境变量必须先清掉，否则这个测试断言的不是它声称的东西。
	//
	// docker-compose 给 api 容器导出了 DISTILL_DATABASE__DSN，koanf 的 env
	// provider 会把它并进配置，于是 Load() 成功、测试失败 —— 而容器里正是
	// MySQL 集成测试唯一能跑起来的地方。反过来，在宿主机与 CI 上它之所以
	// 通过，只是因为那两处恰好没设这个变量。
	//
	// t.Setenv 先声明「本测试要改这个变量」，由它登记恢复；紧接着 Unsetenv
	// 把变量整个移除 —— 只置空串不够，那样 koanf 仍会并进一个空 DSN，测的
	// 就成了「空串也算缺失」而不是「配置里没写 dsn」。
	t.Setenv("DISTILL_DATABASE__DSN", "")
	if err := os.Unsetenv("DISTILL_DATABASE__DSN"); err != nil {
		t.Fatalf("unset DISTILL_DATABASE__DSN: %v", err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "no-db.yaml")
	//nolint:gosec // G306: 测试用临时文件，权限无关紧要
	if err := os.WriteFile(path, []byte(`
server:
  addr: ":10100"
auth:
  users:
    - username: admin
      password_hash: "x"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(path); err == nil {
		t.Fatal("Load() succeeded without database.dsn, want an error")
	}
}

// 连接池上限缺省时必须有默认值：一个没有上限的池会在故障时把
// MySQL 的连接数打满，而症状表现为平台无响应，与数据库无关。
func TestDatabaseDefaultsAreApplied(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "with-db.yaml")
	//nolint:gosec // G306: 测试用临时文件，权限无关紧要
	if err := os.WriteFile(path, []byte(`
server:
  addr: ":10100"
auth:
  users:
    - username: admin
      password_hash: "x"
database:
  dsn: "root:x@tcp(mysql:3306)/distill?parseTime=true&loc=UTC"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Database.MaxOpenConns <= 0 {
		t.Errorf("MaxOpenConns = %d, want a positive default", cfg.Database.MaxOpenConns)
	}
	if cfg.Database.ConnMaxLifetime <= 0 {
		t.Errorf("ConnMaxLifetime = %v, want a positive default", cfg.Database.ConnMaxLifetime)
	}
}
