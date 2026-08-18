package httpapi

import (
	"net/http"

	"github.com/imkerbos/Distill/internal/response"
)

// agentSchemaVersion 是 agent 摄入契约的版本。
//
// agent 跑在别人的集群里，升级节奏不受平台控制。版本显式协商、不认识就
// 拒绝（design doc 2026-08-18 §4）—— 尽力而为的解析会让一次字段错位变成
// 一批静默的错误数据，而错误数据在这个平台上会一路走到策略推荐。
const agentSchemaVersion = 1

// handleAgentConfig 告诉一个 agent 它该采什么。
//
// **不下发 fleet 的任何登记**（design doc §3.4）：网段判定挪到了平台侧，
// agent 不需要知道别的集群存在，也就不该知道。把 fleet 网段发给每一个
// 被管集群，等于把整个 fleet 的拓扑发出去。
func handleAgentConfig() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		clusterID, ok := AgentClusterFrom(r.Context())
		if !ok {
			// 走到这里说明这条路由挂在了 RequireAgent 之外。拒绝而不是
			// 当作「没有归属」继续：一个答不出自己该采哪个集群的配置，
			// 交出去只会让 agent 采错东西。
			response.WriteSystem(w, http.StatusUnauthorized, response.CodeAgentUnauthenticated)
			return
		}
		response.WriteOK(w, map[string]any{
			"clusterId":     clusterID,
			"schemaVersion": agentSchemaVersion,
		})
	}
}
