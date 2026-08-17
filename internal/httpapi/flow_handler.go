package httpapi

import (
	"context"
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

// parseWindow 解析 from / to 查询参数，省略时向 Reader 现问这个集群的默认窗口。
//
// 二者必须同时给出或同时省略：只给一端时，另一端要么取"现在"、要么取
// "开天辟地"，两个默认都会让返回范围与用户以为筛的范围不一致，而界面
// 只会照实回显那个它并没有要求的窗口。
//
// **默认窗口按集群现问，不接受一个由调用方传进来的常量**
// （design doc 2026-08-18 §3.1）。签名里没有"兜底窗口"这个参数，正是为了让
// 「某条路径拿一段与这个集群无关的时间作答」在这个包里写不出来：之前那个
// 参数是 Deps.DefaultWindow，一个在启动时算好的合成数据集范围，六条路径全部
// 用它，而前端从不发 from/to。谁都可以再加第七条路径，但它拿不到别的窗口。
//
// 只在真的要用默认值时才去问 Reader：调用方已经点名 from/to 时，这次查询
// 与「我们能回答哪段时间」无关，不该因为那个集群还没摄入过就被拒绝。
//
// 三个返回值分得开三件事：err 是数据层答不出窗口（交给 writeReaderError，
// 例如「还没有可用的采集数据」），ok=false 是调用方的入参有问题，两者的响应
// 完全不同 —— 合并成一个会让一次「这个集群还没被摄入过」显示成「参数错误」。
func parseWindow(
	ctx context.Context, q map[string][]string, rd store.Reader, clusterID string,
) (store.TimeWindow, bool, error) {
	get := func(k string) string {
		if v := q[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	rawFrom, rawTo := get("from"), get("to")
	if rawFrom == "" && rawTo == "" {
		win, err := rd.DefaultWindow(ctx, clusterID)
		if err != nil {
			return store.TimeWindow{}, false, err
		}
		return win, true, nil
	}
	if rawFrom == "" || rawTo == "" {
		return store.TimeWindow{}, false, nil
	}
	from, ok := parseRFC3339(rawFrom)
	if !ok {
		return store.TimeWindow{}, false, nil
	}
	to, ok := parseRFC3339(rawTo)
	if !ok {
		return store.TimeWindow{}, false, nil
	}
	win := store.TimeWindow{From: from, To: to}
	if !win.Valid() {
		return store.TimeWindow{}, false, nil
	}
	return win, true, nil
}

// parseRFC3339 解析一个时间戳，解不开时第二个返回值为 false。
//
// 只回一个布尔而不是 error：解不开的时间戳是调用方的入参问题，与 parseWindow
// 的第三个返回值（数据层答不出窗口）不是一回事，两者的响应完全不同。把解析
// 失败混进那条 error 会让一次拼错的 from 显示成「该集群还没有可用的采集数据」。
func parseRFC3339(raw string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
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

		// 集群取自同一个 q.Get("cluster")，与下面 FlowFilter 用的是同一个值：
		// 默认窗口按集群推出来，问的集群与查的集群必须是同一个，否则一份
		// 关于 A 的列表会被 B 的观测范围筛过，而两个数字都答得出、都不报错。
		//
		// 不点名集群时分派器答 ErrClusterRequired（20006「请先选一个集群」），
		// 与它对 Flows 的答复一致 —— 一份跨来源的流量列表本就不能给
		// （design doc 2026-08-18 §3.2）。
		cluster := q.Get("cluster")
		window, ok, err := parseWindow(r.Context(), q, d.Reader, cluster)
		if err != nil {
			writeReaderError(w, r, d, err)
			return
		}
		if !ok {
			response.WriteBusiness(w, response.CodeInvalidParam)
			return
		}

		page, err := d.Reader.Flows(r.Context(), store.FlowFilter{
			Cluster:    cluster,
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
