package httpapi

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/imkerbos/Distill/internal/collectstore"
	"github.com/imkerbos/Distill/internal/predict"
	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/response"
	"github.com/imkerbos/Distill/internal/store"
)

// emptyExportMsg 是一条启用规则都没有时的拒绝理由。
//
// 拒绝而不是产出一个空文件（design doc 2026-08-14 §4）：一份空的 policy
// 文件应用下去是"什么都不允许"还是"什么都没做"，取决于集群里已有什么。
// 那个歧义不该由一次导出制造出来，而拒绝必须说明原因 —— 否则操作者只会
// 反复点那个按钮。
const emptyExportMsg = "当前时间窗下没有任何启用中的规则，无可导出内容；" +
	"空的策略文件在不同集群上含义相反，因此不生成。"

// namespacedExportMsg 是按命名空间筛选导出被拒绝时的理由。
//
// 说清楚"为什么不给"而不是只说"不支持"：操作者下一步的处置是清掉筛选
// 再导出，而一句"参数不合法"会把他引向检查命名空间名字拼写。
const namespacedExportMsg = "dry-run 预测按整集群计算，无法描述只含单个命名空间的策略集；" +
	"筛选后的文件会带着一份把文件里没有的规则也算进去的结论。请清除命名空间筛选后再导出。"

// policyExportAPIVersion 与 policyExportKind 是导出文档的 TypeMeta。
//
// 在渲染时补上而不是要求生成层带着走：候选策略在平台内部是一份待评估的
// 推荐，只有离开平台、要被 kubectl apply 的那一刻才需要这两行。
const (
	policyExportAPIVersion = "networking.k8s.io/v1"
	policyExportKind       = "NetworkPolicy"
)

// handlePolicyExport 把已确认的候选策略导出为可直接应用的 NetworkPolicy 文档。
//
// **入参与 policy-preview 完全相同，且用同一个 parseWindow 解析，渲染的是
// 同一次 Reader.PolicyPreview 的结果。** 这是本端点唯一真正的风险所在
// （design doc 2026-08-14 §2）：导出若自己走一条"当前规则集是什么"的路径，
// 就可能产出一份与操作者刚刚读到的那几个数字不对应的策略 —— 他会应用一份
// 自己从未评估过的东西，而屏幕上没有任何迹象。轮 3 出过同一形状的缺陷，
// 四道门禁全绿。
//
// 本端点只读：不落库、不写集群，与 policy-preview 同样是结构性成立
// （spec §9.1）。唯一的写入是审计。
func handlePolicyExport(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		clusterID := chi.URLParam(r, "clusterID")
		window, ok, err := parseWindow(r.Context(), r.URL.Query(), d.Reader, clusterID)
		// 一次流量都没摄入过时默认窗口答不出来，但候选策略照样给得出
		// （design doc 2026-08-18）。**拿不走的推荐等于没有推荐** —— 放行，
		// 由 renderPolicyExport 在头里说清这份文件没有经过任何评估。
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
		namespace := r.URL.Query().Get("namespace")
		// 筛选后的导出一律拒绝（design doc 2026-08-14 §4）。生成与预测恒按
		// 整集群跑，namespace 只裁展示；一份被裁剪的文件配上注释头里那四个
		// 整集群口径的数字，描述的就是另一套策略集 —— 操作者读到的影响把
		// 文件里根本不存在的规则也算了进去，且朝着让人放心的方向偏。
		// 空文件的歧义至少看得出来，这一份带着一个很有说服力的头。
		//
		// 拒绝在读取之前：这是入参范围本身不成立，与集群里有什么无关。
		if namespace != "" {
			response.WriteInvalid(w, namespacedExportMsg)
			return
		}
		// 导出跟着当前粒度走（design doc 2026-08-19 §9）：文件里那份策略必须
		// 与屏幕上看过的那一份逐条对应。
		pv, err := d.Reader.PolicyPreviewAtGranularity(r.Context(), clusterID, namespace, window,
			parseGranularity(r.URL.Query().Get("granularity")))
		if err != nil {
			writeReaderError(w, r, d, err)
			return
		}

		// 被否决的规则到不了这里：Overridden.Enabled 只含启用规则，
		// 也不以注释形式出现（design doc §4）—— 一份要被 apply 的文件里，
		// 注释掉的规则会被读成"平台建议但被人关掉了，也许该打开"。
		rules := countEnabledRules(pv.Overridden.Enabled)
		if rules == 0 {
			response.WriteInvalid(w, emptyExportMsg)
			return
		}

		exporter := actorOf(r)
		exportedAt := time.Now().UTC()
		body, err := renderPolicyExport(pv, exporter.Username, exportedAt)
		if err != nil {
			d.Logger.Error("render policy export failed",
				"request_id", RequestIDFrom(r.Context()),
				"cluster", clusterID, "error", err)
			response.WriteSystem(w, http.StatusInternalServerError, response.CodeInternal)
			return
		}

		// 审计先于响应体：写失败就整次导出失败，不"记一条日志照样把文件发
		// 出去"。导出的是这个集群完整的网络策略，一次没有留下痕迹的导出，
		// 事后没有任何东西能回答"谁把它拿走了"（规范 §43）。
		if err := d.Registry.RecordPolicyExport(r.Context(), exporter, registry.PolicyExport{
			ClusterID: clusterID, Namespace: namespace,
			From: pv.Window.From, To: pv.Window.To,
			Documents: len(pv.Overridden.Enabled), Rules: rules,
		}); err != nil {
			writeRegistryError(w, r, d, err)
			return
		}

		w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
		w.Header().Set("Content-Disposition",
			fmt.Sprintf("attachment; filename=%q", exportFilename(clusterID, exportedAt)))
		w.WriteHeader(http.StatusOK)
		// 头已发出，此处再无法改状态码；写失败留给日志中间件，
		// 与 response.write 同一处置。
		//
		// G705 是把这段响应体当成 HTML 看的误报：Content-Type 是
		// text/yaml，且 SecurityHeaders 中间件挂了 X-Content-Type-Options:
		// nosniff，浏览器不会把它当文档渲染；内容本身也已经过 headerSafe
		// 剥掉控制字符（见 renderPolicyDocs）。
		_, _ = w.Write(body) //nolint:gosec // G705: text/yaml + nosniff，不是 HTML 响应。
	}
}

// countEnabledRules 数一份导出文档集里的启用规则条数。
//
// 数规则而不是数文档：每个候选 workload 恒有一份文档（规则为空即
// default-deny），文档数因此永远不为零，拿它判空等于那条判空永远不成立。
func countEnabledRules(docs []networkingv1.NetworkPolicy) int {
	n := 0
	for _, p := range docs {
		n += len(p.Spec.Ingress) + len(p.Spec.Egress)
	}
	return n
}

// renderPolicyExport 渲染带自述注释头的 YAML 文档集。
//
// 注释头的存在理由是这份文件会脱离平台独自存在 —— 落进一个 PR、贴进工单、
// 隔两天才被应用。到那时唯一能回答"当初凭什么认为它安全"的，只有文件
// 自己带着的这段话（design doc 2026-08-14 §3）。
//
// **头里只有下面这几行，不含任何凭据、host key、内部地址或账号哈希。**
// 每一个取值要么是调用方本来就知道的（集群、命名空间、时间窗、他自己的
// 账号名），要么是本次判定的产物（四类计数、条数、时刻）。
func renderPolicyExport(pv store.PolicyPreview, exporter string, at time.Time) ([]byte, error) {
	return renderPolicyDocs(pv, pv.Overridden.Enabled, "", exporter, at)
}

// renderPolicyDocs 渲染 docs 这一批策略，可选地声明本文件所属的命名空间。
//
// 导出与写回共用这一段（design doc 2026-08-14 §7）：写进 Git 的必须就是操作者
// 能下载下来的那份内容。写回按命名空间切分文件（2026-08-24 design doc §3），
// 因此这里收 docs 而不是自己从 pv 里取全量 —— 切分发生在调用方，渲染只有一份。
//
// fileNamespace 非空表示这是一份只含该命名空间策略的文件。此时注释头必须
// 额外说明四类计数是**整集群**口径：按命名空间拆计数会漏掉跨命名空间的影响，
// 而一个不说明口径的数字会被读成"本命名空间的影响"（design doc §3.4）。
func renderPolicyDocs(
	pv store.PolicyPreview, docs []networkingv1.NetworkPolicy,
	fileNamespace, exporter string, at time.Time,
) ([]byte, error) {
	var buf bytes.Buffer

	// 与写回同一份口径：并入集群已有策略之后的那一套
	// （design doc 2026-08-25-existing-policies §3）。文件注释头与提交信息
	// 是同一批数字的两处呈现，取不同的两份会让下载下来的那份与 PR 上写的
	// 对不上，而屏幕上没有任何迹象。
	counts := pv.OverriddenPredictionWithExisting().Counts
	line := func(format string, args ...any) {
		buf.WriteString("# " + headerSafe(fmt.Sprintf(format, args...)) + "\n")
	}
	line("Distill 已确认策略导出")
	line("集群: %s", pv.Cluster)
	line("命名空间筛选: %s", namespaceLabel(pv.Namespace))
	if fileNamespace != "" {
		line("本文件命名空间: %s", fileNamespace)
	}
	if pv.TrafficObserved {
		line("时间窗: %s ~ %s",
			pv.Window.From.UTC().Format(time.RFC3339), pv.Window.To.UTC().Format(time.RFC3339))
		// 四类计数取应用人工决定之后的那一套，与文档同源：文件渲染的正是
		// Overridden 那一份候选集，配上默认推荐的数字就又是轮 3 那条缺陷。
		//
		// 口径写明是"合并之后"：读这份文件的人要据此判断 apply 的影响，
		// 而只跑候选集的那一份描述的是另一件事（把旧策略也清理掉）。
		for _, k := range predict.AllChangeKinds() {
			line("dry-run %s: %d", k, counts[k])
		}
	} else {
		// **不印那四个 0。** 零条连接下它们全是 0，而这份文件会脱离平台
		// 独自存在 —— 隔两天有人读到「dry-run WOULD_BREAK: 0」，读到的是
		// 「应用它不会打断任何东西」，而事实是没有人评估过。
		//
		// 空缺本身也不行：一份少了那几行的文件与一份老格式的文件长得一样。
		// 必须显式说出来。
		line("时间窗: 无 —— 这个集群还没有任何流量观测")
		line("dry-run: 没有做过。平台一条流量都没有观测到这个集群，因此")
		line("  「应用这些策略会拦断什么」这个问题在这份文件里没有答案。")
		line("下面的策略来自资产推导（Baseline），它们本身是真的；")
		line("**但在有流量数据之前，不要把这份文件当作评估过的变更应用出去。**")
	}
	line("策略文档: %d 份，启用规则: %d 条", len(docs), countEnabledRules(docs))
	if fileNamespace != "" && pv.TrafficObserved {
		// 数字给的是整集群口径，文件里就必须说出来。少了这一行，读者会把
		// 它当成"本命名空间的影响"，而那是一句平台没有算过的话。
		line("以上 dry-run 计数是整集群口径，不是本命名空间的影响。")
	}
	line("导出者: %s", exporter)
	line("导出时刻: %s", at.UTC().Format(time.RFC3339))
	if pv.TrafficObserved {
		line("以上 dry-run 结论算的是导出这一刻的集群状态，不是应用这一刻的。")
		line("集群在此之后发生的变化不在其中；应用前请重新导出并核对这几个数字。")
	}

	for _, p := range docs {
		doc := p
		doc.TypeMeta = metav1.TypeMeta{APIVersion: policyExportAPIVersion, Kind: policyExportKind}
		raw, err := yaml.Marshal(doc)
		if err != nil {
			return nil, fmt.Errorf("marshal %s/%s: %w", doc.Namespace, doc.Name, err)
		}
		buf.WriteString("---\n")
		buf.Write(raw)
	}
	return buf.Bytes(), nil
}

// namespaceLabel 把空筛选渲染成"全集群"。
//
// 留空会被读成"这一行漏填了"，而空筛选恰恰是范围最大的那一次导出。
func namespaceLabel(namespace string) string {
	if namespace == "" {
		return "全集群"
	}
	return namespace
}

// headerSafe 去掉注释头里的换行与其他控制字符。
//
// 注释头里出现的取值有一部分来自调用方（命名空间筛选、路径里的集群 ID）。
// 一个带换行的取值会让后续文本跳出注释、成为文档的一部分 —— 那是一条
// 往被 apply 的文件里注入 YAML 的路径。上游已经拒过不存在的集群与命名
// 空间，这一层是纵深防御，不是它的替代（规范 §19、§26）。
func headerSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// exportFilename 是下载文件名：集群 ID 加导出时刻（design doc §5）。
//
// 集群 ID 逐字符过滤而不是直接拼进去：它来自 URL 路径，而 Content-Disposition
// 的取值里出现引号、分号或换行会改变这个响应头本身的结构（规范 §26）。
func exportFilename(clusterID string, at time.Time) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_', r == '.':
			return r
		default:
			return '-'
		}
	}, clusterID)
	if safe == "" {
		safe = "cluster"
	}
	return fmt.Sprintf("distill-policy-%s-%s.yaml", safe, at.UTC().Format("20060102T150405Z"))
}
