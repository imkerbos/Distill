package policygen

import (
	"errors"
	"strings"
	"time"
)

// OverrideDecision 是人工决定的方向。封闭枚举。
type OverrideDecision string

const (
	// DecisionEnable 表示人工启用一条默认禁用的规则。
	DecisionEnable OverrideDecision = "ENABLE"
	// DecisionDisable 表示人工禁用一条默认启用的规则。
	DecisionDisable OverrideDecision = "DISABLE"
)

// allOverrideDecisions 是枚举的唯一登记处。
var allOverrideDecisions = []OverrideDecision{DecisionEnable, DecisionDisable}

// AllOverrideDecisions 返回全部已登记的决定方向。
func AllOverrideDecisions() []OverrideDecision {
	out := make([]OverrideDecision, len(allOverrideDecisions))
	copy(out, allOverrideDecisions)
	return out
}

// Valid 判断该方向是否已登记。
func (d OverrideDecision) Valid() bool {
	for _, known := range allOverrideDecisions {
		if d == known {
			return true
		}
	}
	return false
}

// ErrBaselineNotDisablable 表示试图禁用一条 Baseline 规则。
//
// 关掉 DNS 出向或健康检查入向的后果分别是解析失败与入口中断，
// 而这两类流量恰恰是学习环节发现不了、只能靠 Baseline 补上的。
// 需要关掉某条 Baseline 时正确的做法是修正它的推导依据，那条路径
// 有据可查，而一次 UI 上的禁用没有。
var ErrBaselineNotDisablable = errors.New("baseline rule cannot be disabled")

// Override 是一条人工确认或否决。
type Override struct {
	// Namespace 与 Workload 定位到一条候选策略。
	Namespace string `json:"namespace"`
	Workload  string `json:"workload"`
	// Fingerprint 是被覆盖规则的内容指纹。
	Fingerprint string `json:"fingerprint"`
	// Decision 是决定方向。
	Decision OverrideDecision `json:"decision"`
	// Reason 是做出该决定的理由，非空。
	//
	// 半年后有人问「这条 SSH 出公网为什么是开的」，答案得在这里。
	Reason string `json:"reason"`
	// DecidedBy 与 DecidedAt 是溯源信息。
	DecidedBy string    `json:"decidedBy"`
	DecidedAt time.Time `json:"decidedAt"`
}

// StaleOverride 是一条已失效的确认。
type StaleOverride struct {
	// Override 是当初的决定，含当时的理由与时间。
	Override Override `json:"override"`
	// CurrentRules 是该 (namespace, workload) 当前的全部规则描述。
	//
	// 列全部而不是猜「最接近的那一条」：猜错会把人的注意力引到错误的
	// 规则上，而这里恰恰是要他重新做判断的地方。workload 已经不存在时
	// 为空切片 —— 那本身就是答案。
	CurrentRules []string `json:"currentRules"`
	// Reason 是失效原因，取值为固定文案。
	Reason string `json:"reason"`
}

// 失效原因的固定文案。不用自由文本：界面要按原因分组，
// 而自由文本没法聚合。
const (
	staleFingerprintMismatch = "规则内容已变，指纹不再匹配"
	staleBaselineProtected   = "Baseline 规则不接受人工禁用"
)

// Apply 把覆盖应用到候选策略集，返回新的 Result 与失效清单。
//
// 不修改入参：默认推荐与人工版本必须能并列展示，改写原对象会让
// 「平台推荐了什么」这个问题失去答案。
func Apply(base Result, overrides []Override) (Result, []StaleOverride) {
	out := Result{
		Policies:         make([]CandidatePolicy, len(base.Policies)),
		MissingBaselines: base.MissingBaselines,
		// 不适用清单与缺失清单同源，人工覆盖改不了「这个 namespace 有没有
		// 推导对象」这件事实，因此原样带过来。
		NotApplicableBaselines: base.NotApplicableBaselines,
		Ungeneratable:          base.Ungeneratable,
		ExcludedWorkloads:      base.ExcludedWorkloads,
		// 挂不上的导入原样带过来：人工覆盖改的是"哪几条规则算启用"，
		// 改不了"这条导入挂没挂上主体"。丢掉它，覆盖之后那一份预览会显示
		// 成"全部导入都挂上了"—— 一份凭空好看起来的清单。
		UnattachedImports: base.UnattachedImports,
		// 同理：人工覆盖改不了"这条 Baseline 规则挂没挂上 workload"这件
		// 事实。丢掉它，覆盖之后那一份预览会显示成候选集完整，而一个真实
		// 存在的暴露依然没有任何放行、也没有任何信号。
		UnattachedBaselines: base.UnattachedBaselines,
		// 与上面几栏同理：人工覆盖改的是"哪几条规则算启用"，改不了
		// "哪一片命名空间平台默认不碰"。丢掉它，覆盖之后那一份预览会显示成
		// "全部命名空间都在候选集里"——一份凭空完整起来的清单。
		ExcludedNamespaces: base.ExcludedNamespaces,
		// 同理：人工覆盖改的是"哪几条规则算启用"，改不了 Service selector
		// 当初点没点名单个 Pod 这件事实。丢掉它，覆盖之后那一份预览会显示成
		// 这条暴露从未放宽过。
		ExposureWidenings: base.ExposureWidenings,
	}
	// 深拷贝规则切片：Result 里的 Policies 与 Rules 都是切片，
	// 直接复用底层数组会让对 out 的写入穿透回 base。
	for i, p := range base.Policies {
		rules := make([]Rule, len(p.Rules))
		copy(rules, p.Rules)
		// Granularity 必须带过来。丢掉它会让覆盖之后的那一份策略集落到零值，
		// 而零值不是任何一个登记取值 —— 渲染时走 workload 分支、却拿着一个
		// 空 Workload，产出的是 `candidate-` 加一个 {"": ""} 的 selector。
		out.Policies[i] = CandidatePolicy{
			Cluster: p.Cluster, Namespace: p.Namespace, Granularity: p.Granularity,
			Workload: p.Workload, WorkloadLabelKey: p.WorkloadLabelKey, Rules: rules,
		}
	}

	stale := make([]StaleOverride, 0, len(overrides))
	for _, o := range overrides {
		applied := false
		for pi := range out.Policies {
			p := &out.Policies[pi]
			if p.Namespace != o.Namespace || p.Workload != o.Workload {
				continue
			}
			for ri := range p.Rules {
				r := &p.Rules[ri]
				if r.Fingerprint != o.Fingerprint {
					continue
				}
				if r.Origin == OriginBaseline && o.Decision == DecisionDisable {
					stale = append(stale, StaleOverride{
						Override: o, CurrentRules: describeRules(p.Rules),
						Reason: staleBaselineProtected,
					})
					applied = true
					break
				}
				r.Enabled = o.Decision == DecisionEnable
				applied = true
				break
			}
			break
		}
		if !applied {
			stale = append(stale, StaleOverride{
				Override: o, CurrentRules: currentRulesOf(out, o.Namespace, o.Workload),
				Reason: staleFingerprintMismatch,
			})
		}
	}
	return out, stale
}

// currentRulesOf 返回某个 workload 当前的全部规则描述；不存在时为空切片。
func currentRulesOf(res Result, ns, wl string) []string {
	for _, p := range res.Policies {
		if p.Namespace == ns && p.Workload == wl {
			return describeRules(p.Rules)
		}
	}
	return []string{}
}

// describeRules 把一组规则渲染成人可读的一行一条。
func describeRules(rules []Rule) []string {
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		out = append(out, string(r.Direction)+" "+
			strings.Join(r.Peers, ",")+" "+strings.Join(r.Ports, ","))
	}
	return out
}
