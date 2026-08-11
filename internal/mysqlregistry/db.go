// Package mysqlregistry 是 registry 的 MySQL 实现。
//
// database/sql 只出现在本包：internal/registry 保持纯净，
// 才能让集群注册的类型与校验逻辑在没有数据库的情况下被完整测试。
package mysqlregistry

import (
	"database/sql"
	"errors"
	"fmt"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/golang-migrate/migrate/v4"
	migratemysql "github.com/golang-migrate/migrate/v4/database/mysql"
	_ "github.com/golang-migrate/migrate/v4/source/file" // file:// 源

	"github.com/imkerbos/Distill/internal/config"
)

// Open 按配置建立应用运行时的连接池。
//
// 连接池上限由调用方给出而非在此取默认值：合适的上限取决于部署形态，
// 而一个没有上限的池会在故障时把 MySQL 的连接数打满。
//
// 这个连接池不开 multiStatements —— 那是迁移专用连接的能力（见
// migrationDSN），运行时连接池开着它只会白白放大注入的影响面。
func Open(cfg config.DatabaseConfig) (*sql.DB, error) {
	db, err := sql.Open("mysql", cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}
	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxIdleConns)
	db.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping mysql: %w", err)
	}
	return db, nil
}

// migrationDSN 在给定 DSN 上打开 multiStatements。
//
// 只有迁移需要它：golang-migrate 的 mysql 驱动把整个迁移文件作为
// 一条查询执行。应用运行时的连接池不开这个开关 —— 允许一次发送多条
// 语句会把任何一次注入的影响面从单条语句放大成任意语句链，
// 而这个库里存着审计记录。
//
// 用驱动自带的解析器而非拼接查询参数：DSN 可能已经带了参数，
// 手工拼 & 是畸形 DSN 被发布出去的典型路径。
func migrationDSN(dsn string) (string, error) {
	cfg, err := mysqldriver.ParseDSN(dsn)
	if err != nil {
		return "", fmt.Errorf("parse dsn: %w", err)
	}
	cfg.MultiStatements = true
	return cfg.FormatDSN(), nil
}

// migrator 打开一个迁移专用的短生命周期连接并构造迁移器。
//
// 这个连接与 Open 返回的应用连接池完全分开：它需要 multiStatements，
// 应用连接池不需要也不应该有。返回的 *migrate.Migrate.Close() 会
// 一并关闭这个连接，调用方无需另行处理。
func migrator(cfg config.DatabaseConfig, dir string) (*migrate.Migrate, error) {
	dsn, err := migrationDSN(cfg.DSN)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open mysql for migration: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping mysql for migration: %w", err)
	}
	driver, err := migratemysql.WithInstance(db, &migratemysql.Config{})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate driver: %w", err)
	}
	m, err := migrate.NewWithDatabaseInstance("file://"+dir, "mysql", driver)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate init: %w", err)
	}
	return m, nil
}

// Migrate 把 schema 升到最新版本。已是最新时不报错。
//
// 迁移用的连接在函数返回前关闭，不会泄漏进调用方的连接池。
func Migrate(cfg config.DatabaseConfig, dir string) error {
	m, err := migrator(cfg, dir)
	if err != nil {
		return err
	}
	defer func() { _, _ = m.Close() }()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate up: %w", err)
	}
	return nil
}

// Rollback 把 schema 全部回滚。
//
// 导出它是为了让测试能验证 down 脚本真的能跑 —— 一个从未被执行过的
// 回滚脚本，在需要它的那天才第一次运行，等于没有回滚。
func Rollback(cfg config.DatabaseConfig, dir string) error {
	m, err := migrator(cfg, dir)
	if err != nil {
		return err
	}
	defer func() { _, _ = m.Close() }()
	if err := m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate down: %w", err)
	}
	return nil
}
