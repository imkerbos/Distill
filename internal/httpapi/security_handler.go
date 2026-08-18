package httpapi

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/imkerbos/Distill/internal/collectstore"
	"github.com/imkerbos/Distill/internal/response"
	"github.com/imkerbos/Distill/internal/store"
)

// handleSecurity 返回一个集群的安全发现汇总。
func handleSecurity(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		clusterID := chi.URLParam(r, "clusterID")
		window, ok, err := parseWindow(r.Context(), r.URL.Query(), d.Reader, clusterID)
		// 这个集群一次流量都没摄入过时，默认窗口答不出来 —— 但这一屏有一栏
		// （裸奔 Pod）本来就与窗口无关，来自资产快照（design doc 2026-08-18
		// §4.2）。带一个空窗口继续，Reader 会按资产作答并把
		// trafficObserved=false 写进响应。
		//
		// 只放行这一个原因：集群压根没被采过仍然是 ErrNoCollection 而不是
		// ErrNoFlowIngest，那时要照旧拒绝。
		if errors.Is(err, collectstore.ErrNoFlowIngest) {
			window, ok, err = store.TimeWindow{}, true, nil
		}
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
