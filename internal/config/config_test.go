package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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

// minimalYAML 是一份只带启动必需项的配置，供各用例在其上追加内容。
const minimalYAML = `
server:
  addr: ":10100"
auth:
  bootstrap_user:
    username: demo
    password_hash: "$2a$10$abcdefghijklmnopqrstuv"
database:
  dsn: "root:x@tcp(mysql:3306)/distill?parseTime=true&loc=UTC"
log:
  level: INFO
`

func TestLoadReadsYAML(t *testing.T) {
	cfg, err := config.Load(writeYAML(t, minimalYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Addr != ":10100" {
		t.Errorf("Addr = %q, want :10100", cfg.Server.Addr)
	}
	if cfg.Auth.BootstrapUser.Username != "demo" {
		t.Errorf("BootstrapUser = %+v, want the account named demo", cfg.Auth.BootstrapUser)
	}
	if cfg.Log.Level != "INFO" {
		t.Errorf("Log.Level = %q, want INFO", cfg.Log.Level)
	}
}

// 迁走的键留在配置文件里必须让启动失败，并指出它去了哪里。
//
// 静默忽略是这里最坏的落法：操作者改一个超时、重启、观察到毫无变化，
// 而平台没有发出任何信号 —— 而把这些键挪进数据库，本来就是为了终结
// 「改了要重启」这件事（design doc §1、§1.1）。
func TestMovedConfigKeysFailStartupAndSayWhereTheyWent(t *testing.T) {
	// 每个键配一份完整的 YAML。值本身无所谓，出现即拒。
	//
	// 不用「最小配置 + 追加一段」拼：server 与 auth 两段在最小配置里已经
	// 存在，再追加一个同名顶层键会被 YAML 解析器当成重复键拒掉 —— 那样
	// 报的是语法错误，测的就不是这里要测的东西了。
	files := map[string]string{
		"gitverify.timeout":       minimalYAML + "gitverify:\n  timeout: 7s\n",
		"gitverify.host_keys":     minimalYAML + "gitverify:\n  host_keys: \"example.com ssh-ed25519 AAAA\"\n",
		"secrets.project":         minimalYAML + "secrets:\n  project: distill-prod\n",
		"secrets.prefix":          minimalYAML + "secrets:\n  prefix: distill-git-\n",
		"secrets.dir":             minimalYAML + "secrets:\n  dir: /run/secrets/distill\n",
		"auth.session_ttl":        strings.Replace(minimalYAML, "auth:\n", "auth:\n  session_ttl: 8h\n", 1),
		"server.read_timeout":     strings.Replace(minimalYAML, "server:\n", "server:\n  read_timeout: 5s\n", 1),
		"server.write_timeout":    strings.Replace(minimalYAML, "server:\n", "server:\n  write_timeout: 10s\n", 1),
		"server.shutdown_timeout": strings.Replace(minimalYAML, "server:\n", "server:\n  shutdown_timeout: 15s\n", 1),
	}
	for key, body := range files {
		t.Run(key, func(t *testing.T) {
			_, err := config.Load(writeYAML(t, body))
			if err == nil {
				t.Fatalf("Load() = nil error with %s still in the file, want a startup failure", key)
			}
			if !strings.Contains(err.Error(), key) {
				t.Errorf("error = %q, want it to name %s — an operator who cannot see which key is at fault cannot fix it",
					err, key)
			}
		})
	}
}

// 改了名的键同样要说出去向，理由与迁走的键一样。
//
// 少了这条，一份旧配置的表现是「auth.bootstrap_user is required」——
// 一句正确但没用的话：文件里明明写着账号。
func TestRenamedAuthUsersFailsStartupAndPointsAtTheNewKey(t *testing.T) {
	body := `
server:
  addr: ":10100"
auth:
  users:
    - username: demo
      password_hash: "x"
database:
  dsn: "root:x@tcp(mysql:3306)/distill?parseTime=true&loc=UTC"
`
	_, err := config.Load(writeYAML(t, body))
	if err == nil {
		t.Fatal("Load() = nil error with the old auth.users key, want a startup failure")
	}
	if !strings.Contains(err.Error(), "auth.users") || !strings.Contains(err.Error(), "auth.bootstrap_user") {
		t.Errorf("error = %q, want it to name both auth.users and auth.bootstrap_user", err)
	}
}

// 迁走的键从环境变量进来也必须被拦下。
//
// 容器里改配置走的正是环境变量（见 TestEnvOverridesFile）；只查文件的话，
// DISTILL_GITVERIFY__TIMEOUT 会安安静静地什么也不做。
func TestMovedKeysAreAlsoRejectedFromTheEnvironment(t *testing.T) {
	t.Setenv("DISTILL_GITVERIFY__TIMEOUT", "7s")

	_, err := config.Load(writeYAML(t, minimalYAML))
	if err == nil {
		t.Fatal("Load() = nil error with DISTILL_GITVERIFY__TIMEOUT set, want a startup failure")
	}
	if !strings.Contains(err.Error(), "gitverify.timeout") {
		t.Errorf("error = %q, want it to name gitverify.timeout", err)
	}
}

// 迁走的键必须返回 ErrInvalidConfig，与其他配置错误同类。
func TestMovedKeyErrorIsAnInvalidConfigError(t *testing.T) {
	_, err := config.Load(writeYAML(t, minimalYAML+"secrets:\n  dir: /run/secrets/distill\n"))
	if !errors.Is(err, config.ErrInvalidConfig) {
		t.Fatalf("Load() error = %v, want ErrInvalidConfig", err)
	}
}

// 配置错误必须在启动时暴露，而不是等到第一个请求打进来才发现。
func TestLoadRejectsMissingAddr(t *testing.T) {
	p := writeYAML(t, `
auth:
  bootstrap_user:
    username: demo
    password_hash: "x"
database:
  dsn: "root:x@tcp(mysql:3306)/distill?parseTime=true&loc=UTC"
`)
	if _, err := config.Load(p); err == nil {
		t.Fatal("Load() = nil error, want error for missing server.addr")
	}
}

// 没有引导账号就没有第一次登录，而建账号需要先登录（design doc §1.2）。
func TestLoadRejectsMissingBootstrapUser(t *testing.T) {
	p := writeYAML(t, "server:\n  addr: \":10100\"\ndatabase:\n  dsn: \"x\"\n")
	if _, err := config.Load(p); err == nil {
		t.Fatal("Load() = nil error, want error when no bootstrap user is configured")
	}
}

func TestLoadRejectsIncompleteBootstrapUser(t *testing.T) {
	p := writeYAML(t, `
server:
  addr: ":10100"
auth:
  bootstrap_user:
    username: demo
database:
  dsn: "x"
`)
	if _, err := config.Load(p); err == nil {
		t.Fatal("Load() = nil error, want error for a bootstrap user with no password_hash")
	}
}

// 环境变量优先于文件，容器里才能不重建镜像改配置。
func TestEnvOverridesFile(t *testing.T) {
	t.Setenv("DISTILL_SERVER__ADDR", ":19999")

	cfg, err := config.Load(writeYAML(t, minimalYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Addr != ":19999" {
		t.Errorf("Addr = %q, want env override :19999", cfg.Server.Addr)
	}
}

// 用双下划线做嵌套分隔符，是为了让字段名里的单下划线不被误当成层级。
//
// 留在文件里的键中只有 database.max_open_conns 这类还带下划线，这条断言
// 因此改用它 —— 原先用的 server.read_timeout 已经迁进数据库。
func TestEnvOverrideKeepsUnderscoresInFieldNames(t *testing.T) {
	t.Setenv("DISTILL_DATABASE__MAX_OPEN_CONNS", "42")

	cfg, err := config.Load(writeYAML(t, minimalYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Database.MaxOpenConns != 42 {
		t.Errorf("MaxOpenConns = %d, want 42 — a single underscore in the field name must survive the transform",
			cfg.Database.MaxOpenConns)
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

	p := writeYAML(t, `
server:
  addr: ":10100"
auth:
  bootstrap_user:
    username: admin
    password_hash: "x"
`)
	if _, err := config.Load(p); err == nil {
		t.Fatal("Load() succeeded without database.dsn, want an error")
	}
}

// 连接池上限缺省时必须有默认值：一个没有上限的池会在故障时把
// MySQL 的连接数打满，而症状表现为平台无响应，与数据库无关。
func TestDatabaseDefaultsAreApplied(t *testing.T) {
	cfg, err := config.Load(writeYAML(t, minimalYAML))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Database.MaxOpenConns <= 0 {
		t.Errorf("MaxOpenConns = %d, want a positive default", cfg.Database.MaxOpenConns)
	}
	if cfg.Database.ConnMaxLifetime <= 0 {
		t.Errorf("ConnMaxLifetime = %v, want a positive default", cfg.Database.ConnMaxLifetime)
	}
	if cfg.Database.ConnMaxLifetime != 30*time.Minute {
		t.Errorf("ConnMaxLifetime = %v, want the 30m default", cfg.Database.ConnMaxLifetime)
	}
}

// 日志级别缺省时补 INFO：一个没有级别的日志器起不来，而这是启动必需项。
func TestLogLevelDefaultsToInfo(t *testing.T) {
	p := writeYAML(t, `
server:
  addr: ":10100"
auth:
  bootstrap_user:
    username: demo
    password_hash: "x"
database:
  dsn: "root:x@tcp(mysql:3306)/distill?parseTime=true&loc=UTC"
`)
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Log.Level != "INFO" {
		t.Errorf("Log.Level = %q, want the INFO default", cfg.Log.Level)
	}
}

// 仓库里的 demo 配置必须能被加载。
//
// 它是本机开发环境唯一的配置文件：瘦身时漏改一处，症状是整个 dev 栈
// 起不来，而这条断言让那件事在 make check 里就暴露。
func TestDemoConfigLoads(t *testing.T) {
	// 与 TestDatabaseDSNIsRequired 同理：容器里跑测试时环境变量会参与合并，
	// 这里要断言的是文件本身自洽，所以先把可能存在的覆盖清掉。
	for _, k := range []string{"DISTILL_DATABASE__DSN", "DISTILL_SERVER__ADDR"} {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatalf("unset %s: %v", k, err)
		}
	}

	cfg, err := config.Load(filepath.Join("..", "..", "configs", "demo.yaml"))
	if err != nil {
		t.Fatalf("Load(configs/demo.yaml) error = %v", err)
	}
	if cfg.Auth.BootstrapUser.Username == "" {
		t.Error("demo config has no bootstrap user — the dev stack cannot be logged into")
	}
}

// withTLS 造一份带 TLS 路径的完整配置。
//
// 不在 minimalYAML 后面再追加一个 server 段：YAML 不允许重复的映射键，
// 那样拼出来的是一份根本解析不了的文件，而用例会因为"解析失败"通过 ——
// 一个测不到任何东西的绿灯。
func withTLS(cert, key string) string {
	out := "\nserver:\n  addr: \":10100\"\n"
	if cert != "" {
		out += "  tls_cert_file: " + cert + "\n"
	}
	if key != "" {
		out += "  tls_key_file: " + key + "\n"
	}
	return out + `
auth:
  bootstrap_user:
    username: demo
    password_hash: "$2a$10$abcdefghijklmnopqrstuv"
database:
  dsn: "root:x@tcp(mysql:3306)/distill?parseTime=true&loc=UTC"
log:
  level: INFO
`
}

// 证书与私钥必须同时给出。
//
// 只给一半是配置写了一半，而它最坏的落法是**静默退回明文监听** —— 一个
// 以为自己在跑 TLS 的部署，agent token 却在明文过网。配置错误要在启动时
// 暴露，不能等到有人抓包才发现。
func TestTLSCertAndKeyMustBeGivenTogether(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		ok   bool
	}{
		{"两个都不给：明文监听，本机开发用", minimalYAML, true},
		{"两个都给", withTLS("/tls/tls.crt", "/tls/tls.key"), true},
		{"只给证书", withTLS("/tls/tls.crt", ""), false},
		{"只给私钥", withTLS("", "/tls/tls.key"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.Load(writeYAML(t, tc.yaml))
			if tc.ok {
				if err != nil {
					t.Fatalf("Load() = %v, want it to succeed", err)
				}
				return
			}
			if err == nil {
				t.Fatal("只写了一半的 TLS 配置被接受了；它会静默退回明文监听")
			}
			if !errors.Is(err, config.ErrInvalidConfig) {
				t.Errorf("err = %v, want ErrInvalidConfig", err)
			}
			if !strings.Contains(err.Error(), "plaintext") {
				t.Errorf("err = %v，希望它说清后果是退回明文", err)
			}
		})
	}
}

// 两项都给出时要读得进来，否则 main 那边永远走不到 TLS 分支。
func TestTLSPathsAreRead(t *testing.T) {
	cfg, err := config.Load(writeYAML(t, withTLS("/tls/tls.crt", "/tls/tls.key")))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.TLSCertFile != "/tls/tls.crt" || cfg.Server.TLSKeyFile != "/tls/tls.key" {
		t.Errorf("TLS 路径 = %q / %q，没有被读进来", cfg.Server.TLSCertFile, cfg.Server.TLSKeyFile)
	}
}

// 证据记账周期缺省时补一个正值：推送式接入没有采集器那一轮循环可挂，
// 缺省为零会让平台悄无声息地不记账，而症状是每条规则永远显示"刚观察到"。
func TestEvidenceIntervalDefaultsToAPositiveValue(t *testing.T) {
	cfg, err := config.Load(writeYAML(t, minimalYAML))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Evidence.Interval <= 0 {
		t.Errorf("Evidence.Interval = %v, want a positive default", cfg.Evidence.Interval)
	}
}

// 显式写 0 表示关掉，且必须与"没写"分得开。
//
// 部署里已经有一个拉取式采集器在记账时，操作者要有办法把平台这一侧关掉；
// 而"没写"必须补默认值，否则升级上来的部署会静默地停止记账。
func TestEvidenceIntervalCanBeTurnedOffExplicitly(t *testing.T) {
	p := writeYAML(t, `
server:
  addr: ":10100"
auth:
  bootstrap_user:
    username: admin
    password_hash: "$2a$10$abcdefghijklmnopqrstuv"
database:
  dsn: "user:pass@tcp(127.0.0.1:3306)/distill?parseTime=true"
evidence:
  interval: 0s
`)
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Evidence.Interval != 0 {
		t.Errorf("Evidence.Interval = %v, want 0 —— 显式关掉被默认值盖住了", cfg.Evidence.Interval)
	}
}

// 负数是写错了，不是"关掉"。启动时拒绝，不要留到运行期变成一个空转的 ticker。
func TestEvidenceIntervalRejectsNegative(t *testing.T) {
	p := writeYAML(t, `
server:
  addr: ":10100"
auth:
  bootstrap_user:
    username: admin
    password_hash: "$2a$10$abcdefghijklmnopqrstuv"
database:
  dsn: "user:pass@tcp(127.0.0.1:3306)/distill?parseTime=true"
evidence:
  interval: -1m
`)
	if _, err := config.Load(p); err == nil {
		t.Fatal("负的记账周期被接受了")
	}
}
