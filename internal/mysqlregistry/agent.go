package mysqlregistry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/imkerbos/Distill/internal/registry"
)

// clusterAgentTarget 是 agent 相关审计行的 target。
//
// 与集群自身的 target 分开：「谁改了这个集群的登记」与「谁给这个集群签了
// 一把能往平台写数据的钥匙」是两个问题，共用一个 target 就等于要求追溯的
// 人先从前后值里认出这次写的是哪一边（同 gitRepoTarget 的理由）。
func clusterAgentTarget(agentID string) string {
	return "cluster-agent/" + agentID
}

// agentRecord 是 agent 审计行的前后值。
//
// **只放公开段与状态，不放 token_hash。** 哈希是离线爆破的输入，而审计表
// 长期留存并会被导出到事实层（规范 §19、§21，V4 spec §9.9）。审计要回答的
// 是「谁给哪个集群签了哪一把」，不是把那把钥匙再存一份。
type agentRecord struct {
	AgentID string              `json:"agentId"`
	State   registry.AgentState `json:"state"`
}

// IssueClusterAgent 登记一把新签发的 token，同事务写审计。
//
// 校验在入库前跑：一条哈希长度不对的记录进了库，之后表现成「这把 token
// 怎么都认不过」，而成因在写入侧，从症状反推不到（见 ValidateClusterAgent）。
func (s *Store) IssueClusterAgent(
	ctx context.Context, actor registry.Actor, a registry.ClusterAgent,
) error {
	if err := registry.ValidateClusterAgent(a); err != nil {
		return err
	}
	after := agentRecord{AgentID: a.AgentID, State: registry.AgentActive}
	return s.mutate(ctx, actor, a.ClusterID, "ISSUE_CLUSTER_AGENT",
		clusterAgentTarget(a.AgentID), nil, after,
		func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO cluster_agent
				        (cluster_id, agent_id, token_hash, state, created_by, created_at)
				 VALUES (?, ?, ?, ?, ?, ?)`,
				a.ClusterID, a.AgentID, a.TokenHash,
				string(registry.AgentActive), actor.Username, s.now(),
			); err != nil {
				// 唯一键冲突意味着这个公开段已经签发过。这不是服务故障，
				// 也几乎不可能是操作者填错 —— agent_id 由平台生成 ——
				// 但它必须是一次明确失败：静默覆盖会让旧 agent 的 token
				// 悄悄失效，而那个集群的采集会就此停摆且无人知道。
				return writeFailure("insert cluster agent",
					fmt.Sprintf("agent %q 已存在", a.AgentID), "", err)
			}
			return nil
		})
}

// RevokeClusterAgent 把一把 token 置为吊销，同事务写审计。
//
// **不删行**：认证层要能分辨「被吊销」与「从来不存在」。前者意味着有人正在
// 用一把该扔掉的凭据 —— 那是一条要被看见的信号；删了行就只剩「未知 token」，
// 与打错字没有区别。
//
// WHERE 带 state = ACTIVE，因此重复吊销答 ErrNotFound 而不是静默成功：
// 静默成功会让一次吊销错对象的操作看起来生效了。
//
// WHERE 也带 cluster_id，尽管 agent_id 全局唯一：少了那一半，一个管理员就能
// 吊销别的集群的 agent，而界面上看起来他只是在操作自己那个集群（规范 §8）。
func (s *Store) RevokeClusterAgent(
	ctx context.Context, actor registry.Actor, clusterID, agentID string,
) error {
	before := agentRecord{AgentID: agentID, State: registry.AgentActive}
	after := agentRecord{AgentID: agentID, State: registry.AgentRevoked}
	return s.mutate(ctx, actor, clusterID, "REVOKE_CLUSTER_AGENT",
		clusterAgentTarget(agentID), before, after,
		func(tx *sql.Tx) error {
			res, err := tx.ExecContext(ctx,
				`UPDATE cluster_agent
				    SET state = ?, revoked_at = ?
				  WHERE cluster_id = ? AND agent_id = ? AND state = ?`,
				string(registry.AgentRevoked), s.now(),
				clusterID, agentID, string(registry.AgentActive))
			if err != nil {
				return fmt.Errorf("revoke cluster agent: %w", err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return fmt.Errorf("revoke cluster agent: %w", err)
			}
			if n == 0 {
				return ErrNotFound
			}
			return nil
		})
}

// agentColumns 是两个读方法共用的列清单。
//
// 单独取名而不是各写一份：两处的列顺序必须一致，抄两份的那一天只会是
// 其中一处新增了列而另一处没有，而错位的 token_hash 只会表现成
// 「认证怎么都不过」，没有任何东西指向真正的成因。
const agentColumns = `cluster_id, agent_id, token_hash, state, created_by,
	                  created_at, last_seen_at, revoked_at`

// ClusterAgents 返回一个集群下的全部 agent，**含已吊销的**。
//
// 含已吊销：操作者要看得见「这个集群历史上签过几把、还剩几把活的」。
// 只显示活的，会让一次忘记吊销无从发现。
func (s *Store) ClusterAgents(ctx context.Context, clusterID string) ([]registry.ClusterAgent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+agentColumns+`
		   FROM cluster_agent WHERE cluster_id = ? ORDER BY created_at DESC, agent_id`,
		clusterID)
	if err != nil {
		return nil, fmt.Errorf("read cluster agents: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []registry.ClusterAgent
	for rows.Next() {
		a, err := scanClusterAgent(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("read cluster agents: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read cluster agents: %w", err)
	}
	return out, nil
}

// ClusterAgentByID 按公开段查一条记录。不存在时第二个返回值为 false。
//
// 这是认证路径上唯一一次查库，走 uk_cluster_agent_agent_id 唯一索引而不是
// 扫表：摄入是高频路径，一次全表比对在这里就是一个放大器（规范 §24）。
//
// **不按 state 过滤。** 已吊销的记录照样返回，由调用方判定 —— 认证层需要
// 分辨「被吊销」与「不存在」，在这里过滤掉就把两者合并了。
func (s *Store) ClusterAgentByID(ctx context.Context, agentID string) (registry.ClusterAgent, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+agentColumns+` FROM cluster_agent WHERE agent_id = ?`, agentID)
	a, err := scanClusterAgent(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return registry.ClusterAgent{}, false, nil
	}
	if err != nil {
		return registry.ClusterAgent{}, false, fmt.Errorf("read cluster agent: %w", err)
	}
	return a, true, nil
}

// TouchClusterAgent 记下一次成功认证的时刻。
//
// **不写审计**：每一次成功的摄入都会调它一次，写审计等于让审计表按摄入
// 频率增长，把「谁做了什么」淹掉（规范 §43 要的是可复盘的链条，不是流水）。
//
// 也不改 state：这是给操作者看的「这个 agent 还活着吗」，不是一条安全判定。
func (s *Store) TouchClusterAgent(ctx context.Context, agentID string, at time.Time) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE cluster_agent SET last_seen_at = ? WHERE agent_id = ?`, at, agentID); err != nil {
		return fmt.Errorf("touch cluster agent: %w", err)
	}
	return nil
}

// scanClusterAgent 把一行 cluster_agent 读进 registry.ClusterAgent。
//
// 可空的两列走 sql.NullTime：它们表示「还没发生过」，而不是某个默认时刻。
// 读成零值时间再渲染出去，会让「从未用过」显示成公元元年用过一次。
func scanClusterAgent(scan func(...any) error) (registry.ClusterAgent, error) {
	var (
		a         registry.ClusterAgent
		state     string
		lastSeen  sql.NullTime
		revokedAt sql.NullTime
	)
	if err := scan(&a.ClusterID, &a.AgentID, &a.TokenHash, &state,
		&a.CreatedBy, &a.CreatedAt, &lastSeen, &revokedAt); err != nil {
		return registry.ClusterAgent{}, err
	}
	a.State = registry.AgentState(state)
	if lastSeen.Valid {
		a.LastSeenAt = lastSeen.Time
	}
	if revokedAt.Valid {
		a.RevokedAt = revokedAt.Time
	}
	return a, nil
}
