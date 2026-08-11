package mysqlregistry

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/imkerbos/Distill/internal/registry"
)

// ErrNotFound 表示目标行不存在，包装 registry.ErrNotFound 以便
// 边界层用 errors.Is 识别，而不必 import 本包。
var ErrNotFound = fmt.Errorf("%w: row missing", registry.ErrNotFound)

// Store 是 registry.Store 的 MySQL 实现。
type Store struct {
	db *sql.DB
	// now 可在测试中替换，生产恒为 time.Now。
	now func() time.Time
}

// New 构造 Store。
func New(db *sql.DB) *Store {
	return &Store{db: db, now: func() time.Time { return time.Now().UTC() }}
}

// mutate 在单个事务内完成业务写入与审计写入。
//
// 业务写入失败时审计行随之回滚：一条留下来的审计行会记录一件
// 从未发生过的事，而复盘时没有比这更糟的输入。
func (s *Store) mutate(
	ctx context.Context,
	actor registry.Actor,
	clusterID, action, target string,
	before, after any,
	fn func(*sql.Tx) error,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := fn(tx); err != nil {
		return err
	}

	beforeJSON, err := marshalOrNil(before)
	if err != nil {
		return err
	}
	afterJSON, err := marshalOrNil(after)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO audit_log (cluster_id, actor, action, target, before_val, after_val, at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		clusterID, actor.Username, action, target, beforeJSON, afterJSON, s.now(),
	); err != nil {
		return fmt.Errorf("write audit: %w", err)
	}
	return tx.Commit()
}

// marshalOrNil 把值序列化为 JSON；nil 保持为 SQL NULL。
func marshalOrNil(v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal audit payload: %w", err)
	}
	return string(b), nil
}
