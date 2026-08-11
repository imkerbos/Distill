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
	return nil
}
