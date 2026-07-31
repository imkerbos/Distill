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

// flowListResponse 是流量列表的响应体。
//
// 不回裸数组：列表被截断时，只有把总数和实际返回条数一并写出来，
// 界面才可能告诉用户"还有 166 条没给你看"。少了这几个数字，
// 一次截断就变成了一句"一共就这些"的假话。
type flowListResponse struct {
	Items    []store.FlowRecord `json:"items"`
	Total    int                `json:"total"`
	Returned int                `json:"returned"`
	Limit    int                `json:"limit"`
}

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

		verdict, confidence := q.Get("verdict"), q.Get("confidence")
		// 取值不在封闭枚举里必须报错，不能当成"筛不到"：一个拼错的
		// verdict 会返回空列表，把输入错误伪装成"这个集群没有这类流量"。
		if verdict != "" && !store.ValidVerdict(verdict) {
			response.WriteBusiness(w, response.CodeInvalidParam)
			return
		}
		if confidence != "" && !store.ValidConfidence(confidence) {
			response.WriteBusiness(w, response.CodeInvalidParam)
			return
		}

		page, err := d.Reader.Flows(r.Context(), store.FlowFilter{
			Cluster:    q.Get("cluster"),
			Verdict:    verdict,
			Confidence: confidence,
			Limit:      limit,
		})
		if err != nil {
			writeReaderError(w, r, d, err)
			return
		}
		response.WriteOK(w, flowListResponse{
			Items:    page.Items,
			Total:    page.Total,
			Returned: len(page.Items),
			Limit:    page.Limit,
		})
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
