package httpapi

import (
	"sort"
	"strconv"
	"time"

	"github.com/imkerbos/Distill/internal/flow"
	"github.com/imkerbos/Distill/internal/policygen"
	"github.com/imkerbos/Distill/internal/predict"
	"github.com/imkerbos/Distill/internal/store"
)

// renderPolicyCaveats 把「平台自己知道哪里没看全」写成给人读的几行。
//
// 导出文件的注释头与写回的提交信息共用这一个实现，不是为了少写一遍代码，
// 是因为分开写就会漂移：这两段文字是同一次判定的两处呈现，而它们的读者
// 做的是同一个决定——要不要把这批策略应用到集群上。两处说的话不一样时，
// 屏幕上不会有任何迹象。
//
// **为什么必须有这几行**：四类 dry-run 计数回答的是「按平台看到的这些，
// 应用会拦断什么」，它没有回答、也回答不了「平台没看到的那些呢」。窗口
// 不完整、基线推不出、主体挂不上、规则被放宽——每一条都意味着这份文件
// 之外还有东西，而导出的 YAML 会脱离平台独自存在，隔几天被人应用出去，
// 那时他手上只有这段文字。少了它们，读者读到的是四个数字加一句让人安心
// 的收尾（design doc §3.4 的同一形状）。
//
// line 由调用方给：导出侧加 "# " 前缀、写回侧不加，两侧都在自己的闭包里
// 过 headerSafe。这个函数只负责说什么，不负责怎么转义。
func renderPolicyCaveats(pv store.PolicyPreview, line func(format string, args ...any)) {
	line("—— 以下是平台知道自己没看全的地方 ——")

	// **证据停摆排在最前面，且在停摆时先于完整度说。**
	//
	// 其余每一栏描述的是"平台看到了什么、没看到什么"，而这一栏描述的是
	// "下面这些数字还是不是新的"。它不成立时，后面每一行都要打个折扣——
	// 包括完整度本身，因为完整度也是记账那一刻算出来的。
	//
	// 2026-08-29 实测：周期记账连续失败 13 小时，界面上唯一的症状是几个
	// 数字不再变大，而"不再变大"与"这段时间确实没有新观测"长得一模一样。
	if pv.EvidenceLag.Stale() {
		line("⚠ 证据已停止更新: 记账最远记到 %s，而流量已经摄入到 %s，落后 %s。",
			lagStamp(pv.EvidenceLag.AccountedTo), lagStamp(pv.EvidenceLag.IngestedTo),
			pv.EvidenceLag.Behind().Round(time.Minute))
		line("  下面每一条的证据计数都停在那一刻，**不是这一刻的集群状态**。")
		line("  先查平台的周期记账为什么没跑，再决定要不要用这份文件。")
	}

	// 完整度单独一行且永远打印：它是「学到的规则能不能被信」这条链的源头
	// （完整度非 COMPLETE → 可信度 DEGRADED → 证据 INCOMPLETE_WINDOW →
	// 规则默认禁用）。缺了它，读者无从判断这份文件是「看全了之后的结论」
	// 还是「看了一部分的结论」。
	switch pv.WindowCompleteness {
	case flow.CompletenessComplete:
		line("观测窗口完整度: COMPLETE —— 窗口内每一轮采集都到齐了。")
	case flow.CompletenessDegraded:
		line("观测窗口完整度: DEGRADED —— 窗口内有采集缺口。" +
			"这份文件里没有的放行，不等于集群里不需要它。")
	case flow.CompletenessUnknown:
		line("观测窗口完整度: UNKNOWN —— 平台判断不出窗口是否完整，按不完整对待。")
	default:
		// 空值或将来新增的取值：照原样印出来。**不静默**——一个认不出的
		// 完整度被当成"没问题"是这里最坏的失败方式。
		line("观测窗口完整度: %q —— 平台不认识这个取值，按不完整对待。", string(pv.WindowCompleteness))
	}

	line("规则粒度: %s", granularityLabel(pv.Granularity))

	said := false
	say := func(format string, args ...any) {
		said = true
		line(format, args...)
	}

	if len(pv.Kinds) > 0 {
		line("参与推导的基线类别: %s", joinKinds(pv.Kinds))
	}

	if n := len(pv.MissingBaselines); n > 0 {
		say("基线推导不出: %d 个命名空间 —— 这些命名空间缺了本该有的放行。", n)
		listEach(line, n, func(i int) string {
			m := pv.MissingBaselines[i]
			return m.Namespace + ": " + joinKinds(m.Kinds)
		})
	}
	if n := len(pv.NotApplicableBaselines); n > 0 {
		say("基线不适用: %d 个命名空间 —— 平台判定这些类别在此处用不上。", n)
		listEach(line, n, func(i int) string {
			m := pv.NotApplicableBaselines[i]
			return m.Namespace + ": " + joinKinds(m.Kinds)
		})
	}
	if n := len(pv.NotAssessedBaselines); n > 0 {
		say("基线未评估: %s —— 平台这一轮没有判断这些类别，既不是有也不是没有。",
			joinKinds(pv.NotAssessedBaselines))
	}

	if n := len(pv.UnattachedImports); n > 0 {
		say("集群既有策略挂不上主体: %d 条 —— 它们仍在集群里生效，但平台没能把它们"+
			"对应到 workload 上，因此没有计入上面的 dry-run。", n)
		listEach(line, n, func(i int) string {
			u := pv.UnattachedImports[i]
			return u.Namespace + "/" + u.Name + "（" + string(u.Reason) + "）"
		})
	}
	if n := len(pv.UnattachedBaselines); n > 0 {
		say("基线规则挂不上主体: %d 条 —— 推导出了规则，却找不到它该选中谁。", n)
		listEach(line, n, func(i int) string {
			u := pv.UnattachedBaselines[i]
			return string(u.Kind) + " " + u.Namespace + "/" + u.Name + "（" + string(u.Reason) + "）"
		})
	}

	if n := len(pv.Widening); n > 0 {
		ns, rules, grants := 0, 0, 0
		for _, w := range pv.Widening {
			ns++
			rules += w.Rules
			grants += w.ExtraGrants
		}
		say("规则被放宽: %d 个命名空间、%d 条规则，比观测到的多放行 %d 组 —— "+
			"按命名空间归并的代价，不是观测到的流量。", ns, rules, grants)
		listEach(line, n, func(i int) string {
			w := pv.Widening[i]
			return w.Namespace + "（" + strconv.Itoa(w.Rules) + " 条规则，多放行 " + strconv.Itoa(w.ExtraGrants) + " 组）"
		})
	}
	// **只报 ExtraPods > 0 的。** pv.ExposureWidenings 是每条已挂上的暴露
	// 规则一行的全账，不是放宽清单：ExtraPods = WorkloadPods − SelectedPods，
	// 为 0 意味着生成的 podSelector 覆盖的正好就是 Service 暴露的那一批，
	// 一个 Pod 都没多放。把它也写成"放宽"，是在这份文件里断言一件没发生
	// 的事——与漏报同样是假的，方向还相反。
	widened := make([]policygen.ExposureWidening, 0, len(pv.ExposureWidenings))
	for _, e := range pv.ExposureWidenings {
		if e.ExtraPods > 0 {
			widened = append(widened, e)
		}
	}
	if n := len(widened); n > 0 {
		say("暴露放宽: %d 个 Service —— 生成的 podSelector 覆盖到了 Service 并没有"+
			"暴露的 Pod，放行范围比 Service 本身大。", n)
		listEach(line, n, func(i int) string {
			e := widened[i]
			return e.Namespace + "/" + e.Service + " → " + e.Workload +
				"（规则覆盖 " + strconv.Itoa(e.WorkloadPods) + " 个 Pod，其中 " +
				strconv.Itoa(e.ExtraPods) + " 个 Service 并没有暴露）"
		})
	}

	if n := len(pv.ExcludedNamespaces); n > 0 {
		say("排除的命名空间: %d 个 —— 本文件完全不涉及它们。", n)
		listEach(line, n, func(i int) string {
			e := pv.ExcludedNamespaces[i]
			return e.Namespace + "（" + string(e.Reason) + "）"
		})
	}
	if n := len(pv.ExcludedWorkloads); n > 0 {
		say("排除的主体: %d 个 —— 平台在它们上面判不准，本文件不动它们。", n)
		listEach(line, n, func(i int) string {
			e := pv.ExcludedWorkloads[i]
			return e.Namespace + "/" + e.Pod + "（" + string(e.Reason) + "）"
		})
	}
	if n := len(pv.Ungeneratable); n > 0 {
		say("生成不出规则的流: %d 条 —— 观测到了，但没能变成规则；"+
			"应用 default-deny 之后它们会被拦断。", n)
		// 按原因归并，**不逐条列**，也**不带 Detail**。两个理由：
		//
		// 逐条列没有信息量——同一个原因会重复上百次，而读者要做的判断
		// ("哪一类流量会被拦断、有多少")只取决于原因和条数。
		//
		// 更要紧的是 Detail 是自由文本，会把集群内容（命名空间/Pod 名、
		// 标签取值）插进去。这段文字要落进仓库历史，而仓库历史撤不回来。
		// Reason 是封闭枚举（规范 §3），说清了是哪一类，也只说这些。
		byReason := map[policygen.UngeneratableReason]int{}
		for _, u := range pv.Ungeneratable {
			byReason[u.Reason]++
		}
		reasons := make([]string, 0, len(byReason))
		for r := range byReason {
			reasons = append(reasons, string(r))
		}
		sort.Strings(reasons)
		listEach(line, len(reasons), func(i int) string {
			r := reasons[i]
			return r + "：" + strconv.Itoa(byReason[policygen.UngeneratableReason(r)]) + " 条"
		})
	}
	// **这一栏最要紧,因为四类计数报不出它。** 其余每一栏描述的都是"平台没
	// 看到什么",而这一栏描述的是"策略里有些东西本窗口没见过"——它放行的
	// 流量本窗口内不存在,不产生任何 change kind,于是 WOULD_OPEN 一动不动,
	// 而策略集实际放行的比这个窗口的证据支持的多(design doc 2026-08-29 §3.4)。
	if n := len(pv.UnobservedRules); n > 0 {
		oldest := pv.UnobservedRules[0].LastSeen
		for _, u := range pv.UnobservedRules {
			if u.LastSeen.Before(oldest) {
				oldest = u.LastSeen
			}
		}
		say("来自累积证据的规则: %d 条 —— 它们在本次求值窗口内没有出现，"+
			"放行范围因此大于这个窗口的证据所能支持的。"+
			"上面四类 dry-run 计数**算不到它们**：那几个数比较的是观测到的流量，"+
			"而这些规则放行的流量本窗口里根本没有。最早一条上次出现在 %s。",
			n, oldest.UTC().Format(time.RFC3339))
		listEach(line, n, func(i int) string {
			u := pv.UnobservedRules[i]
			return u.Namespace + "/" + u.Workload +
				"（上次出现 " + u.LastSeen.UTC().Format(time.RFC3339) + "）"
		})
	}

	if n := len(pv.StaleOverrides); n > 0 {
		say("失效的人工决定: %d 条 —— 曾经有人对这些规则做过决定，而规则已经变了，"+
			"那个决定没有落在本文件上。", n)
		// 同样按原因归并：Reason 取自两个常量，逐条列出去就是同一句话
		// 重复 n 遍。
		byReason := map[string]int{}
		for _, o := range pv.StaleOverrides {
			byReason[o.Reason]++
		}
		reasons := make([]string, 0, len(byReason))
		for r := range byReason {
			reasons = append(reasons, r)
		}
		sort.Strings(reasons)
		listEach(line, len(reasons), func(i int) string {
			return reasons[i] + "：" + strconv.Itoa(byReason[reasons[i]]) + " 条"
		})
	}

	if !said {
		// 显式说"无"，不是留空。一份少了这几行的文件与一份老格式的文件、
		// 与一份渲染器出了问题的文件长得一模一样，而三者的含义完全不同。
		line("除以上两项外，本次没有其他缺口：基线均已推导、主体均已挂上、" +
			"没有规则被放宽、没有主体被排除。")
	}
}

// caveatListCap 限制逐条列举的条数。**超出的部分明说有多少条**——
// 静默截断读起来就是"只有这些"，而那正是这几行要防的那句话。
const caveatListCap = 8

func listEach(line func(format string, args ...any), n int, at func(i int) string) {
	shown := n
	if shown > caveatListCap {
		shown = caveatListCap
	}
	for i := 0; i < shown; i++ {
		line("  - %s", at(i))
	}
	if n > shown {
		line("  - 另有 %d 条未列出，完整清单见平台上的策略预览页。", n-shown)
	}
}

func granularityLabel(g policygen.Granularity) string {
	switch g {
	case policygen.GranularityWorkload:
		return "WORKLOAD —— 每个 workload 一条策略。"
	case policygen.GranularityNamespace:
		return "NAMESPACE —— 按命名空间归并，放行范围大于观测到的流量。"
	default:
		return string(g) + " —— 平台不认识这个取值。"
	}
}

// renderPolicyBasis 渲染这次判定的依据：时间窗与四类 dry-run 计数——
// 或者，在一条流量都没观测到时，明说这几个数字不存在。
//
// 与 renderPolicyCaveats 同样是导出注释头与写回提交信息共用。分开写的代价
// 在这里比在缺口那几行更直接：**注释头本来就有「没观测到流量就不印那四个
// 0」这一段判断，提交信息没有**，于是同一份判定在文件里说「没人评估过」、
// 在合并请求上说「WOULD_BREAK: 0」。后者是评审人唯一会读的那句话。
//
// counts 由调用方给：写回侧用的是出计划那一刻重算的一套，导出侧用的是
// 预览里应用过人工决定的那一套。两侧各自取值，这里只负责怎么说。
func renderPolicyBasis(
	pv store.PolicyPreview, counts map[predict.ChangeKind]int,
	line func(format string, args ...any),
) {
	if !pv.TrafficObserved {
		// **不印那四个 0。** 零条连接下它们全是 0，而这份文字会脱离平台
		// 独自存在——隔两天有人读到「dry-run WOULD_BREAK: 0」，读到的是
		// 「应用它不会打断任何东西」，而事实是没有人评估过。
		//
		// 空缺本身也不行：一份少了那几行的文字与一份老格式的文字长得一样。
		// 必须显式说出来。
		line("时间窗: 无 —— 这个集群还没有任何流量观测")
		line("dry-run: 没有做过。平台一条流量都没有观测到这个集群，因此")
		line("  「应用这些策略会拦断什么」这个问题在这里没有答案。")
		line("下面的策略来自资产推导（Baseline），它们本身是真的；")
		line("**但在有流量数据之前，不要把这批策略当作评估过的变更应用出去。**")
		return
	}
	line("时间窗: %s ~ %s",
		pv.Window.From.UTC().Format(time.RFC3339), pv.Window.To.UTC().Format(time.RFC3339))
	for _, k := range predict.AllChangeKinds() {
		line("dry-run %s: %d", k, counts[k])
	}
}

// lagStamp 把时刻渲染成人读的形状；零值说"从来没有过"，不印一个 0001 年。
func lagStamp(t time.Time) string {
	if t.IsZero() {
		return "从来没有过"
	}
	return t.UTC().Format(time.RFC3339)
}
