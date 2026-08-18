package httpapi

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/imkerbos/Distill/internal/agentauth"
	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/response"
)

// AgentClusterFrom 取出本次请求的 agent 所绑定的集群。
//
// **这是摄入路径取集群归属的唯一入口**（design doc 2026-08-18 §2）。请求体、
// 查询串、路径参数里出现的任何 clusterId 都到不了这里 —— 一个能自己声明
// 集群的 agent 可以把 A 集群的 Pod 写进 B 集群的身份表，而不同集群的 Pod
// CIDR 可能重叠，之后的 join 会落到错误的 Pod 上**且不报错**（CLAUDE.md §4）。
//
// 第二个返回值为 false 意味着 handler 挂在了 RequireAgent 之外，那是装配
// 错误，调用方必须拒绝而不是当作"没有归属"继续。
func AgentClusterFrom(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(ctxKeyAgentCluster).(string)
	return id, ok
}

// RequireAgent 认证一次来自集群 agent 的请求。
//
// 与 RequireSession 是两条**互不相通**的链：这里不读 Cookie，那里不读
// Authorization（design doc §3.3）。互不相通是刻意的：
//
//   - 人的会话不得成为一次摄入的身份 —— 摄入是往身份表里写，而会话代表
//     的是一个人在看页面；
//   - 一把泄漏的节点 agent token 不得成为一把能读全平台的钥匙。
//
// 也因此这条链上没有 authorizer：agent 没有角色，它能做什么完全由 token
// 绑定的那个集群决定。
//
// **失败一律 401 + 同一个码，差别只进日志**（规范 §22）：未知、已吊销、
// 格式不对、干脆没带，对调用方是同一句话。分开回答等于帮试探者确认哪个
// agent_id 是存在的、哪一把只是被吊销了。
func RequireAgent(d Deps) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearerToken(r)
			if !ok {
				refuseAgent(w, r, d, "no bearer token", "")
				return
			}
			agentID, ok := agentauth.Parse(token)
			if !ok {
				refuseAgent(w, r, d, "malformed token", "")
				return
			}

			agent, found, err := d.Registry.ClusterAgentByID(r.Context(), agentID)
			if err != nil {
				// 查不动库不是"放行"：一次数据库抖动不该变成一次无身份的
				// 写入（规范 §49）。也不是 401 —— 我们并没有判定这把 token
				// 无效，是根本没能判定。
				d.Logger.Error("cannot resolve an agent identity",
					"err", err, "agent", agentID, "request_id", RequestIDFrom(r.Context()))
				response.WriteSystem(w, http.StatusInternalServerError, response.CodeInternal)
				return
			}
			if !found {
				refuseAgent(w, r, d, "unknown agent", agentID)
				return
			}
			if agent.State != registry.AgentActive {
				// 已吊销要在日志里与"未知"分开：有人正在用一把该扔掉的
				// 凭据，那是一条要被看见的信号，不是一次打错字。
				refuseAgent(w, r, d, "revoked agent", agentID)
				return
			}
			if !agentauth.Matches(token, agent.TokenHash) {
				refuseAgent(w, r, d, "digest mismatch", agentID)
				return
			}

			// last_seen 写失败不影响这次认证：它是给操作者看的「这个 agent
			// 还活着吗」，不是一条安全判定。让它拒掉一次合法推送，是把
			// 可观测性的故障升级成功能故障。
			if err := d.Registry.TouchClusterAgent(r.Context(), agentID, time.Now().UTC()); err != nil {
				d.Logger.Warn("cannot record an agent's last seen time",
					"err", err, "agent", agentID, "request_id", RequestIDFrom(r.Context()))
			}

			// 身份在这里第一次成立，因此回填信箱也只能在这里 —— 请求日志
			// 要回答的「谁」，对这条链而言是一个 agent 而不是一个人。前缀
			// 让两者在日志里一眼分得开，也让它撞不上任何一个真实用户名。
			recordActor(r.Context(), "agent:"+agentID)

			ctx := context.WithValue(r.Context(), ctxKeyAgentCluster, agent.ClusterID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// refuseAgent 写出统一的拒绝响应，并把真实原因留在日志里。
//
// 单独抽出来，是为了让「对外只有一句话」这件事只有一个实现：五个拒绝点
// 各写一遍，迟早有一处会多说一句，而多说的那句正是试探者要的。
func refuseAgent(w http.ResponseWriter, r *http.Request, d Deps, reason, agentID string) {
	d.Logger.Warn("refused an agent request",
		"reason", reason, "agent", agentID, "request_id", RequestIDFrom(r.Context()))
	response.WriteSystem(w, http.StatusUnauthorized, response.CodeAgentUnauthenticated)
}

// bearerToken 取出 Authorization 头里的 Bearer 值。
//
// 方案名区分大小写：RFC 7235 说它不区分，但这里收窄成只认 "Bearer " ——
// 平台自己发的客户端只会用这一种写法，放宽只会多出几条等价路径，而每一条
// 都要有人记得一起改。
func bearerToken(r *http.Request) (string, bool) {
	tok, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !found || tok == "" {
		return "", false
	}
	return tok, true
}
