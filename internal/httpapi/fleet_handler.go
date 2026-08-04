package httpapi

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/imkerbos/Distill/internal/response"
	"github.com/imkerbos/Distill/internal/store"
)

// handleListClusters 返回全部集群概览。
func handleListClusters(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got, err := d.Reader.Clusters(r.Context())
		if err != nil {
			writeReaderError(w, r, d, err)
			return
		}
		response.WriteOK(w, got)
	}
}

// handleTopology 返回指定集群的通信拓扑。
func handleTopology(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got, err := d.Reader.Topology(r.Context(), chi.URLParam(r, "clusterID"))
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
func writeReaderError(w http.ResponseWriter, r *http.Request, d Deps, err error) {
	if errors.Is(err, store.ErrClusterNotFound) {
		response.WriteBusiness(w, response.CodeNotFound)
		return
	}
	d.Logger.Error("reader failed",
		"request_id", RequestIDFrom(r.Context()),
		"path", r.URL.Path,
		"error", err)
	response.WriteSystem(w, http.StatusInternalServerError, response.CodeInternal)
}
