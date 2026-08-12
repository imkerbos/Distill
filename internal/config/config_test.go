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

// sampleHostKey 是一条 known_hosts 记录，供校验配置相关的用例复用。
//
// 它是一把真实的 ed25519 公钥，但只用于这些用例 —— 公钥不是机密，
// 配置里的 host key 本就与凭据分开存放（spec §2.2）。
const sampleHostKey = "gitlab.example.com ssh-ed25519 " +
	"AAAAC3NzaC1lZDI1NTE5AAAAIKyjWioKIYrbPTzY9F8JKIElwSThZ4xuqtqQPGo9tDIg"

// verificationYAML 拼一份带 secrets / gitverify 两段的完整配置。
func verificationYAML(t *testing.T, secretsAndVerify string) string {
	t.Helper()
	return writeYAML(t, `
server:
  addr: ":10100"
auth:
  users:
    - username: demo
      password_hash: "x"
database:
  dsn: "root:x@tcp(mysql:3306)/distill?parseTime=true&loc=UTC"
`+secretsAndVerify)
}

func TestSecretsAndGitVerifySectionsRoundTrip(t *testing.T) {
	p := verificationYAML(t, `
secrets:
  project: distill-prod
  prefix: distill-git-
gitverify:
  timeout: 7s
  host_keys: "`+sampleHostKey+`"
`)
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Secrets.Project != "distill-prod" || cfg.Secrets.Prefix != "distill-git-" {
		t.Errorf("Secrets = %+v, want the values from the file", cfg.Secrets)
	}
	if cfg.GitVerify.Timeout != 7*time.Second {
		t.Errorf("GitVerify.Timeout = %v, want 7s", cfg.GitVerify.Timeout)
	}
	if cfg.GitVerify.HostKeys != sampleHostKey {
		t.Errorf("GitVerify.HostKeys = %q, want the known_hosts line from the file", cfg.GitVerify.HostKeys)
	}
}

func TestSecretsDirRoundTrips(t *testing.T) {
	p := verificationYAML(t, `
secrets:
  dir: /run/secrets/distill
gitverify:
  host_keys: "`+sampleHostKey+`"
`)
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Secrets.Dir != "/run/secrets/distill" {
		t.Errorf("Secrets.Dir = %q, want /run/secrets/distill", cfg.Secrets.Dir)
	}
}

// 后端由配置字段选出来，两种形态各自映射到一个取值。
//
// 这一层是「配置说了什么」；「据此构造什么」在 cmd/distill-api 那边，
// 两边各有断言，谁被改坏了都说得出是哪一边。
func TestSecretsBackendFollowsTheConfiguredFields(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.SecretsConfig
		want config.SecretsBackend
	}{
		{"dir", config.SecretsConfig{Dir: "/run/secrets/distill"}, config.SecretsBackendDir},
		{"secret manager", config.SecretsConfig{Project: "distill-prod", Prefix: "distill-git-"}, config.SecretsBackendSecretManager},
		{"none", config.SecretsConfig{}, config.SecretsBackendNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.Backend(); got != tt.want {
				t.Errorf("Backend() = %q, want %q", got, tt.want)
			}
		})
	}
}

// dir 与 project/prefix 同时出现必须拒绝。
//
// 无论按哪一边解释，都有一半配置是死的，而写下它的人不会知道是哪一半。
// 最坏的一种落法是生产进程从本地目录读私钥 —— 那不是一次会被发现的
// 误配，而是一个正常起来、正常校验、只是身份来源错了的进程。
func TestSecretsWithBothBackendsIsRejected(t *testing.T) {
	p := verificationYAML(t, `
secrets:
  project: distill-prod
  prefix: distill-git-
  dir: /run/secrets/distill
gitverify:
  host_keys: "`+sampleHostKey+`"
`)
	if _, err := config.Load(p); err == nil {
		t.Fatal("Load() = nil error, want an error when both secrets backends are configured")
	}
}

// 超时省略时补 spec §4 的 10 秒，而不是把 0 交给 gitverify.New ——
// 那个 0 会被它拒绝，进程起不来，报错却说不出是哪个字段。
func TestGitVerifyTimeoutDefaultsToTenSeconds(t *testing.T) {
	p := verificationYAML(t, `
secrets:
  dir: /run/secrets/distill
gitverify:
  host_keys: "`+sampleHostKey+`"
`)
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.GitVerify.Timeout != 10*time.Second {
		t.Errorf("GitVerify.Timeout = %v, want the 10s default from spec §4", cfg.GitVerify.Timeout)
	}
}

// 配了凭据解析却没配 host key：gitverify.New 会直接构造失败，且没有
// 「未配置就不校验」的回退分支。这种配置必须在启动时就被拒绝 ——
// 一个起得来但每次校验都失败的进程，比一个起不来的进程更难发现。
func TestSecretsWithoutHostKeysIsRejected(t *testing.T) {
	p := verificationYAML(t, `
secrets:
  dir: /run/secrets/distill
`)
	if _, err := config.Load(p); err == nil {
		t.Fatal("Load() = nil error, want an error when secrets is configured without gitverify.host_keys")
	}
}

// Secret Manager 后端缺 prefix 或缺 project 都必须拒绝。
//
// 空 prefix 一样能拼出合法资源路径，所以它不会在运行时报错 —— 它只是把
// 围栏从「项目里的这批 secret」放大成「项目里的任意 secret」，而那正是
// spec §2.1 用前缀换来的东西。缺失只能在启动时拦。
func TestSecretManagerBackendNeedsBothProjectAndPrefix(t *testing.T) {
	tests := map[string]string{
		"missing prefix":  "  project: distill-prod\n",
		"missing project": "  prefix: distill-git-\n",
	}
	for name, secretsBlock := range tests {
		t.Run(name, func(t *testing.T) {
			p := verificationYAML(t, "secrets:\n"+secretsBlock+`gitverify:
  host_keys: "`+sampleHostKey+`"
`)
			if _, err := config.Load(p); err == nil {
				t.Fatal("Load() = nil error, want an error for an incomplete Secret Manager backend")
			}
		})
	}
}

// 负超时不会被 applyDefaults 兜住（它只补 0 值），必须由 validate 拦下。
func TestNonPositiveGitVerifyTimeoutIsRejected(t *testing.T) {
	p := verificationYAML(t, `
secrets:
  dir: /run/secrets/distill
gitverify:
  timeout: -1s
  host_keys: "`+sampleHostKey+`"
`)
	if _, err := config.Load(p); err == nil {
		t.Fatal("Load() = nil error, want an error for a non-positive gitverify.timeout")
	}
}

// 不配 secrets 是一种正常形态（demo 就是）：不做校验，结论一律
// NOT_VERIFIED。此时不该因为缺 host key 而拒绝启动。
func TestConfigWithoutSecretsNeedsNoHostKeys(t *testing.T) {
	p := verificationYAML(t, "")
	if _, err := config.Load(p); err != nil {
		t.Fatalf("Load() error = %v, want a config with no secrets section to be valid", err)
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
