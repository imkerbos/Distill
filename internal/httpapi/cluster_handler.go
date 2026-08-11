package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/response"
)

// clusterPayload 是集群注册与修改的请求体。
//
// 与 registry.Cluster 分开：接口形状属于边界层，直接复用领域类型会让
// 一次内部字段重命名变成一次不兼容的 API 变更。
type clusterPayload struct {
	ID                 string               `json:"id"`
	DisplayName        string               `json:"displayName"`
	PodCIDR            string               `json:"podCidr"`
	NodeCIDR           string               `json:"nodeCidr"`
	CCNPPresent        bool                 `json:"ccnpPresent"`
	State              string               `json:"state"`
	APIServers         []registry.APIServer `json:"apiServers"`
	HealthCheckSources []string             `json:"healthCheckSources"`
	Git                *registry.GitBinding `json:"git"`
}

// toCluster 把请求体转成领域对象。
//
// state 参数**完全忽略**：接入状态反映平台实际收到了什么，不是调用方的意愿。
// 只做「为空时兜底」是不够的 —— 那样一次显式的 {"state":"READY"} 仍会被接受，
// 等于允许把「还没有数据」标成「可以出推荐了」。创建一律从 REGISTERED 起步，
// 修改时保留库里已有的状态（见 handleUpdateCluster）。
func (p clusterPayload) toCluster() registry.Cluster {
	return registry.Cluster{
		State:              registry.StateRegistered,
		ID:                 p.ID,
		DisplayName:        p.DisplayName,
		PodCIDR:            p.PodCIDR,
		NodeCIDR:           p.NodeCIDR,
		CCNPPresent:        p.CCNPPresent,
		APIServers:         p.APIServers,
		HealthCheckSources: p.HealthCheckSources,
		Git:                p.Git,
	}
}

// decodeClusterPayload 解析请求体。
//
// 解析失败是协议层的问题（请求体本身不是合法 JSON），不是业务失败 ——
// 与 handleCreateSession 对畸形登录请求的处理保持一致，返回真实的 400
// 而不是 200 + code，否则网关与前端拦截器都需要先解析响应体才能
// 判断这是不是同一类故障。
func decodeClusterPayload(w http.ResponseWriter, r *http.Request) (clusterPayload, bool) {
	var p clusterPayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		response.WriteSystem(w, http.StatusBadRequest, response.CodeInvalidParam)
		return clusterPayload{}, false
	}
	return p, true
}

// handleListClustersFromRegistry 返回已注册的集群。
func handleListClustersFromRegistry(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		list, err := d.Registry.Clusters(r.Context())
		if err != nil {
			writeRegistryError(w, r, d, err)
			return
		}
		if list == nil {
			list = []registry.Cluster{}
		}
		response.WriteOK(w, list)
	}
}

// handleCreateCluster 注册一个集群。
func handleCreateCluster(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := decodeClusterPayload(w, r)
		if !ok {
			return
		}
		if err := d.Registry.CreateCluster(r.Context(), actorOf(r), p.toCluster()); err != nil {
			writeRegistryError(w, r, d, err)
			return
		}
		response.WriteOK(w, map[string]string{"id": p.ID})
	}
}

// handleUpdateCluster 修改集群。
func handleUpdateCluster(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := decodeClusterPayload(w, r)
		if !ok {
			return
		}
		p.ID = chi.URLParam(r, "clusterID")
		c := p.toCluster()
		// 保留库里已有的接入状态：修改网段不该把一个 READY 的集群打回 REGISTERED，
		// 也不该让调用方借修改之机把状态改成任何它想要的值。
		existing, found, err := d.Registry.Cluster(r.Context(), c.ID)
		if err != nil {
			writeRegistryError(w, r, d, err)
			return
		}
		if !found {
			response.WriteBusiness(w, response.CodeNotFound)
			return
		}
		c.State = existing.State
		if err := d.Registry.UpdateCluster(r.Context(), actorOf(r), c); err != nil {
			writeRegistryError(w, r, d, err)
			return
		}
		response.WriteOK(w, map[string]string{"id": p.ID})
	}
}

// handleDeleteCluster 下线集群。
func handleDeleteCluster(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "clusterID")
		if err := d.Registry.SoftDeleteCluster(r.Context(), actorOf(r), id); err != nil {
			writeRegistryError(w, r, d, err)
			return
		}
		response.WriteOK(w, map[string]string{"id": id})
	}
}

// actorOf 从请求的会话中取出操作者身份。
//
// 审计的 actor 必须来自会话而非请求体：任何允许调用方自称身份的
// 审计记录，都无法在事后证明是谁做的。
//
// 这些 handler 全部挂在 RequireSession 之后，取不到会话意味着路由
// 装配错了 —— 返回空串而不是 panic，让审计行留下一个可见的空洞，
// 比让进程崩掉更容易定位。
func actorOf(r *http.Request) registry.Actor {
	sess, ok := SessionFrom(r.Context())
	if !ok {
		return registry.Actor{}
	}
	return registry.Actor{Username: sess.Username}
}

// writeRegistryError 把 registry 层的错误映射为响应。
//
// 输入不合法与目标不存在都是业务失败，用 code + HTTP 200；
// 其余按内部错误处理，真实原因只进日志。判据是「该不该计入服务错误率」。
func writeRegistryError(w http.ResponseWriter, r *http.Request, d Deps, err error) {
	switch {
	case errors.Is(err, registry.ErrInvalid):
		response.WriteBusiness(w, response.CodeInvalidParam)
	case errors.Is(err, registry.ErrNotFound):
		response.WriteBusiness(w, response.CodeNotFound)
	default:
		d.Logger.Error("registry operation failed",
			"err", err, "request_id", RequestIDFrom(r.Context()))
		response.WriteSystem(w, http.StatusInternalServerError, response.CodeInternal)
	}
}
