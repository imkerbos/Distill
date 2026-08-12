package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/response"
)

// importPayload 是导入请求体。
type importPayload struct {
	Role         string `json:"role"`
	Source       string `json:"source"`
	YAML         string `json:"yaml"`
	GitCommitSHA string `json:"gitCommitSha"`
}

// registeredCluster 解析路径里的集群，未注册时写好响应并返回 false。
//
// 三个导入端点都必须先过这一关，不能只靠 policy_import 的外键：外键只
// 证明 cluster 表里有那一行，而下线是软删除，行还在（spec §4.5）。少了
// 这一关，往一个已下线的集群里导入会成功，并留下一条时间戳晚于下线操作
// 的审计记录 —— 那是「注册表只是摆设」这个失败最坏的形态。
func registeredCluster(w http.ResponseWriter, r *http.Request, d Deps) bool {
	_, found, err := d.Registry.Cluster(r.Context(), chi.URLParam(r, "clusterID"))
	if err != nil {
		writeRegistryError(w, r, d, err)
		return false
	}
	if !found {
		response.WriteBusiness(w, response.CodeNotFound)
		return false
	}
	return true
}

// handleListImports 返回一个集群下的导入清单。
func handleListImports(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !registeredCluster(w, r, d) {
			return
		}
		list, err := d.Registry.PolicyImports(r.Context(), chi.URLParam(r, "clusterID"))
		if err != nil {
			writeRegistryError(w, r, d, err)
			return
		}
		if list == nil {
			list = []registry.PolicyImport{}
		}
		response.WriteOK(w, list)
	}
}

// handleCreateImport 解析、校验并记录一条导入。
func handleCreateImport(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !registeredCluster(w, r, d) {
			return
		}
		var p importPayload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			// 请求体不是合法 JSON 是协议层问题，与 handleCreateSession 对
			// 畸形请求体的处理保持一致。
			response.WriteSystem(w, http.StatusBadRequest, response.CodeInvalidParam)
			return
		}
		parsed, err := registry.ParseImport(p.YAML)
		if err != nil {
			// ParseImport 在这里把写坏的 YAML／ipBlock 拦下来，而不是让它
			// 一路走到求值层才产出 POLICY_MALFORMED —— 那样使用者会把
			// 自己的输入错误读成平台的缺陷。
			writeRegistryError(w, r, d, err)
			return
		}
		clusterID := chi.URLParam(r, "clusterID")
		now := time.Now().UTC()
		item := registry.PolicyImport{
			ClusterID: clusterID,
			// 导入标识由服务端生成：让调用方指定会引入两次导入互相覆盖的可能。
			ImportID:     fmt.Sprintf("%s-%s-%d", parsed.Namespace, parsed.Name, now.UnixNano()),
			Plane:        "networkpolicy",
			Role:         registry.ImportRole(p.Role),
			Source:       registry.ImportSource(p.Source),
			Namespace:    parsed.Namespace,
			Name:         parsed.Name,
			YAML:         p.YAML,
			SpecHash:     parsed.SpecHash,
			GitCommitSHA: p.GitCommitSHA,
			ImportedBy:   actorOf(r).Username,
			ImportedAt:   now,
		}
		if err := d.Registry.CreatePolicyImport(r.Context(), actorOf(r), item); err != nil {
			writeRegistryError(w, r, d, err)
			return
		}
		response.WriteOK(w, map[string]string{"importId": item.ImportID})
	}
}

// handleDeleteImport 删除一条导入。
func handleDeleteImport(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !registeredCluster(w, r, d) {
			return
		}
		clusterID := chi.URLParam(r, "clusterID")
		importID := chi.URLParam(r, "importID")
		if err := d.Registry.SoftDeletePolicyImport(
			r.Context(), actorOf(r), clusterID, importID); err != nil {
			writeRegistryError(w, r, d, err)
			return
		}
		response.WriteOK(w, map[string]string{"importId": importID})
	}
}
