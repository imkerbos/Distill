package mysqlregistry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/imkerbos/Distill/internal/registry"
)

// Clusters 返回全部未删除的集群，按 ID 升序。
func (s *Store) Clusters(ctx context.Context) ([]registry.Cluster, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT cluster_id, display_name, pod_cidr, node_cidr, ccnp_present, onboard_state
		   FROM cluster WHERE deleted_at IS NULL ORDER BY cluster_id`)
	if err != nil {
		return nil, fmt.Errorf("query clusters: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []registry.Cluster
	for rows.Next() {
		var c registry.Cluster
		if err := rows.Scan(&c.ID, &c.DisplayName, &c.PodCIDR, &c.NodeCIDR,
			&c.CCNPPresent, &c.State); err != nil {
			return nil, fmt.Errorf("scan cluster: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate clusters: %w", err)
	}
	for i := range out {
		if err := s.loadChildren(ctx, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Cluster 按 ID 查一个未删除的集群。
func (s *Store) Cluster(ctx context.Context, id string) (registry.Cluster, bool, error) {
	var c registry.Cluster
	err := s.db.QueryRowContext(ctx,
		`SELECT cluster_id, display_name, pod_cidr, node_cidr, ccnp_present, onboard_state
		   FROM cluster WHERE cluster_id = ? AND deleted_at IS NULL`, id).
		Scan(&c.ID, &c.DisplayName, &c.PodCIDR, &c.NodeCIDR, &c.CCNPPresent, &c.State)
	if errors.Is(err, sql.ErrNoRows) {
		return registry.Cluster{}, false, nil
	}
	if err != nil {
		return registry.Cluster{}, false, fmt.Errorf("query cluster: %w", err)
	}
	if err := s.loadChildren(ctx, &c); err != nil {
		return registry.Cluster{}, false, err
	}
	return c, true, nil
}

// loadChildren 填充端点、健康检查网段与 Git 绑定。
func (s *Store) loadChildren(ctx context.Context, c *registry.Cluster) error {
	apiRows, err := s.db.QueryContext(ctx,
		`SELECT host, cidr, port FROM cluster_apiserver WHERE cluster_id = ? ORDER BY host`, c.ID)
	if err != nil {
		return fmt.Errorf("query apiservers: %w", err)
	}
	defer func() { _ = apiRows.Close() }()
	for apiRows.Next() {
		var a registry.APIServer
		if err := apiRows.Scan(&a.Host, &a.CIDR, &a.Port); err != nil {
			return fmt.Errorf("scan apiserver: %w", err)
		}
		c.APIServers = append(c.APIServers, a)
	}
	if err := apiRows.Err(); err != nil {
		return fmt.Errorf("iterate apiservers: %w", err)
	}

	hcRows, err := s.db.QueryContext(ctx,
		`SELECT cidr FROM cluster_health_check_source WHERE cluster_id = ? ORDER BY cidr`, c.ID)
	if err != nil {
		return fmt.Errorf("query health check sources: %w", err)
	}
	defer func() { _ = hcRows.Close() }()
	for hcRows.Next() {
		var cidr string
		if err := hcRows.Scan(&cidr); err != nil {
			return fmt.Errorf("scan health check source: %w", err)
		}
		c.HealthCheckSources = append(c.HealthCheckSources, cidr)
	}
	if err := hcRows.Err(); err != nil {
		return fmt.Errorf("iterate health check sources: %w", err)
	}

	var g registry.GitBinding
	var credRef, lastCommit sql.NullString
	err = s.db.QueryRowContext(ctx,
		`SELECT repo_url, branch, policy_path, credential_ref, last_written_commit
		   FROM cluster_git_binding WHERE cluster_id = ?`, c.ID).
		Scan(&g.RepoURL, &g.Branch, &g.PolicyPath, &credRef, &lastCommit)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("query git binding: %w", err)
	}
	g.CredentialRef = credRef.String
	g.LastWrittenCommit = lastCommit.String
	c.Git = &g
	return nil
}

// CreateCluster 注册一个集群，同事务写审计。
func (s *Store) CreateCluster(ctx context.Context, actor registry.Actor, c registry.Cluster) error {
	if err := registry.ValidateCluster(c); err != nil {
		return err
	}
	return s.mutate(ctx, actor, c.ID, "CREATE_CLUSTER", "cluster/"+c.ID, nil, c,
		func(tx *sql.Tx) error {
			now := s.now()
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO cluster
				   (cluster_id, display_name, pod_cidr, node_cidr, ccnp_present,
				    onboard_state, created_at, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				c.ID, c.DisplayName, c.PodCIDR, c.NodeCIDR, c.CCNPPresent,
				string(c.State), now, now,
			); err != nil {
				return fmt.Errorf("insert cluster: %w", err)
			}
			return insertChildren(ctx, tx, c)
		})
}

// UpdateCluster 修改集群，同事务写审计。
//
// 子表整体重写而非逐条 diff：网段清单是一个整体，
// 逐条 diff 会在漏删一条时留下一个没人知道来源的旧网段。
func (s *Store) UpdateCluster(ctx context.Context, actor registry.Actor, c registry.Cluster) error {
	if err := registry.ValidateCluster(c); err != nil {
		return err
	}
	before, ok, err := s.Cluster(ctx, c.ID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: cluster %s", ErrNotFound, c.ID)
	}
	return s.mutate(ctx, actor, c.ID, "UPDATE_CLUSTER", "cluster/"+c.ID, before, c,
		func(tx *sql.Tx) error {
			res, err := tx.ExecContext(ctx,
				`UPDATE cluster SET display_name = ?, pod_cidr = ?, node_cidr = ?,
				        ccnp_present = ?, onboard_state = ?, updated_at = ?
				  WHERE cluster_id = ? AND deleted_at IS NULL`,
				c.DisplayName, c.PodCIDR, c.NodeCIDR, c.CCNPPresent,
				string(c.State), s.now(), c.ID,
			)
			if err != nil {
				return fmt.Errorf("update cluster: %w", err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return fmt.Errorf("rows affected: %w", err)
			}
			if n == 0 {
				// 事务外的存在性检查挡不住并发删除：只有事务内的行数
				// 能证明这次写真的落在了一行上。返回 ErrNotFound 会让
				// mutate 连审计一起回滚 —— 否则审计里会留下一条描述
				// 从未发生过的操作的记录。
				return fmt.Errorf("%w: cluster %s", ErrNotFound, c.ID)
			}
			for _, stmt := range []string{
				`DELETE FROM cluster_apiserver WHERE cluster_id = ?`,
				`DELETE FROM cluster_health_check_source WHERE cluster_id = ?`,
				`DELETE FROM cluster_git_binding WHERE cluster_id = ?`,
			} {
				if _, err := tx.ExecContext(ctx, stmt, c.ID); err != nil {
					return fmt.Errorf("clear children: %w", err)
				}
			}
			return insertChildren(ctx, tx, c)
		})
}

// SoftDeleteCluster 下线集群，同事务写审计。
func (s *Store) SoftDeleteCluster(ctx context.Context, actor registry.Actor, id string) error {
	before, ok, err := s.Cluster(ctx, id)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: cluster %s", ErrNotFound, id)
	}
	return s.mutate(ctx, actor, id, "DELETE_CLUSTER", "cluster/"+id, before, nil,
		func(tx *sql.Tx) error {
			res, err := tx.ExecContext(ctx,
				`UPDATE cluster SET deleted_at = ?, updated_at = ?
				  WHERE cluster_id = ? AND deleted_at IS NULL`,
				s.now(), s.now(), id,
			)
			if err != nil {
				return fmt.Errorf("soft delete cluster: %w", err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return fmt.Errorf("rows affected: %w", err)
			}
			if n == 0 {
				// 同上：并发删除会让这条 UPDATE 匹配不到行，
				// 事务外的存在性检查看不到这一刻的状态。
				return fmt.Errorf("%w: cluster %s", ErrNotFound, id)
			}
			return nil
		})
}

// insertChildren 写入端点、健康检查网段与 Git 绑定。
func insertChildren(ctx context.Context, tx *sql.Tx, c registry.Cluster) error {
	for _, a := range c.APIServers {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO cluster_apiserver (cluster_id, host, cidr, port) VALUES (?, ?, ?, ?)`,
			c.ID, a.Host, a.CIDR, a.Port,
		); err != nil {
			return fmt.Errorf("insert apiserver: %w", err)
		}
	}
	for _, cidr := range c.HealthCheckSources {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO cluster_health_check_source (cluster_id, cidr) VALUES (?, ?)`,
			c.ID, cidr,
		); err != nil {
			return fmt.Errorf("insert health check source: %w", err)
		}
	}
	if c.Git != nil {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO cluster_git_binding
			   (cluster_id, repo_url, branch, policy_path, credential_ref, last_written_commit)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			c.ID, c.Git.RepoURL, c.Git.Branch, c.Git.PolicyPath,
			nullIfEmpty(c.Git.CredentialRef), nullIfEmpty(c.Git.LastWrittenCommit),
		); err != nil {
			return fmt.Errorf("insert git binding: %w", err)
		}
	}
	return nil
}

// nullIfEmpty 把空串转成 SQL NULL。
//
// 空串与 NULL 在这里语义不同：NULL 表示「没有绑定凭据」，
// 空串会让「已配置但值为空」与「未配置」无法区分。
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
