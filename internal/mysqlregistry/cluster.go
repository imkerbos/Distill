package mysqlregistry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/imkerbos/Distill/internal/registry"
)

// Clusters 返回全部未删除的集群，按 ID 升序。
func (s *Store) Clusters(ctx context.Context) ([]registry.Cluster, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT cluster_id, display_name, pod_cidr, node_cidr, ccnp_present, other_planes,
		        business_cycle_seconds, business_cycle_reason,
		        onboard_state, kubeconfig_ref, data_source, no_node_agents_reason
		   FROM cluster WHERE deleted_at IS NULL ORDER BY cluster_id`)
	if err != nil {
		return nil, fmt.Errorf("query clusters: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []registry.Cluster
	for rows.Next() {
		var c registry.Cluster
		var cycleSeconds uint32
		if err := rows.Scan(&c.ID, &c.DisplayName, &c.PodCIDR, &c.NodeCIDR,
			&c.CCNPPresent, &c.OtherPlanes, &cycleSeconds, &c.BusinessCycleReason,
			&c.State, &c.KubeconfigRef, &c.DataSource,
			&c.NoNodeAgentsReason); err != nil {
			return nil, fmt.Errorf("scan cluster: %w", err)
		}
		c.BusinessCycle = time.Duration(cycleSeconds) * time.Second
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
	var (
		c            registry.Cluster
		cycleSeconds uint32
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT cluster_id, display_name, pod_cidr, node_cidr, ccnp_present, other_planes,
		        business_cycle_seconds, business_cycle_reason,
		        onboard_state, kubeconfig_ref, data_source, no_node_agents_reason
		   FROM cluster WHERE cluster_id = ? AND deleted_at IS NULL`, id).
		Scan(&c.ID, &c.DisplayName, &c.PodCIDR, &c.NodeCIDR, &c.CCNPPresent, &c.OtherPlanes,
			&cycleSeconds, &c.BusinessCycleReason,
			&c.State, &c.KubeconfigRef, &c.DataSource, &c.NoNodeAgentsReason)
	if errors.Is(err, sql.ErrNoRows) {
		return registry.Cluster{}, false, nil
	}
	if err != nil {
		return registry.Cluster{}, false, fmt.Errorf("query cluster: %w", err)
	}
	c.BusinessCycle = time.Duration(cycleSeconds) * time.Second
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

	// metrics 抓取端：METRICS_SCRAPE Baseline 依据的一半，观测不出来
	// （design doc 2026-08-18-metrics-scrape-evidence §3.2）。
	msRows, err := s.db.QueryContext(ctx,
		`SELECT namespace, labels FROM cluster_metrics_scraper
		  WHERE cluster_id = ? ORDER BY namespace, labels_key`, c.ID)
	if err != nil {
		return fmt.Errorf("query metrics scrapers: %w", err)
	}
	defer func() { _ = msRows.Close() }()
	for msRows.Next() {
		var (
			sc  registry.MetricsScraper
			raw []byte
		)
		if err := msRows.Scan(&sc.Namespace, &raw); err != nil {
			return fmt.Errorf("scan metrics scraper: %w", err)
		}
		if err := json.Unmarshal(raw, &sc.Labels); err != nil {
			// 标签列坏了就是坏了，不当成"这个抓取端没有标签"：后者会生成
			// 一个空 podSelector，放行那个命名空间里的每一个 Pod。
			return fmt.Errorf("decode metrics scraper labels: %w", err)
		}
		c.MetricsScrapers = append(c.MetricsScrapers, sc)
	}
	if err := msRows.Err(); err != nil {
		return fmt.Errorf("iterate metrics scrapers: %w", err)
	}

	// 节点级 agent：与 metrics 抓取端同源，端口只有人知道
	// （design doc 2026-08-18-node-agent-applicability §3）。
	naRows, err := s.db.QueryContext(ctx,
		`SELECT namespace, app, host_network, target_port FROM cluster_node_agent
		  WHERE cluster_id = ? ORDER BY namespace, app`, c.ID)
	if err != nil {
		return fmt.Errorf("query node agents: %w", err)
	}
	defer func() { _ = naRows.Close() }()
	for naRows.Next() {
		var a registry.NodeAgentRegistration
		if err := naRows.Scan(&a.Namespace, &a.App, &a.HostNetwork, &a.TargetPort); err != nil {
			return fmt.Errorf("scan node agent: %w", err)
		}
		c.NodeAgents = append(c.NodeAgents, a)
	}
	if err := naRows.Err(); err != nil {
		return fmt.Errorf("iterate node agents: %w", err)
	}

	var g registry.GitBinding
	var lastCommit, verifyResult sql.NullString
	var verifiedAt sql.NullTime
	err = s.db.QueryRowContext(ctx,
		`SELECT repo_id, policy_path, last_written_commit, verified_at, verify_result
		   FROM cluster_git_binding WHERE cluster_id = ?`, c.ID).
		Scan(&g.RepoID, &g.PolicyPath, &lastCommit, &verifiedAt, &verifyResult)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("query git binding: %w", err)
	}
	g.LastWrittenCommit = lastCommit.String
	// NULL 必须落成 nil，不能落成零值 time.Time：零值是一个真实存在的
	// 过去时间点（1970 年），任何新鲜度检查都会把它当成「校验过」而非
	// 「从未校验」放行。
	if verifiedAt.Valid {
		t := verifiedAt.Time
		g.VerifiedAt = &t
	}
	g.VerifyResult = registry.BindingVerifyResult(verifyResult.String)
	c.Git = &g
	return nil
}

// CreateCluster 注册一个集群，同事务写审计。
//
// **不写 data_source。** 新注册的集群一律落在列默认值 COLLECTED 上：通过这条
// 路径登记的集群按定义是一个真集群，而 FIXTURE 只属于 000002 种下的两个演示
// 集群（design doc 2026-08-17 §2、§6）。让调用方指定来源，等于给出一条把真
// 集群标成演示集群的入口，而那正是本轮要防的那个最坏结果的手动版本。
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
				    business_cycle_seconds, business_cycle_reason,
				    no_node_agents_reason, onboard_state, kubeconfig_ref,
				    created_at, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				c.ID, c.DisplayName, c.PodCIDR, c.NodeCIDR, c.CCNPPresent,
				uint32(c.BusinessCycle.Seconds()), c.BusinessCycleReason,
				c.NoNodeAgentsReason, string(c.State), c.KubeconfigRef, now, now,
			); err != nil {
				return writeFailure("insert cluster",
					fmt.Sprintf("集群 ID %q 已被注册，请换一个或先下线原集群", c.ID), "", err)
			}
			return insertChildren(ctx, tx, c)
		})
}

// UpdateCluster 修改集群，同事务写审计。
//
// 子表整体重写而非逐条 diff：网段清单是一个整体，
// 逐条 diff 会在漏删一条时留下一个没人知道来源的旧网段。
//
// **data_source 不在 SET 清单里**，理由同 CreateCluster：改一次网段顺手把
// 数据来源换掉，是这个字段一旦进了写路径就必然会发生的事。c.DataSource 上
// 即便带着值也不落库 —— 与 c.Git 同一条纪律。
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
				        ccnp_present = ?, business_cycle_seconds = ?, business_cycle_reason = ?,
				        no_node_agents_reason = ?,
				        onboard_state = ?, kubeconfig_ref = ?, updated_at = ?
				  WHERE cluster_id = ? AND deleted_at IS NULL`,
				c.DisplayName, c.PodCIDR, c.NodeCIDR, c.CCNPPresent,
				uint32(c.BusinessCycle.Seconds()), c.BusinessCycleReason,
				c.NoNodeAgentsReason, string(c.State), c.KubeconfigRef, s.now(), c.ID,
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
			// 绑定不在清单里：它有自己的生命周期与自己的写入
			// （design doc 2026-08-13 §2）。改一次网段顺手解绑，
			// 是嵌在集群写模型里时才会发生的事。
			for _, stmt := range []string{
				`DELETE FROM cluster_apiserver WHERE cluster_id = ?`,
				`DELETE FROM cluster_health_check_source WHERE cluster_id = ?`,
				`DELETE FROM cluster_metrics_scraper WHERE cluster_id = ?`,
				`DELETE FROM cluster_node_agent WHERE cluster_id = ?`,
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

// insertChildren 写入端点与健康检查网段。
//
// 不含 Git 绑定：绑定是被绑定的资源，由 BindGitRepo 写入并写自己的审计行
// （design doc 2026-08-13 §2、§4）。集群对象上即便带着 Git 也不落库 ——
// 一条能绕开 BIND_GIT_REPO 的绑定写路径，会让审计答不出仓库地址是谁改的。
func insertChildren(ctx context.Context, tx *sql.Tx, c registry.Cluster) error {
	for _, a := range c.APIServers {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO cluster_apiserver (cluster_id, host, cidr, port) VALUES (?, ?, ?, ?)`,
			c.ID, a.Host, a.CIDR, a.Port,
		); err != nil {
			return writeFailure("insert apiserver",
				fmt.Sprintf("apiserver host %q 在本集群下重复", a.Host), "", err)
		}
	}
	for _, cidr := range c.HealthCheckSources {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO cluster_health_check_source (cluster_id, cidr) VALUES (?, ?)`,
			c.ID, cidr,
		); err != nil {
			return writeFailure("insert health check source",
				fmt.Sprintf("健康检查网段 %q 在本集群下重复", cidr), "", err)
		}
	}
	for _, sc := range c.MetricsScrapers {
		labels, err := json.Marshal(sc.Labels)
		if err != nil {
			return fmt.Errorf("marshal metrics scraper labels: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO cluster_metrics_scraper (cluster_id, namespace, labels_key, labels)
			 VALUES (?, ?, ?, ?)`,
			c.ID, sc.Namespace, sc.LabelsKey(), labels,
		); err != nil {
			return writeFailure("insert metrics scraper",
				fmt.Sprintf("metrics 抓取端 %q 在本集群下重复", sc.Namespace), "", err)
		}
	}
	for _, a := range c.NodeAgents {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO cluster_node_agent (cluster_id, namespace, app, host_network, target_port)
			 VALUES (?, ?, ?, ?, ?)`,
			c.ID, a.Namespace, a.App, a.HostNetwork, a.TargetPort,
		); err != nil {
			return writeFailure("insert node agent",
				fmt.Sprintf("节点 agent %s/%s 在本集群下重复", a.Namespace, a.App), "", err)
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

// nullTimeIfNil 把 nil 指针转成 SQL NULL，非 nil 时取其指向的值。
//
// database/sql 不接受 *time.Time 作为驱动参数（它既不是基础类型也没
// 实现 driver.Valuer），必须在这里显式拆箱，否则 ExecContext 直接报错。
func nullTimeIfNil(t *time.Time) any {
	if t == nil {
		return nil
	}
	return *t
}

// SetOtherPlanes 记下一次策略平面探测的结论（design doc 2026-08-25 §2.3）。
//
// **与 data_source 同一条纪律：它不在 CreateCluster / UpdateCluster 的 SET
// 清单里。** 这是一个关于集群的**事实**，由采集层测出来写下，而不是人在页面上
// 填的 —— 一条能从常规 CRUD 改掉它的路径，等于允许操作者把一份不可信的判定
// 拨成可信。人能拨的那一份是 ccnp_present，且只能往降级方向拨
// （registry.Cluster.EffectivePlanes）。
//
// 不写审计：这与 SetGitVerifyResult 同类，是采集的产物而不是一次人的动作，
// 记进审计只会把"谁改了这个集群"淹没在探测流水里。运行记录在采集运行那张表。
func (s *Store) SetOtherPlanes(
	ctx context.Context, clusterID string, planes registry.PolicyPlanes,
) error {
	if !planes.Valid() {
		return fmt.Errorf("%w: other planes %q", registry.ErrInvalid, planes)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE cluster SET other_planes = ? WHERE cluster_id = ? AND deleted_at IS NULL`,
		string(planes), clusterID)
	if err != nil {
		return fmt.Errorf("set other planes: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set other planes: %w", err)
	}
	if n > 0 {
		return nil
	}
	// **RowsAffected 为 0 不等于「这个集群不存在」。** MySQL 对一次值没有
	// 变化的 UPDATE 报 0 行受影响（除非连接开了 CLIENT_FOUND_ROWS），而这个
	// 写入天生是幂等的：探测结论连续两轮相同是常态。把 0 直接当成 ErrNotFound
	// 的后果是每一轮采集都记一条"写不下去"的告警，然后没有人再看那类告警。
	//
	// 因此这里再问一次行在不在。多一次查询换掉一个只在"值没变"时才出现的
	// 假错误 —— 而那正是最常见的那种情形（本条由真集群上的一次采集发现）。
	var exists bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM cluster WHERE cluster_id = ? AND deleted_at IS NULL`, clusterID,
	).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return registry.ErrNotFound
		}
		return fmt.Errorf("set other planes: %w", err)
	}
	return nil
}
