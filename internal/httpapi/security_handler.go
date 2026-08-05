package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/imkerbos/Distill/internal/response"
)

// handleSecurity 返回一个集群的安全发现汇总。
func handleSecurity(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		window, ok := parseWindow(r.URL.Query(), d.DefaultWindow)
		if !ok {
			response.WriteBusiness(w, response.CodeInvalidParam)
			return
		}
		rep, err := d.Reader.Security(r.Context(), chi.URLParam(r, "clusterID"), window)
		if err != nil {
			writeReaderError(w, r, d, err)
			return
		}
		response.WriteOK(w, rep)
	}
}
