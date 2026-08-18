package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/imkerbos/Distill/internal/collectstore"
	"github.com/imkerbos/Distill/internal/policygen"
	"github.com/imkerbos/Distill/internal/response"
	"github.com/imkerbos/Distill/internal/store"
)

// handlePolicyPreview 返回候选策略与 dry-run 预测。
//
// 本端点只读：候选策略不落库、不生成 Git 产物、不写集群。平台主服务
// 不持有日常 Kubernetes 策略写权限（spec §9.1），这里是结构性成立。
func handlePolicyPreview(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		clusterID := chi.URLParam(r, "clusterID")
		window, ok, err := parseWindow(r.Context(), r.URL.Query(), d.Reader, clusterID)
		// 一次流量都没摄入过时默认窗口答不出来，但候选策略照样给得出：
		// Baseline 按 workload 无条件注入，依据是资产而不是流量
		// （design doc 2026-08-18）。带空窗口继续，Reader 会标注
		// trafficObserved=false。
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
		pv, err := d.Reader.PolicyPreviewAtGranularity(
			r.Context(),
			clusterID,
			r.URL.Query().Get("namespace"),
			window,
			parseGranularity(r.URL.Query().Get("granularity")),
		)
		if err != nil {
			writeReaderError(w, r, d, err)
			return
		}
		response.WriteOK(w, pv)
	}
}

// parseGranularity 把查询参数解成主体粒度。
//
// **缺省与未登记取值一律 WORKLOAD。** 那是本轮之前的行为（没有回归），也是
// 更精确的那一侧 —— 落到 NAMESPACE 会把一份本该只选中一个 workload 的策略
// 变成选中整个命名空间，而那个方向不该靠一个拼错的参数走到
// （安全规范 §49，design doc 2026-08-19 §6）。
//
// 不为拼错的取值报错：一个 ?granularity=namesapce 的请求拿到 workload 粒度
// 的策略，而响应里的 granularity 字段会照实回显 WORKLOAD —— 界面据此显示
// 当前粒度，拼错看得见。报错则会让一次手误变成一屏空白。
// 大小写不敏感：查询参数沿用拓扑那套小写词汇（?level=namespace 已经存在），
// 而枚举值是大写。直接比对两者永远不相等 —— 那种失败尤其难查，因为它不报错，
// 只是**静默落回默认粒度**，而响应里的回显看起来完全正常。
func parseGranularity(raw string) policygen.Granularity {
	if strings.EqualFold(raw, string(policygen.GranularityNamespace)) {
		return policygen.GranularityNamespace
	}
	return policygen.GranularityWorkload
}
