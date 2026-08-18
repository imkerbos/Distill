package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/response"
)

// driftStatus 是漂移检测结论的响应形状。
type driftStatus struct {
	DriftResult registry.DriftResult `json:"driftResult"`
}

// handleGitBindingDrift 报告写进去的那份策略现在还在不在。
//
// **GET 而非 POST**：与 verify 那条相反，这次调用只读 —— 不写仓库、不改绑定、
// 不动锚点，也不落任何结论（design doc 2026-08-18-drift-detection §4）。
// 重放它没有副作用，因此它是一次读取。
//
// 结论不落库：它是「此刻问一次」的答案，存下来就会有人读到一个过期的
// IN_SYNC。要留痕的是操作者据此做的动作（重推），而那条本来就有审计。
//
// **未配置校验器时答 UNKNOWN**，由 settingsGitVerifier 给出 —— 这一层不自己
// 判 nil，那会让"没去看过"这个结论有两处定义。
func handleGitBindingDrift(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "clusterID")
		c, found, err := d.Registry.Cluster(r.Context(), id)
		if err != nil {
			writeRegistryError(w, r, d, err)
			return
		}
		// 集群不存在与集群没有绑定同码，与 handleVerifyGitBinding 一致：
		// 从调用方视角两者都是「要检测的那个东西不在」。
		if !found || c.Git == nil {
			response.WriteBusiness(w, response.CodeNotFound)
			return
		}
		if d.GitVerifier == nil {
			// 没有校验器的部署没去看过仓库。答 UNKNOWN 而不是 IN_SYNC ——
			// 后者会让操作者以为下发的东西还在（安全规范 §49）。
			response.WriteOK(w, driftStatus{DriftResult: registry.DriftUnknown})
			return
		}
		repo, ok := bindingVerificationTarget(w, r, d, *c.Git)
		if !ok {
			return
		}
		result := d.GitVerifier.Drift(r.Context(), repo, c.Git.PolicyPath, c.Git.LastWrittenCommit)
		response.WriteOK(w, driftStatus{DriftResult: result})
	}
}
