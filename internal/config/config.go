// Package config 加载并校验平台配置。
package config

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

// envPrefix 是环境变量前缀。DISTILL_SERVER__ADDR 覆盖 server.addr。
const envPrefix = "DISTILL_"

// ErrInvalidConfig 表示配置自身不合法。
var ErrInvalidConfig = errors.New("invalid config")

// ServerConfig 是 HTTP 服务参数。
type ServerConfig struct {
	// Addr 是监听地址。
	Addr string `koanf:"addr"`
	// ReadTimeout 限制读取请求的时长。
	ReadTimeout time.Duration `koanf:"read_timeout"`
	// WriteTimeout 限制写出响应的时长。
	WriteTimeout time.Duration `koanf:"write_timeout"`
	// ShutdownTimeout 是优雅退出的等待上限。
	ShutdownTimeout time.Duration `koanf:"shutdown_timeout"`
}

// User 是一个本地账号。
type User struct {
	// Username 是登录名。
	Username string `koanf:"username"`
	// PasswordHash 是 bcrypt 哈希。明文密码永远不进配置。
	PasswordHash string `koanf:"password_hash"`
}

// AuthConfig 是认证参数。
type AuthConfig struct {
	// SessionTTL 是会话有效期。
	SessionTTL time.Duration `koanf:"session_ttl"`
	// Users 是本地账号列表。
	Users []User `koanf:"users"`
}

// LogConfig 是日志参数。
type LogConfig struct {
	// Level 取 DEBUG / INFO / WARN / ERROR。
	Level string `koanf:"level"`
}

// DatabaseConfig 是平台自身数据库的连接参数。
//
// 平台库存的是集群注册、导入策略、覆盖决定与审计 —— 不是策略的部署事实
// 来源。部署事实来源是 Git 仓库（见 spec §1）。
type DatabaseConfig struct {
	// DSN 是 MySQL 连接串。必须带 parseTime=true 与 loc=UTC：
	// 缺前者时 DATETIME 以 []byte 读出，缺后者时驱动按本地时区解释，
	// 两者都会让时间列静默错位，而错位的审计时间在复盘时毫无价值。
	//
	// 刻意不带 multiStatements=true：那是迁移专用连接的能力（见
	// mysqlregistry.migrationDSN），这里的 DSN 供应用运行时连接池使用，
	// 开着它只会把任何一次注入的影响面从单条语句放大成任意语句链。
	DSN string `koanf:"dsn"`
	// MaxOpenConns 是连接池上限。
	MaxOpenConns int `koanf:"max_open_conns"`
	// MaxIdleConns 是空闲连接上限。
	MaxIdleConns int `koanf:"max_idle_conns"`
	// ConnMaxLifetime 是单个连接的最长存活时间。
	ConnMaxLifetime time.Duration `koanf:"conn_max_lifetime"`
}

// SecretsConfig 是凭据解析参数。
//
// 整段留空表示不解析凭据，因而也不做 Git 绑定校验 —— 结论一律记为
// NOT_VERIFIED。这是一个可以说出口的形态：没做过的检查不是通过了的检查。
type SecretsConfig struct {
	// Project 是 Secret Manager 所在的项目。
	//
	// 与 Prefix 一起选中 Secret Manager 解析器（生产形态）。不配置服务
	// 账号密钥文件路径：身份走 Workload Identity，见 v4 spec §9.8。
	Project string `koanf:"project"`
	// Prefix 是短名拼进资源路径前的固定前缀。
	//
	// 前缀写在配置里而不是让 credential_ref 自己带：短名的字符集已被
	// secrets.ValidateRef 收窄，前缀再固定一层，操作者就无法用一个引用
	// 表达出本项目之外的资源路径。
	Prefix string `koanf:"prefix"`
	// Dir 是本地开发用的凭据目录，一个 ref 一个文件。该目录必须 gitignored。
	Dir string `koanf:"dir"`
}

// GitVerifyConfig 是 Git 绑定只读校验的参数。
type GitVerifyConfig struct {
	// Timeout 是单次校验的出站超时。
	//
	// 校验挂在操作者的保存动作上，没有超时就等于界面可以被一个不响应的
	// 仓库永久挂住（spec §4）。超时归入 REPO_UNREACHABLE，不单列取值。
	Timeout time.Duration `koanf:"timeout"`
	// HostKeys 是 known_hosts 格式的已知主机公钥。
	//
	// 它不是机密，与凭据分开存放（spec §2.2），因此直接进配置文件而不走
	// Secret Manager。**必须显式配置**：没有 host key 就没有 SSH 校验，
	// 而回退到不校验等于接受任意中间人，链路终点是生产集群的策略集合。
	HostKeys string `koanf:"host_keys"`
}

// Config 是平台的完整配置。
type Config struct {
	// Server 是 HTTP 服务参数。
	Server ServerConfig `koanf:"server"`
	// Auth 是认证参数。
	Auth AuthConfig `koanf:"auth"`
	// Log 是日志参数。
	Log LogConfig `koanf:"log"`
	// Database 是平台自身数据库参数。
	Database DatabaseConfig `koanf:"database"`
	// Secrets 是凭据解析参数。留空表示不做 Git 绑定校验。
	Secrets SecretsConfig `koanf:"secrets"`
	// GitVerify 是 Git 绑定校验参数。
	GitVerify GitVerifyConfig `koanf:"gitverify"`
}

// enabled 表示这一段配置有内容，即操作者打算让平台解析凭据。
func (s SecretsConfig) enabled() bool {
	return s.Project != "" || s.Prefix != "" || s.Dir != ""
}

// SecretsBackend 是凭据解析后端的封闭取值。
type SecretsBackend string

const (
	// SecretsBackendNone 表示不解析凭据，因而也不做 Git 绑定校验。
	SecretsBackendNone SecretsBackend = ""
	// SecretsBackendDir 是本地开发用的目录解析器。
	SecretsBackendDir SecretsBackend = "dir"
	// SecretsBackendSecretManager 是生产用的 Secret Manager 解析器。
	SecretsBackendSecretManager SecretsBackend = "secretmanager"
)

// Backend 返回本段配置选中的解析后端。
//
// 后端由「配了哪些字段」决定，而不是再加一个 backend 开关：多一个开关就
// 多一种「开关写着 secretmanager、dir 也还留在文件里」的形态，而那种形态
// 最坏的落法是生产进程从本地目录读私钥。validateSecrets 保证 dir 与
// project/prefix 不会同时出现，所以这里的判断是完备的，不存在优先级。
func (s SecretsConfig) Backend() SecretsBackend {
	switch {
	case s.Dir != "":
		return SecretsBackendDir
	case s.Project != "" || s.Prefix != "":
		return SecretsBackendSecretManager
	default:
		return SecretsBackendNone
	}
}

// Load 从 YAML 文件读取配置，并允许环境变量覆盖。
//
// 校验在启动时完成而非首个请求时：配置错误应当让进程起不来，
// 而不是等到有人访问才暴露成一次线上故障。
func Load(path string) (*Config, error) {
	k := koanf.New(".")

	if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	// DISTILL_SERVER__ADDR -> server.addr
	envProvider := env.Provider(envPrefix, ".", func(s string) string {
		s = strings.TrimPrefix(s, envPrefix)
		s = strings.ReplaceAll(s, "__", ".")
		return strings.ToLower(s)
	})
	if err := k.Load(envProvider, nil); err != nil {
		return nil, fmt.Errorf("read env overrides: %w", err)
	}

	var cfg Config
	if err := k.UnmarshalWithConf("", &cfg, koanf.UnmarshalConf{Tag: "koanf"}); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}

	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// applyDefaults 填充可省略的字段。
func (c *Config) applyDefaults() {
	if c.Server.ReadTimeout == 0 {
		c.Server.ReadTimeout = 10 * time.Second
	}
	if c.Server.WriteTimeout == 0 {
		c.Server.WriteTimeout = 20 * time.Second
	}
	if c.Server.ShutdownTimeout == 0 {
		c.Server.ShutdownTimeout = 15 * time.Second
	}
	if c.Auth.SessionTTL == 0 {
		c.Auth.SessionTTL = 8 * time.Hour
	}
	if c.Log.Level == "" {
		c.Log.Level = "INFO"
	}
	// 不设上限的连接池会在故障时把 MySQL 的连接数打满，而症状表现为
	// 平台无响应，排查方向会被引向平台本身而不是数据库。
	if c.Database.MaxOpenConns == 0 {
		c.Database.MaxOpenConns = 20
	}
	if c.Database.MaxIdleConns == 0 {
		c.Database.MaxIdleConns = 5
	}
	if c.Database.ConnMaxLifetime == 0 {
		c.Database.ConnMaxLifetime = 30 * time.Minute
	}
	// 10 秒是 spec §4 写死的默认值，不是这里随手取的：校验挂在操作者的
	// 保存动作上，更长的等待在界面上与"卡住了"没有区别。省略时补这个值，
	// 但补完仍要过 validate 的正取检查 —— gitverify.New 拒绝非正超时，
	// 一个 0 值必须在启动时就被拦下，不能等到第一次校验才发现。
	if c.GitVerify.Timeout == 0 {
		c.GitVerify.Timeout = 10 * time.Second
	}
}

// validate 检查缺失即无法运行的字段。
func (c *Config) validate() error {
	if c.Server.Addr == "" {
		return fmt.Errorf("%w: server.addr is required", ErrInvalidConfig)
	}
	if len(c.Auth.Users) == 0 {
		return fmt.Errorf("%w: at least one auth.users entry is required", ErrInvalidConfig)
	}
	for i, u := range c.Auth.Users {
		if u.Username == "" || u.PasswordHash == "" {
			return fmt.Errorf("%w: auth.users[%d] needs both username and password_hash", ErrInvalidConfig, i)
		}
	}
	if c.Database.DSN == "" {
		return fmt.Errorf("%w: database.dsn is required", ErrInvalidConfig)
	}
	return c.validateVerification()
}

// validateVerification 检查凭据解析与校验两段配置是否自洽。
//
// 三条约束都指向同一件事：一个能起来、但每一次校验都注定失败的进程，
// 比一个起不来的进程更糟 —— 前者会在界面上持续产出一串没人能解释的
// 结论，而运维看到的是服务健康。
//
//   - 配了 secrets 就必须配 host keys：gitverify.New 没有 host key 直接
//     构造失败，且不存在"未配置就不校验"的回退分支。
//   - 后端必须选得出来且只选中一个，见 validateSecrets。
//   - 超时必须为正：非正值会被 gitverify.New 拒绝，那是启动失败，
//     但报错点应该在配置这一层，说得出是哪个字段。
func (c *Config) validateVerification() error {
	if !c.Secrets.enabled() {
		return nil
	}
	if err := validateSecrets(c.Secrets); err != nil {
		return err
	}
	if c.GitVerify.HostKeys == "" {
		return fmt.Errorf("%w: gitverify.host_keys is required when secrets is configured", ErrInvalidConfig)
	}
	if c.GitVerify.Timeout <= 0 {
		return fmt.Errorf("%w: gitverify.timeout must be positive", ErrInvalidConfig)
	}
	return nil
}

// validateSecrets 检查这一段恰好选中一个解析后端。
//
// 两条约束的方向是同一个：不让一份配置同时是两种意思。
//
//   - dir 与 project/prefix 互斥。同时出现时无论按哪一边解释都有一半配置
//     是死的，而写下它的人不会知道是哪一半 —— 最坏的一种落法是生产进程
//     去读本地目录里的私钥。这里不定优先级，直接拒。
//   - Secret Manager 后端要求 project 与 prefix 都在。空 prefix 一样能拼出
//     合法路径，但围栏就只剩项目一层，任何短名都能指向项目里的任意
//     secret，这与 spec §2.1 的意图相反。
func validateSecrets(s SecretsConfig) error {
	if s.Dir != "" && (s.Project != "" || s.Prefix != "") {
		return fmt.Errorf("%w: secrets.dir cannot be combined with secrets.project/secrets.prefix — configure exactly one backend", ErrInvalidConfig)
	}
	if s.Backend() == SecretsBackendSecretManager && (s.Project == "" || s.Prefix == "") {
		return fmt.Errorf("%w: secrets.project and secrets.prefix are both required for the Secret Manager backend", ErrInvalidConfig)
	}
	return nil
}
