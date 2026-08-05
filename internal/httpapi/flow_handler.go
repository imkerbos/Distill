package httpapi

import (
	"net/http"
	"strconv"
	"time"

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
	// Window 是实际生效的查询时间窗。理由同上：一个按时间筛过的列表
	// 若不说明筛的是哪一段，与全量列表在界面上无法区分。
	Window store.TimeWindow `json:"window"`
}

// parseWindow 解析 from / to 查询参数。
//
// 二者必须同时给出或同时省略：只给一端时，另一端要么取"现在"、要么取
// "开天辟地"，两个默认都会让返回范围与用户以为筛的范围不一致，而界面
// 只会照实回显那个它并没有要求的窗口。省略时用装配方注入的默认窗口。
func parseWindow(q map[string][]string, fallback store.TimeWindow) (store.TimeWindow, bool) {
	get := func(k string) string {
		if v := q[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	rawFrom, rawTo := get("from"), get("to")
	if rawFrom == "" && rawTo == "" {
		return fallback, true
	}
	if rawFrom == "" || rawTo == "" {
		return store.TimeWindow{}, false
	}
	from, err := time.Parse(time.RFC3339, rawFrom)
	if err != nil {
		return store.TimeWindow{}, false
	}
	to, err := time.Parse(time.RFC3339, rawTo)
	if err != nil {
		return store.TimeWindow{}, false
	}
	win := store.TimeWindow{From: from, To: to}
	if !win.Valid() {
		return store.TimeWindow{}, false
	}
	return win, true
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

		window, ok := parseWindow(q, d.DefaultWindow)
		if !ok {
			response.WriteBusiness(w, response.CodeInvalidParam)
			return
		}

		page, err := d.Reader.Flows(r.Context(), store.FlowFilter{
			Cluster:    q.Get("cluster"),
			Verdict:    verdict,
			Confidence: confidence,
			Limit:      limit,
			Window:     window,
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
			Window:   page.Window,
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
