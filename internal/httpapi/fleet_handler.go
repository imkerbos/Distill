package httpapi

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/imkerbos/Distill/internal/collectstore"
	"github.com/imkerbos/Distill/internal/response"
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
