// Package config 加载并校验平台配置。
//
// 文件里只留启动必需项 —— 没有它就连不上数据库或起不来进程的那些
// （design doc 2026-08-13 §1）。运行期配置（会话 TTL、HTTP 超时、凭据
// 后端、Git 校验超时与 SSH host keys）在 platform_setting 表里，从设置页
// 改，由 internal/settings 在使用处按需读取。
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
//
// 只剩监听地址：读写与优雅退出超时已经迁进 platform_setting 表。
type ServerConfig struct {
	// Addr 是监听地址。
	Addr string `koanf:"addr"`
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
	// BootstrapUser 是引导账号，**只**为让首次登录成为可能而存在。
	//
	// 账号本该落库，但那样会成环：首次登录需要先有账号，而建账号需要
	// 先登录。拆掉这个环要引入独立的引导流程，属于另一件事
	// （design doc §1.2）。字段名说出这件事，免得它被当成常规的账号来源。
	BootstrapUser User `koanf:"bootstrap_user"`
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

// Config 是平台的完整配置。
//
// 数据库连接参数留在文件里而设置在库里，这不是矛盾：读设置本身就要先
// 连上库，把 DSN 也挪进去就成环了。
type Config struct {
	// Server 是 HTTP 服务参数。
	Server ServerConfig `koanf:"server"`
	// Auth 是认证参数。
	Auth AuthConfig `koanf:"auth"`
	// Log 是日志参数。
	Log LogConfig `koanf:"log"`
	// Database 是平台自身数据库参数。
	Database DatabaseConfig `koanf:"database"`
}

// relocatedKey 是一个不再属于配置文件的键，以及它的去向。
type relocatedKey struct {
	// key 是配置里的完整键名。
	key string
	// where 说明它去了哪里，直接拼进报错。
	where string
}

// relocatedKeys 是全部已迁走或改名的键。
//
// **出现即拒，不做兼容读取。** 静默忽略是这里最坏的落法：操作者改一个
// 超时、重启、观察到毫无变化，而平台没有发出任何信号 —— 把这些键挪进
// 数据库正是为了终结「改了要重启」，一个被忽略的键会让它变成「改了永远
// 不生效」。也不做「读文件的值去写库」那种迁移：那等于每次重启都用文件
// 覆盖操作者在页面上的修改。
//
// 用切片而非 map：报错要稳定指向同一个键，map 的遍历顺序会让同一份配置
// 在两次启动里报出不同的键名，而操作者会以为自己改错了地方。
var relocatedKeys = []relocatedKey{
	{"server.read_timeout", "the platform_setting table as httpReadTimeout; change it on the settings page"},
	{"server.write_timeout", "the platform_setting table as httpWriteTimeout; change it on the settings page"},
	{"server.shutdown_timeout", "the platform_setting table as httpShutdownTimeout; change it on the settings page"},
	{"auth.session_ttl", "the platform_setting table as sessionTtl; change it on the settings page"},
	{"auth.users", "auth.bootstrap_user in this same file; it now holds exactly one account, and it exists only to make the first login possible"},
	{"secrets.project", "the platform_setting table as secretsProject; change it on the settings page"},
	{"secrets.prefix", "the platform_setting table as secretsPrefix; change it on the settings page"},
	{"secrets.dir", "the platform_setting table as secretsDir; change it on the settings page"},
	{"gitverify.timeout", "the platform_setting table as gitVerifyTimeout; change it on the settings page"},
	{"gitverify.host_keys", "the platform_setting table as gitVerifyHostKeys; change it on the settings page"},
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

	// 检测放在 Unmarshal 之前：一份旧配置解出来的结构体里，迁走的键根本
	// 没有落脚点，validate 只会抱怨「bootstrap_user 缺失」—— 一句正确但
	// 没用的话，因为文件里明明写着账号。
	if err := checkRelocatedKeys(k); err != nil {
		return nil, err
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

// checkRelocatedKeys 拒绝任何已迁走或改名的键。
//
// 查的是合并后的 koanf 实例，因此环境变量里的同名键一样会被拦下 ——
// 容器里改配置走的正是环境变量，只查文件等于给它留了一条静默失效的路。
func checkRelocatedKeys(k *koanf.Koanf) error {
	for _, rk := range relocatedKeys {
		if k.Exists(rk.key) {
			return fmt.Errorf("%w: %s no longer belongs in the config file — it moved to %s",
				ErrInvalidConfig, rk.key, rk.where)
		}
	}
	return nil
}

// applyDefaults 填充可省略的字段。
//
// 只剩留在文件里的那几项。运行期配置的默认值不在这里 —— 它们由迁移的
// 种子行给出，因为设置表不能为空：那些字段的零值不是「用默认」，而是
// 关掉超时保护、会话立即过期。
func (c *Config) applyDefaults() {
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
}

// validate 检查缺失即无法运行的字段。
func (c *Config) validate() error {
	if c.Server.Addr == "" {
		return fmt.Errorf("%w: server.addr is required", ErrInvalidConfig)
	}
	// 没有引导账号就没有第一次登录，而建账号需要先登录（design doc §1.2）。
	if c.Auth.BootstrapUser.Username == "" || c.Auth.BootstrapUser.PasswordHash == "" {
		return fmt.Errorf("%w: auth.bootstrap_user needs both username and password_hash",
			ErrInvalidConfig)
	}
	if c.Database.DSN == "" {
		return fmt.Errorf("%w: database.dsn is required", ErrInvalidConfig)
	}
	return nil
}
