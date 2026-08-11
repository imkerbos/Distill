package mysqlregistry

import (
	"context"
	"database/sql"
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
	if !p.Role.Valid() {
		return fmt.Errorf("%w: unregistered import role %q", registry.ErrInvalid, p.Role)
	}
	if !p.Source.Valid() {
		return fmt.Errorf("%w: unregistered import source %q", registry.ErrInvalid, p.Source)
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
				return fmt.Errorf("insert policy import: %w", err)
			}
			return nil
		})
}

// SoftDeletePolicyImport 删除一条导入，同事务写审计。
func (s *Store) SoftDeletePolicyImport(
	ctx context.Context, actor registry.Actor, clusterID, importID string,
) error {
	target := fmt.Sprintf("policy_import/%s/%s", clusterID, importID)
	return s.mutate(ctx, actor, clusterID, "DELETE_POLICY_IMPORT", target, nil, nil,
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
