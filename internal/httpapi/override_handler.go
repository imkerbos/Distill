package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/imkerbos/Distill/internal/policygen"
	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/response"
)

// overridePayload 是人工决定的请求体。
//
// From/To 是可选的：调用方在预览页看到的候选集若不是用默认窗口生成的
// （比如自己传了 ?from=&to= 查了一段自定义时间），提交覆盖时必须带上
// 同一个窗口，否则服务端会拿默认窗口重新生成候选集去核验指纹——两个
// 窗口内容不一样，一条明明画在屏幕上的规则会被拒绝，而拒绝给出的理由
// 是"页面可能已过期"，那不是真正的原因，会把人指向错误的排查方向。
type overridePayload struct {
	Namespace   string `json:"namespace"`
	Workload    string `json:"workload"`
	Fingerprint string `json:"fingerprint"`
	Decision    string `json:"decision"`
	Reason      string `json:"reason"`
	From        string `json:"from"`
	To          string `json:"to"`
}

// baselineNotDisablableMsg 是禁用一条 BASELINE 规则被拒绝时的用户可见文案。
//
// 不直接回传 policygen.ErrBaselineNotDisablable.Error()：那句英文是
// 写给 errors.Is 做等值比较的，不是写给界面看的。两件事分开，文案才能
// 独立于错误值的具体写法演化，而不会把内部标识符原样露给用户。
const baselineNotDisablableMsg = "BASELINE 规则不接受人工禁用：需要修正其推导依据，而不是在这里覆盖"

// handleCreateOverride 记录一条人工确认或否决。
//
// 集群注册状态每个请求现查（spec §4.5）：未注册或已下线的集群必须
// 拒绝写入，这条门禁在导入策略那一轮就是靠教训才补上的——当时它漏了，
// 一个已下线集群仍然接受写入，留下一条时间戳晚于下线操作的审计行。
func handleCreateOverride(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		clusterID := chi.URLParam(r, "clusterID")
		if _, ok, err := d.Registry.Cluster(r.Context(), clusterID); err != nil {
			writeRegistryError(w, r, d, err)
			return
		} else if !ok {
			response.WriteBusiness(w, response.CodeNotFound)
			return
		}

		var p overridePayload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			response.WriteSystem(w, http.StatusBadRequest, response.CodeInvalidParam)
			return
		}

		o := registry.RuleOverride{
			ClusterID: clusterID, Namespace: p.Namespace, Workload: p.Workload,
			Fingerprint: p.Fingerprint,
			Decision:    policygen.OverrideDecision(p.Decision),
			Reason:      p.Reason,
			// 操作者来自会话而非请求体：任何允许调用方自称身份的
			// 审计记录，都无法在事后证明是谁做的。
			DecidedBy: actorOf(r).Username,
			DecidedAt: time.Now().UTC(),
		}
		if err := registry.ValidateOverride(o); err != nil {
			writeRegistryError(w, r, d, err)
			return
		}
		// 窗口决定"当前候选集"是哪一份：两者都不填才退回默认窗口——
		// 这是最常见的路径，必须继续可用。只填一个是半成品，与
		// parseWindow 在 /flows、/policy-preview 上的既有判据一致，
		// 当成调用方的输入错误，不当默认值处理。
		window, ok := parseWindow(
			map[string][]string{"from": {p.From}, "to": {p.To}}, d.DefaultWindow)
		if !ok {
			response.WriteBusiness(w, response.CodeInvalidParam)
			return
		}
		// 指纹必须先在当前候选集里核验，才能落库：过期页面提交的覆盖
		// 若不做这一步，不会报错，只会永远待在预览页的「已失效」那
		// 一节，而它从来就没生效过。
		if err := d.Reader.EnsureRuleExists(
			r.Context(), clusterID, o.Namespace, o.Workload, o.Fingerprint,
			o.Decision, window,
		); err != nil {
			if errors.Is(err, policygen.ErrBaselineNotDisablable) {
				response.WriteInvalid(w, baselineNotDisablableMsg)
				return
			}
			writeRegistryError(w, r, d, err)
			return
		}
		if err := d.Registry.CreateRuleOverride(r.Context(), actorOf(r), o); err != nil {
			writeRegistryError(w, r, d, err)
			return
		}
		response.WriteOK(w, map[string]string{"fingerprint": o.Fingerprint})
	}
}

// handleDeleteOverride 撤销一条人工决定。
func handleDeleteOverride(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		clusterID := chi.URLParam(r, "clusterID")
		q := r.URL.Query()
		ns, wl, fp := q.Get("namespace"), q.Get("workload"), q.Get("fingerprint")
		// 主键是四元组，缺任何一个直接拒绝，不做宽松匹配：一次少传
		// 参数的删除若按前缀执行，会一次撤掉一批人工决定，而调用方
		// 以为自己只撤了一条。
		if ns == "" || wl == "" || fp == "" {
			response.WriteInvalid(w,
				"namespace、workload、fingerprint 三个参数缺一不可")
			return
		}
		if _, ok, err := d.Registry.Cluster(r.Context(), clusterID); err != nil {
			writeRegistryError(w, r, d, err)
			return
		} else if !ok {
			response.WriteBusiness(w, response.CodeNotFound)
			return
		}
		if err := d.Registry.SoftDeleteRuleOverride(
			r.Context(), actorOf(r), clusterID, ns, wl, fp); err != nil {
			writeRegistryError(w, r, d, err)
			return
		}
		response.WriteOK(w, map[string]string{"fingerprint": fp})
	}
}
