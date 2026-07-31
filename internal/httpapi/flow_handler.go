package httpapi

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/imkerbos/Distill/internal/response"
	"github.com/imkerbos/Distill/internal/store"
)

// maxFlowLimit 限制单次返回的流量条数，避免一次请求拖垮界面。
const maxFlowLimit = 1000

// handleListFlows 返回按条件筛选的流量列表。
func handleListFlows(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()

		limit := 0
		if raw := q.Get("limit"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n <= 0 || n > maxFlowLimit {
				// 查询串取值不合法是业务级失败：请求本身格式是好的。
				response.WriteBusiness(w, response.CodeInvalidParam)
				return
			}
			limit = n
		}

		got, err := d.Reader.Flows(r.Context(), store.FlowFilter{
			Cluster:    q.Get("cluster"),
			Verdict:    q.Get("verdict"),
			Confidence: q.Get("confidence"),
			Limit:      limit,
		})
		if err != nil {
			writeReaderError(w, r, d, err)
			return
		}
		response.WriteOK(w, got)
	}
}

// handleFlowDecision 返回单条流量的完整判定与理由。
func handleFlowDecision(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dec, ok, err := d.Reader.Flow(r.Context(), chi.URLParam(r, "flowID"))
		if err != nil {
			writeReaderError(w, r, d, err)
			return
		}
		if !ok {
			response.WriteBusiness(w, response.CodeNotFound)
			return
		}
		response.WriteOK(w, dec)
	}
}
