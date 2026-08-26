package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/imkerbos/Distill/internal/collectstore"
	"github.com/imkerbos/Distill/internal/reconcile"
	"github.com/imkerbos/Distill/internal/response"
	"github.com/imkerbos/Distill/internal/snapshotstore"
	"github.com/imkerbos/Distill/internal/store"
)

// handleTopology 返回指定集群的通信拓扑。
func handleTopology(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 缺省 namespace 粒度；取值不在封闭枚举里必须报错而非静默回退 ——
		// 一个拼错的 level 会让界面展示 namespace 粒度，而使用者以为
		// 自己在看 workload 粒度。
		level := store.LevelNamespace
		if raw := r.URL.Query().Get("level"); raw != "" {
			if !store.ValidTopologyLevel(raw) {
				response.WriteBusiness(w, response.CodeInvalidParam)
				return
			}
			level = store.TopologyLevel(raw)
		}

		got, err := d.Reader.Topology(r.Context(), chi.URLParam(r, "clusterID"), level)
		if err != nil {
			writeReaderError(w, r, d, err)
			return
		}
		response.WriteOK(w, got)
	}
}

// handleQuality 返回指定集群的数据质量。
func handleQuality(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got, err := d.Reader.Quality(r.Context(), chi.URLParam(r, "clusterID"))
		if err != nil {
			writeReaderError(w, r, d, err)
			return
		}
		response.WriteOK(w, got)
	}
}

// handleReconciliation 报告平台判定与执行平面判定的一致率
// （design doc 2026-08-25 §3）。
//
// **GET 且只读**：它不落库、不改任何结论 —— 一份"此刻问一次"的答案。
// 落库的是后续的门禁决定，那条本来就有审计。
//
// 时间窗按集群现解，与其余带窗口的读方法同一条路：拿一个跨集群共用的常量
// 窗口去问，一致率会算在一段与这个集群无关的时间上。
func handleReconciliation(d Deps) http.HandlerFunc {
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
		got, err := d.Reader.Reconciliation(r.Context(), clusterID, window)
		if err != nil {
			writeReaderError(w, r, d, err)
			return
		}
		response.WriteOK(w, got)
	}
}

// writeReaderError 把数据层错误映射成响应。
//
// 资源不存在是业务级失败，返回 HTTP 200 + 20002 —— 查询一个不存在的
// 集群不是服务出了问题，不该计入服务错误率。其余错误按内部故障处理，
// 真实原因只进日志。
//
// 不存在的 namespace 与不存在的集群同码：两者都是"你问的东西不在"，
// 而不是"一切正常"。少了这一条，一次 namespace 拼写错误会得到一份
// 全绿的空报告。
//
// "还没有可用的采集"也不是服务故障，因此不落回 50001：那句"服务内部错误"
// 会把操作者支去查服务，而事实是这个集群还没被采过（design doc §6）。
// 这里认的是 collectstore 的哨兵而不是一个字符串前缀 —— 靠文本匹配的映射
// 会在改一句错误文案的时候悄悄失效，而失效之后的症状恰好就是它原本要
// 消除的那个 500。
func writeReaderError(w http.ResponseWriter, r *http.Request, d Deps, err error) {
	if errors.Is(err, store.ErrClusterNotFound) || errors.Is(err, store.ErrNamespaceNotFound) {
		response.WriteBusiness(w, response.CodeNotFound)
		return
	}
	// **先判 ErrNoFlowIngest，再判 ErrNoCollection。** 前者用 %w 包着后者，
	// 顺序反过来外层那条会把它整个吃掉，而两者的处置完全相反：20005 的文案
	// 是「请先跑一次采集与流量摄入」，一个已经采过资产、只是没接上流量来源的
	// 集群读到它会去重跑那一步已经成功过的资产采集，跑多少次这一屏都不会出数。
	// 该做的是部署采集器或开流量日志，而那句话只在 20009 里。
	if errors.Is(err, collectstore.ErrNoFlowIngest) {
		response.WriteBusiness(w, response.CodeNoIngestRun)
		return
	}
	if errors.Is(err, collectstore.ErrNoCollection) {
		response.WriteBusiness(w, response.CodeNoUsableCollection)
		return
	}
	// "这次查询必须点名一个集群"同样不是服务故障：请求本身格式无误，被拒绝
	// 的原因是平台上同时存在两种数据来源，一份跨来源的流量列表要么半真半假、
	// 要么让真集群整体缺席（design doc 2026-08-18 §3.2）。落回 50001 会让
	// 界面显示"服务内部错误"，而正确的提示是"请先选一个集群"。
	if errors.Is(err, store.ErrClusterRequired) {
		response.WriteBusiness(w, response.CodeClusterRequired)
		return
	}
	// "这条读路径还没接通"同样不是服务故障。方向仍然朝关 —— 拒绝的那一半
	// 不因这条映射而改变（collectstore/notyet.go 无条件拒绝）；改变的只是
	// 操作者读到的那句话：500 会把他支去查服务，而正确的信息是"平台还没接
	// 这条路，跑多少次采集都不会变"。同时它也不再被计进服务错误率。
	if errors.Is(err, collectstore.ErrReadNotCollectedYet) {
		response.WriteBusiness(w, response.CodeReadNotWired)
		return
	}
	d.Logger.Error("reader failed",
		"request_id", RequestIDFrom(r.Context()),
		"path", r.URL.Path,
		"error", err)
	response.WriteSystem(w, http.StatusInternalServerError, response.CodeInternal)
}

// ReconciliationTrendReader 读一致率的历史走向。
//
// 窄成一个方法而不是让 handler 拿整个 snapshotstore：这条路只需要"把历次
// 对账按时间取回来"，而那个 Store 还带着全部写方法（同 FlowIngestReader、
// settings.Provider 的理由）。
type ReconciliationTrendReader interface {
	ReconciliationTrend(
		ctx context.Context, clusterID string, limit int,
	) ([]snapshotstore.ReconciliationRun, error)
}

// maxTrendPoints 是趋势一次最多回多少个点。
//
// 与存储层的上限同值：两处不一致时，一个"要 200 个点"的请求会拿到 50 个，
// 而界面上那条线看起来只是短一点，没有任何迹象说明它被截断了。
const maxTrendPoints = 200

// trendPoint 是趋势上的一个点。
type trendPoint struct {
	WindowFrom time.Time `json:"windowFrom"`
	WindowTo   time.Time `json:"windowTo"`
	ComputedAt time.Time `json:"computedAt"`
	// Rate 是这个窗口的一致率；**算不出时为 null，不是 0**。
	//
	// 这是这个结构里最要紧的一件事：把"算不出"画成 0，趋势图上就会出现一个
	// 触底的点，读起来是"那天全错了"，而事实是那天没有可比对的连接（来源
	// 不报判定，或分母为零）。一条会说谎的曲线比没有曲线更糟。
	Rate *float64 `json:"rate"`
	// Comparable 是参与计算的连接数，一致率的分母。
	//
	// 必须跟着走：一个基于 3 条连接的 100% 与一个基于 3 万条的 100% 在图上
	// 是同一个点，而它们的含义差着数量级。
	Comparable int `json:"comparable"`
	// Under 与 Over 是两类分歧的条数。
	Under int `json:"under"`
	Over  int `json:"over"`
	// PlatformUnknown 是平台答不出的条数：**不是分歧**，是未覆盖。
	PlatformUnknown int `json:"platformUnknown"`
	// SourceReports 表示那次对账的来源到底报不报判定。
	//
	// Rate 为 null 时靠它区分两种原因：来源压根不报判定（这条接入方式对不了
	// 账），还是报了但那个窗口没有可比对的连接。两者的处置完全不同。
	SourceReports bool `json:"sourceReports"`
}

// handleReconciliationTrend 返回一致率的历史走向，最近的在前。
//
// **不带时间窗参数**：趋势要回答的是"最近这些轮里在变好还是变坏"，而按窗口
// 筛会让调用方有机会挑一段好看的区间。要看某一个窗口的明细，走
// /reconciliation 那条路。
func handleReconciliationTrend(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		clusterID := chi.URLParam(r, "clusterID")
		// 先确认集群存在：不确认的话，一个拼错的集群名会拿到一份空趋势，
		// 而空趋势读起来是"这个集群还没对过账"—— 一句没人算过的断言。
		if !registeredCluster(w, r, d) {
			return
		}
		if d.Reconciliations == nil {
			// 本部署不记录对账历史。**不是空数组** —— 空数组读起来是
			// "这个集群还没对过账"，而事实是这里根本没在记；前者会让操作者
			// 去等一份永远不会出现的趋势。
			response.WriteBusiness(w, response.CodeReadNotWired)
			return
		}
		runs, err := d.Reconciliations.ReconciliationTrend(r.Context(), clusterID, maxTrendPoints)
		if err != nil {
			writeReaderError(w, r, d, err)
			return
		}
		points := make([]trendPoint, 0, len(runs))
		for _, run := range runs {
			c := run.Report.Overall
			p := trendPoint{
				WindowFrom: run.WindowFrom.UTC(), WindowTo: run.WindowTo.UTC(),
				ComputedAt:      run.ComputedAt.UTC(),
				Under:           c[reconcile.ClassUnderPermissive],
				Over:            c[reconcile.ClassOverPermissive],
				PlatformUnknown: c[reconcile.ClassPlatformUnknown],
				SourceReports:   run.SourceReports,
			}
			p.Comparable = c[reconcile.ClassAgree] + p.Under + p.Over
			// 一致率复用纯包那一个定义，不在这里再算一遍：一个每处各算一遍
			// 的比率迟早会有两个口径，而门禁按其中一个拦人。
			if rate, ok := c.AgreementRate(); ok {
				p.Rate = &rate
			}
			points = append(points, p)
		}
		response.WriteOK(w, map[string]any{"cluster": clusterID, "points": points})
	}
}
