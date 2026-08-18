package policygen

import "sort"

// Granularity 是候选策略的主体粒度，封闭枚举。
//
// 显式字段而不是拿空 Workload 当信号：那会让「这个字段没填」与「这是
// namespace 粒度」变成同一个状态，而两者的处置完全不同
// （同 validRunErrorReason 那条注释）。
type Granularity string

const (
	// GranularityWorkload 是默认粒度：主体是一个 workload，podSelector 用它
	// 实际命中的标签键构造。
	GranularityWorkload Granularity = "WORKLOAD"
	// GranularityNamespace 把主体粗化到整个 namespace，podSelector 为空。
	//
	// **对端不跟着粗化。** 放行的对端仍然精确到 workload 与端口 ——
	// 只写 namespaceSelector 会把「放行 kube-system」变成「放行它里面每一个
	// Pod」，那是另一个数量级的放宽（design doc 2026-08-19 §1）。
	GranularityNamespace Granularity = "NAMESPACE"
)

// Valid 报告该粒度是否已登记。零值不合法。
func (g Granularity) Valid() bool {
	switch g {
	case GranularityWorkload, GranularityNamespace:
		return true
	default:
		return false
	}
}

// Widening 是折叠到 namespace 粒度之后**多放出去**的量。
//
// 粗化只会放宽：一条原本只属于某个 workload 的放行，折叠之后该 namespace 里
// 每个 Pod 都拿到了。这是一次刻意的取舍（可用性更安全、安全性更不安全），
// 也是 NetworkPolicy 落地的常规路径 —— 但**不得让它无声发生**
// （design doc §2）。
type Widening struct {
	// Namespace 是被折叠的命名空间。
	Namespace string `json:"namespace"`
	// Workloads 是该 namespace 里进入候选集的 workload 数。
	Workloads int `json:"workloads"`
	// Rules 是折叠后的规则条数，已按指纹去重。
	Rules int `json:"rules"`
	// ExtraGrants 是多出来的 (workload, 规则) 授权数：
	//
	//	Σ over 规则 (该 ns 的 workload 总数 − 原本持有这条规则的 workload 数)
	//
	// 为 0 表示这个 namespace 里每条规则本来就人人都有，折叠没有放宽任何
	// 东西。**这个 0 要能被答出来**：把无损的折叠与真的放宽了的混在一起，
	// 操作者就分不出哪几个 namespace 值得回到 workload 粒度去看。
	ExtraGrants int `json:"extraGrants"`
}

// AtNamespaceGranularity 把候选集折叠成一个 namespace 一份策略，
// 并报出折叠多放宽了多少。
//
// **是 Result 上的纯变换，不是第二次生成。** 给 Generate 加一个粒度参数
// 意味着两条各自演化的路径，而它们本该是同一批规则的两种呈现 —— 分家之后
// 没有任何东西看得出来（同 EnabledPolicies 与导出共用一段渲染的理由）。
//
// 折叠取的是**入参里的规则**，因此人工确认自动被尊重：确认记在 workload
// 粒度，而这里读到的已经是覆盖之后的集合。一条规则在 A 上被禁、在 B 上仍
// 启用时，折叠后它仍在 —— 那是**并集**，语义正确：这个 namespace 里确实还有
// Pod 需要它。namespace 粒度上不存在「只为这个 namespace 禁用」这个动作，
// 因为它的爆炸半径与 workload 粒度的同名动作完全不同，合用一个键会让两者
// 互相污染（design doc §4）。
//
// 其余三块（MissingBaselines / NotApplicableBaselines / NotAssessed 与
// Ungeneratable / ExcludedWorkloads）原样带过来：它们讲的是这个集群缺什么、
// 表达不了什么，与主体粒度无关。
func (r Result) AtNamespaceGranularity() (Result, []Widening) {
	// 每个 namespace 收集：workload 集合、按指纹归并的规则、每条规则原本
	// 属于几个 workload。
	type collected struct {
		workloads map[string]bool
		order     []string
		rules     map[string]Rule
		holders   map[string]int
	}
	byNS := map[string]*collected{}
	var nsOrder []string

	for _, p := range r.Policies {
		c, ok := byNS[p.Namespace]
		if !ok {
			c = &collected{
				workloads: map[string]bool{},
				rules:     map[string]Rule{},
				holders:   map[string]int{},
			}
			byNS[p.Namespace] = c
			nsOrder = append(nsOrder, p.Namespace)
		}
		// 同一个 workload 可能出现在多份策略里（不同标签键），按名字计一次：
		// ExtraGrants 的分母是"这个 namespace 里有多少个主体"，重复计数会让
		// 放宽量虚高，而虚高的警告与虚假的警告一样会被忽略。
		if p.Workload != "" && !c.workloads[p.Workload] {
			c.workloads[p.Workload] = true
		}
		// 每份策略里同一指纹只算一次持有者。
		seenHere := map[string]bool{}
		for _, rule := range p.Rules {
			if _, ok := c.rules[rule.Fingerprint]; !ok {
				c.rules[rule.Fingerprint] = rule
				c.order = append(c.order, rule.Fingerprint)
			} else if rule.Enabled {
				// 并集：任何一个 workload 上仍然启用，折叠后就启用。
				merged := c.rules[rule.Fingerprint]
				merged.Enabled = true
				c.rules[rule.Fingerprint] = merged
			}
			if p.Workload != "" && !seenHere[rule.Fingerprint] {
				seenHere[rule.Fingerprint] = true
				c.holders[rule.Fingerprint]++
			}
		}
	}

	out := Result{
		MissingBaselines:       r.MissingBaselines,
		NotApplicableBaselines: r.NotApplicableBaselines,
		Ungeneratable:          r.Ungeneratable,
		ExcludedWorkloads:      r.ExcludedWorkloads,
	}
	var widening []Widening

	sort.Strings(nsOrder)
	for _, ns := range nsOrder {
		c := byNS[ns]
		rules := make([]Rule, 0, len(c.order))
		extra := 0
		for _, fp := range c.order {
			rules = append(rules, c.rules[fp])
			// 这条规则原本只有 holders 个 workload 持有，折叠后整个
			// namespace 都持有 —— 差额就是多出来的授权。
			if d := len(c.workloads) - c.holders[fp]; d > 0 {
				extra += d
			}
		}
		out.Policies = append(out.Policies, CandidatePolicy{
			Cluster:     r.clusterOf(ns),
			Namespace:   ns,
			Granularity: GranularityNamespace,
			// Workload 与 WorkloadLabelKey 留空：主体是整个 namespace。
			// 那是折叠的结果，不是粒度的判据 —— 判据是 Granularity。
			Rules: rules,
		})
		widening = append(widening, Widening{
			Namespace: ns, Workloads: len(c.workloads),
			Rules: len(rules), ExtraGrants: extra,
		})
	}
	return out, widening
}

// clusterOf 取这个 namespace 下任意一份策略的集群名。
//
// 折叠不跨集群：一次 Generate 的产物只属于一个集群（Input.ClusterID），
// 因此这里取到的必然是同一个。单独写一个函数是为了不在折叠里凭空造一个
// 空的 Cluster —— 一份说不出自己属于哪个集群的策略，落进导出文件之后
// 无从追溯。
func (r Result) clusterOf(namespace string) string {
	for _, p := range r.Policies {
		if p.Namespace == namespace {
			return p.Cluster
		}
	}
	return ""
}
