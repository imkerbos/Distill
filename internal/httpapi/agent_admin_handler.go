package httpapi

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/imkerbos/Distill/internal/agentauth"
	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/response"
)

// agentView 是一条 agent 在可见面上的形态（design doc 2026-08-18 §3）。
//
// **没有 token 字段，也没有摘要字段。** 明文只在签发那一次的响应里出现；
// 摘要是离线爆破的输入，同样不出边界（规范 §19、§20）。
//
// 刻意不直接序列化 registry.ClusterAgent：那样一来，将来往模型上加一个
// 敏感字段就会自动出现在这个响应里，而 review 看到的只是模型那一处改动。
type agentView struct {
	AgentID string `json:"agentId"`
	State   string `json:"state"`
	// CreatedBy 是签发它的操作者，用于回答「这把钥匙是谁发的」。
	CreatedBy string `json:"createdBy"`
	CreatedAt string `json:"createdAt"`
	// LastSeenAt 为空表示这把 token 从未被用过 —— 与「很久以前用过」
	// 是两件事，因此空值不渲染成任何时刻。
	LastSeenAt string `json:"lastSeenAt,omitempty"`
	RevokedAt  string `json:"revokedAt,omitempty"`
}

// handleIssueClusterAgent 为一个集群签发一把新 token。
//
// 响应里带明文 token，**这是全平台唯一一处**。操作者拿走它去装 agent，
// 平台此后只有摘要。丢了只能重签一把、把旧的吊销 —— 找回不可能，这正是
// 期望性质（规范 §33）。
//
// 先确认集群存在再签发：给一个不存在的集群签出去的 token，之后会认到一个
// 查不到集群的归属上，而签发那一刻看起来完全成功。
func handleIssueClusterAgent(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		clusterID := chi.URLParam(r, "clusterID")
		_, found, err := d.Registry.Cluster(r.Context(), clusterID)
		if err != nil {
			writeRegistryError(w, r, d, err)
			return
		}
		if !found {
			response.WriteBusiness(w, response.CodeNotFound)
			return
		}

		token, agentID, hash, err := agentauth.Issue()
		if err != nil {
			// 随机数取不到不是「再试一次」的事：一把熵不足的 token 与一把
			// 正常 token 长得一模一样（规范 §32）。失败要响亮。
			d.Logger.Error("cannot mint an agent token",
				"err", err, "cluster", clusterID, "request_id", RequestIDFrom(r.Context()))
			response.WriteSystem(w, http.StatusInternalServerError, response.CodeInternal)
			return
		}

		actor := actorOf(r)
		if err := d.Registry.IssueClusterAgent(r.Context(), actor, registry.ClusterAgent{
			ClusterID: clusterID,
			AgentID:   agentID,
			TokenHash: hash,
			State:     registry.AgentActive,
			CreatedBy: actor.Username,
		}); err != nil {
			writeRegistryError(w, r, d, err)
			return
		}

		// 明文只在这一次交出去。它不进日志：这个进程的日志终点是集群日志，
		// 而那里留下的一把 token 与库里那把等效。
		response.WriteOK(w, map[string]string{"agentId": agentID, "token": token})
	}
}

// handleListClusterAgents 列出一个集群下的 agent，含已吊销的。
//
// 含已吊销：操作者要看得见「这个集群历史上签过几把、还剩几把活的」。
// 只显示活的，会让一次忘记吊销无从发现。
func handleListClusterAgents(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		agents, err := d.Registry.ClusterAgents(r.Context(), chi.URLParam(r, "clusterID"))
		if err != nil {
			writeRegistryError(w, r, d, err)
			return
		}
		out := make([]agentView, 0, len(agents))
		for _, a := range agents {
			out = append(out, agentView{
				AgentID:    a.AgentID,
				State:      string(a.State),
				CreatedBy:  a.CreatedBy,
				CreatedAt:  a.CreatedAt.UTC().Format(time.RFC3339),
				LastSeenAt: optionalTime(a.LastSeenAt),
				RevokedAt:  optionalTime(a.RevokedAt),
			})
		}
		response.WriteOK(w, out)
	}
}

// handleRevokeClusterAgent 吊销一把 token。
//
// 路径上的 clusterID 一并交给存储层定位（见 RevokeClusterAgent）：少了它，
// 一个管理员能吊销别的集群的 agent，而界面上看起来他只在操作自己那个。
func handleRevokeClusterAgent(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		err := d.Registry.RevokeClusterAgent(r.Context(), actorOf(r),
			chi.URLParam(r, "clusterID"), chi.URLParam(r, "agentID"))
		if err != nil {
			writeRegistryError(w, r, d, err)
			return
		}
		response.WriteOK(w, nil)
	}
}

// optionalTime 把零值时间渲染成空串，让 omitempty 生效。
//
// 零值不渲染成 "0001-01-01T00:00:00Z"：那个日期读起来像一个真实发生过的
// 时刻，而它的含义是「从来没有发生过」。
func optionalTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
