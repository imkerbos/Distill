package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/imkerbos/Distill/internal/response"
)

// handlePolicyPreview 返回候选策略与 dry-run 预测。
//
// 本端点只读：候选策略不落库、不生成 Git 产物、不写集群。平台主服务
// 不持有日常 Kubernetes 策略写权限（spec §9.1），这里是结构性成立。
func handlePolicyPreview(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		clusterID := chi.URLParam(r, "clusterID")
		window, ok, err := parseWindow(r.Context(), r.URL.Query(), d.Reader, clusterID)
		if err != nil {
			writeReaderError(w, r, d, err)
			return
		}
		if !ok {
			response.WriteBusiness(w, response.CodeInvalidParam)
			return
		}
		pv, err := d.Reader.PolicyPreview(
			r.Context(),
			clusterID,
			r.URL.Query().Get("namespace"),
			window,
		)
		if err != nil {
			writeReaderError(w, r, d, err)
			return
		}
		response.WriteOK(w, pv)
	}
}
