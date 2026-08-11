package mysqlregistry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/imkerbos/Distill/internal/registry"
)

// PolicyImports 返回一个集群下未删除的导入策略，按导入时间升序。
func (s *Store) PolicyImports(ctx context.Context, clusterID string) ([]registry.PolicyImport, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT cluster_id, import_id, plane, role, source, namespace, name,
		        yaml, spec_hash, git_commit_sha, imported_by, imported_at
		   FROM policy_import
		  WHERE cluster_id = ? AND deleted_at IS NULL
		  ORDER BY imported_at, import_id`, clusterID)
	if err != nil {
		return nil, fmt.Errorf("query policy imports: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []registry.PolicyImport
	for rows.Next() {
		var p registry.PolicyImport
		var commit sql.NullString
		if err := rows.Scan(&p.ClusterID, &p.ImportID, &p.Plane, &p.Role, &p.Source,
			&p.Namespace, &p.Name, &p.YAML, &p.SpecHash, &commit,
			&p.ImportedBy, &p.ImportedAt); err != nil {
			return nil, fmt.Errorf("scan policy import: %w", err)
		}
		p.GitCommitSHA = commit.String
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate policy imports: %w", err)
	}
	return out, nil
}

// CreatePolicyImport 记录一条导入，同事务写审计。
func (s *Store) CreatePolicyImport(
	ctx context.Context, actor registry.Actor, p registry.PolicyImport,
) error {
	// 校验规则住在纯净的 internal/registry 里，本包只负责在写之前调用它：
	// 「来源为 GIT 就必须有 commit」这类判断不需要数据库就能测，而放在
	// 这里会让它只能靠一个跑着 MySQL 的测试来保证。
	if err := registry.ValidatePolicyImport(p); err != nil {
		return err
	}
	target := fmt.Sprintf("policy_import/%s/%s", p.ClusterID, p.ImportID)
	return s.mutate(ctx, actor, p.ClusterID, "CREATE_POLICY_IMPORT", target, nil, p,
		func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO policy_import
				   (cluster_id, import_id, plane, role, source, namespace, name,
				    yaml, spec_hash, git_commit_sha, imported_by, imported_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				p.ClusterID, p.ImportID, p.Plane, string(p.Role), string(p.Source),
				p.Namespace, p.Name, p.YAML, p.SpecHash,
				nullIfEmpty(p.GitCommitSHA), p.ImportedBy, p.ImportedAt,
			); err != nil {
				return writeFailure("insert policy import",
					fmt.Sprintf("导入标识 %q 在本集群下已存在", p.ImportID),
					fmt.Sprintf("集群 %s 未注册", p.ClusterID), err)
			}
			return nil
		})
}

// policyImport 按 (cluster_id, import_id) 取一条未删除的导入。
func (s *Store) policyImport(
	ctx context.Context, clusterID, importID string,
) (registry.PolicyImport, bool, error) {
	var p registry.PolicyImport
	var commit sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT cluster_id, import_id, plane, role, source, namespace, name,
		        yaml, spec_hash, git_commit_sha, imported_by, imported_at
		   FROM policy_import
		  WHERE cluster_id = ? AND import_id = ? AND deleted_at IS NULL`,
		clusterID, importID).
		Scan(&p.ClusterID, &p.ImportID, &p.Plane, &p.Role, &p.Source,
			&p.Namespace, &p.Name, &p.YAML, &p.SpecHash, &commit,
			&p.ImportedBy, &p.ImportedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return registry.PolicyImport{}, false, nil
	}
	if err != nil {
		return registry.PolicyImport{}, false, fmt.Errorf("query policy import: %w", err)
	}
	p.GitCommitSHA = commit.String
	return p, true, nil
}

// SoftDeletePolicyImport 删除一条导入，同事务写审计。
//
// 删除前先把整行读出来当作审计的 before，与 DELETE_CLUSTER 一致：
// 一条 before 与 after 都是 NULL 的审计记录只能证明「有人删过东西」，
// 而复盘时要回答的是「删掉的是哪一条策略、内容是什么」。
func (s *Store) SoftDeletePolicyImport(
	ctx context.Context, actor registry.Actor, clusterID, importID string,
) error {
	before, ok, err := s.policyImport(ctx, clusterID, importID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: policy import %s/%s", ErrNotFound, clusterID, importID)
	}
	target := fmt.Sprintf("policy_import/%s/%s", clusterID, importID)
	return s.mutate(ctx, actor, clusterID, "DELETE_POLICY_IMPORT", target, before, nil,
		func(tx *sql.Tx) error {
			res, err := tx.ExecContext(ctx,
				`UPDATE policy_import SET deleted_at = ?
				  WHERE cluster_id = ? AND import_id = ? AND deleted_at IS NULL`,
				s.now(), clusterID, importID)
			if err != nil {
				return fmt.Errorf("soft delete policy import: %w", err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return fmt.Errorf("rows affected: %w", err)
			}
			if n == 0 {
				return fmt.Errorf("%w: policy import %s/%s", ErrNotFound, clusterID, importID)
			}
			return nil
		})
}
