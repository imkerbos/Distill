package mysqlregistry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/imkerbos/Distill/internal/registry"
)

// RuleOverrides 返回一个集群下未删除的人工决定，按 namespace/workload/指纹排序。
func (s *Store) RuleOverrides(
	ctx context.Context, clusterID string,
) ([]registry.RuleOverride, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT cluster_id, namespace, workload, rule_fingerprint, decision,
		        reason, decided_by, decided_at, merged_commit_sha
		   FROM rule_override
		  WHERE cluster_id = ? AND deleted_at IS NULL
		  ORDER BY namespace, workload, rule_fingerprint`, clusterID)
	if err != nil {
		return nil, fmt.Errorf("query rule overrides: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []registry.RuleOverride
	for rows.Next() {
		var o registry.RuleOverride
		var merged sql.NullString
		if err := rows.Scan(&o.ClusterID, &o.Namespace, &o.Workload, &o.Fingerprint,
			&o.Decision, &o.Reason, &o.DecidedBy, &o.DecidedAt, &merged); err != nil {
			return nil, fmt.Errorf("scan rule override: %w", err)
		}
		o.MergedCommitSHA = merged.String
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate rule overrides: %w", err)
	}
	return out, nil
}

// ruleOverride 按主键取一条未删除的人工决定。
func (s *Store) ruleOverride(
	ctx context.Context, clusterID, namespace, workload, fingerprint string,
) (registry.RuleOverride, bool, error) {
	var o registry.RuleOverride
	var merged sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT cluster_id, namespace, workload, rule_fingerprint, decision,
		        reason, decided_by, decided_at, merged_commit_sha
		   FROM rule_override
		  WHERE cluster_id = ? AND namespace = ? AND workload = ?
		    AND rule_fingerprint = ? AND deleted_at IS NULL`,
		clusterID, namespace, workload, fingerprint).
		Scan(&o.ClusterID, &o.Namespace, &o.Workload, &o.Fingerprint,
			&o.Decision, &o.Reason, &o.DecidedBy, &o.DecidedAt, &merged)
	if errors.Is(err, sql.ErrNoRows) {
		return registry.RuleOverride{}, false, nil
	}
	if err != nil {
		return registry.RuleOverride{}, false, fmt.Errorf("query rule override: %w", err)
	}
	o.MergedCommitSHA = merged.String
	return o, true, nil
}

// CreateRuleOverride 记录一条人工决定，同事务写审计。
//
// 重复决定覆盖旧值而非报冲突：人改主意是正常的，而两条互相矛盾的
// 决定并存会让「这条规则到底开不开」没有答案。旧值作为审计的 before
// 留下，所以改主意这件事本身有记录。
func (s *Store) CreateRuleOverride(
	ctx context.Context, actor registry.Actor, o registry.RuleOverride,
) error {
	if err := registry.ValidateOverride(o); err != nil {
		return err
	}
	existing, ok, err := s.ruleOverride(ctx, o.ClusterID, o.Namespace, o.Workload, o.Fingerprint)
	if err != nil {
		return err
	}
	var before any
	if ok {
		before = existing
	}
	target := fmt.Sprintf("rule_override/%s/%s/%s/%s",
		o.ClusterID, o.Namespace, o.Workload, o.Fingerprint)
	return s.mutate(ctx, actor, o.ClusterID, "CREATE_RULE_OVERRIDE", target, before, o,
		func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO rule_override
				   (cluster_id, namespace, workload, rule_fingerprint, decision,
				    reason, decided_by, decided_at, merged_commit_sha, deleted_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)
				 ON DUPLICATE KEY UPDATE
				   decision = VALUES(decision), reason = VALUES(reason),
				   decided_by = VALUES(decided_by), decided_at = VALUES(decided_at),
				   merged_commit_sha = VALUES(merged_commit_sha), deleted_at = NULL`,
				o.ClusterID, o.Namespace, o.Workload, o.Fingerprint, string(o.Decision),
				o.Reason, o.DecidedBy, o.DecidedAt, nullIfEmpty(o.MergedCommitSHA),
			); err != nil {
				return writeFailure("insert rule override",
					"", fmt.Sprintf("集群 %s 未注册", o.ClusterID), err)
			}
			return nil
		})
}

// SoftDeleteRuleOverride 撤销一条人工决定，同事务写审计。
func (s *Store) SoftDeleteRuleOverride(
	ctx context.Context, actor registry.Actor,
	clusterID, namespace, workload, fingerprint string,
) error {
	before, ok, err := s.ruleOverride(ctx, clusterID, namespace, workload, fingerprint)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: rule override %s/%s/%s/%s",
			ErrNotFound, clusterID, namespace, workload, fingerprint)
	}
	target := fmt.Sprintf("rule_override/%s/%s/%s/%s", clusterID, namespace, workload, fingerprint)
	return s.mutate(ctx, actor, clusterID, "DELETE_RULE_OVERRIDE", target, before, nil,
		func(tx *sql.Tx) error {
			res, err := tx.ExecContext(ctx,
				`UPDATE rule_override SET deleted_at = ?
				  WHERE cluster_id = ? AND namespace = ? AND workload = ?
				    AND rule_fingerprint = ? AND deleted_at IS NULL`,
				s.now(), clusterID, namespace, workload, fingerprint)
			if err != nil {
				return fmt.Errorf("soft delete rule override: %w", err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return fmt.Errorf("rows affected: %w", err)
			}
			if n == 0 {
				// 事务外的存在性检查挡不住并发删除：只有事务内的行数
				// 能证明这次写真的落在了一行上。返回 ErrNotFound 让
				// mutate 连审计一起回滚 —— 否则审计里会留下一条描述
				// 从未发生过的撤销的记录。
				return fmt.Errorf("%w: rule override %s/%s/%s/%s",
					ErrNotFound, clusterID, namespace, workload, fingerprint)
			}
			return nil
		})
}
