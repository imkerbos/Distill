package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/imkerbos/Distill/internal/response"
)

// handleSecurity 返回一个集群的安全发现汇总。
func handleSecurity(d Deps) http.HandlerFunc {
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
		rep, err := d.Reader.Security(r.Context(), clusterID, window)
		if err != nil {
			writeReaderError(w, r, d, err)
			return
		}
		response.WriteOK(w, rep)
	}
}
