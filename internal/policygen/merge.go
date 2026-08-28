package policygen

import (
	"time"

	networkingv1 "k8s.io/api/networking/v1"

	"github.com/imkerbos/Distill/internal/risk"
)

// LearnedRule 是一条跨窗口累积下来的规则。
type LearnedRule struct {
	Namespace   string
	Workload    string
	Fingerprint string
	// LastSeen 是最后一次观测到它的窗口末端。
	LastSeen time.Time
	// Observations 是累计观测次数，替代单窗口的 FlowCount。
	Observations int64
	// Rule 是还原出来的规则（UnmarshalRule 的产物）。
	Rule Rule
}

// UnobservedRule 是一条进了候选集、但**本次求值窗口内没有出现**的规则。
//
// 它必须被报出来，因为 dry-run 的四类计数报不出它：那几个数比较的是观测到的
// 流量在两套策略下的判定，而这条规则放行的流量本窗口里根本没出现，不产生任何
// change kind。于是策略集放行的比本窗口证据支持的多，而没有一个数字会动
// （design doc 2026-08-29 §3.4）。
type UnobservedRule struct {
	Namespace   string    `json:"namespace"`
	Workload    string    `json:"workload"`
	Fingerprint string    `json:"fingerprint"`
	LastSeen    time.Time `json:"lastSeen"`
}

// MergeLearned 把累积的规则并进一次窗口生成的候选集。
//
// 这是"规则集不再被单个观测窗口限死"的合并一端。窗口决定 dry-run 算在哪段
// 流量上，累积决定策略集里有哪些规则——两者分开之后，观测多久就能学到多久。
// UAT 实测 nacos:8848 的调用方 90 秒看得见 45 个、6 小时 222 个，曲线远未收敛
// （design doc 2026-08-29 §1.1）。
//
// **只增不减**：窗口里学到的一条都不动，累积只会让候选集更宽。
//
// **只并到已经有候选策略的 workload 上。** 一个不在当前花名册里的 workload
// 说明它已经不在集群里了，给它生成策略没有意义；而更要紧的是这里拿不到它的
// WorkloadLabelKey——凭空补一个会产出一条 selector 选不中任何 Pod 的策略，
// 那在 NetworkPolicy 语义下等于一条 default-deny。
func MergeLearned(res Result, learned []LearnedRule) (Result, []UnobservedRule) {
	if len(learned) == 0 {
		return res, nil
	}

	// 本次窗口已有的 (namespace, workload, fingerprint)。
	have := make(map[string]struct{}, len(res.Policies)*4)
	// workload → 候选策略下标，用来把累积规则挂回去。
	at := make(map[string]int, len(res.Policies))
	for i, p := range res.Policies {
		at[p.Namespace+"/"+p.Workload] = i
		for _, r := range p.Rules {
			have[p.Namespace+"/"+p.Workload+"/"+r.Fingerprint] = struct{}{}
		}
	}

	out := res
	out.Policies = make([]CandidatePolicy, len(res.Policies))
	copy(out.Policies, res.Policies)
	// 规则切片各自深拷贝：直接 append 会穿透回调用方那一份。
	for i := range out.Policies {
		rules := make([]Rule, len(out.Policies[i].Rules))
		copy(rules, out.Policies[i].Rules)
		out.Policies[i].Rules = rules
	}

	// 恒为切片而不是 nil：合并一定跑过，"一条都没有"是一个算过的空集。
	unobserved := []UnobservedRule{}
	for _, l := range learned {
		key := l.Namespace + "/" + l.Workload
		i, ok := at[key]
		if !ok {
			continue
		}
		if _, dup := have[key+"/"+l.Fingerprint]; dup {
			continue
		}
		have[key+"/"+l.Fingerprint] = struct{}{}

		r := l.Rule
		r.Fingerprint = l.Fingerprint
		// FlowCount 取累计观测数，不是某一个旧窗口的计数：界面上这个数
		// 回答的是"这条规则被看见过多少次"，而累计才是那个问题的答案。
		r.FlowCount = int(l.Observations)
		// Enabled 与 Risk 在这里重算，不从库里取（MarshalRule 刻意不存它们）：
		// 判据是纯函数，而存下来的那一份会在风险清单更新之后过期，
		// 过期的方向是"这个端口不再算风险"。
		rp, risky := risk.Lookup(portOf(r))
		r.Enabled = r.Evidence == EvidenceTrustedAllow && !risky
		if risky {
			copied := rp
			r.Risk = &copied
		}
		r.Peers, r.Ports = describeBody(r)

		out.Policies[i].Rules = append(out.Policies[i].Rules, r)
		unobserved = append(unobserved, UnobservedRule{
			Namespace: l.Namespace, Workload: l.Workload,
			Fingerprint: l.Fingerprint, LastSeen: l.LastSeen,
		})
	}
	return out, unobserved
}

// portOf 取一条规则的端口，供风险查表用。
//
// 取第一个数值端口：学习规则按 (对端, 协议, 端口) 聚合，一条规则只有一个端口。
// 取不到时返回 0——risk.Lookup(0) 不命中任何风险端口，也就是"查不出端口就不
// 标风险"。这个方向是对的：标错风险会让一条本该启用的规则被扣住，而扣住的
// 代价是人工确认，不是阻断。
func portOf(r Rule) int32 {
	var ports []networkingv1.NetworkPolicyPort
	switch {
	case r.Ingress != nil:
		ports = r.Ingress.Ports
	case r.Egress != nil:
		ports = r.Egress.Ports
	}
	for _, p := range ports {
		if p.Port == nil {
			continue
		}
		// 端口号超出 uint16 的记录当作取不到：那不是一个合法端口，而截断成
		// int32 会把它变成另一个**合法**端口号——于是风险查表查的是别人。
		v := p.Port.IntValue()
		if v > 0 && v <= 65535 {
			return int32(v)
		}
	}
	return 0
}

// describeBody 按规则体渲染展示串。
//
// 重新渲染而不是把展示串一起存下来：那是一份两天前的渲染，描述的是两天前的
// 标签（describeSelector 命中 workloadLabelKeys 时只留标签值）。
func describeBody(r Rule) (peers, ports []string) {
	var ps []networkingv1.NetworkPolicyPeer
	var pt []networkingv1.NetworkPolicyPort
	switch {
	case r.Ingress != nil:
		ps, pt = r.Ingress.From, r.Ingress.Ports
	case r.Egress != nil:
		ps, pt = r.Egress.To, r.Egress.Ports
	}
	for _, p := range ps {
		peers = append(peers, describePeer(p))
	}
	for _, p := range pt {
		ports = append(ports, describePort(p))
	}
	return peers, ports
}
