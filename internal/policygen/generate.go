package policygen

import (
	"sort"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/imkerbos/Distill/internal/baseline"
	"github.com/imkerbos/Distill/internal/replay"
	"github.com/imkerbos/Distill/internal/risk"
	"github.com/imkerbos/Distill/internal/snapshot"
)

// nsNameLabel 是 Kubernetes 自动给每个 namespace 打上的名称标签。
const nsNameLabel = "kubernetes.io/metadata.name"

// Input 是一次生成所需的全部输入。
type Input struct {
	// ClusterID 是生成策略的目标集群。
	ClusterID string
	// Namespace 限定只生成该 namespace 的策略；为空表示全集群。
	Namespace string
	// Assets 是该集群的资产快照，Baseline 推导的依据。
	Assets snapshot.Assets
	// Namespaces 是该集群的命名空间快照。
	Namespaces []replay.NamespaceRef
	// Observations 是带判定结果的观测流量。
	Observations []Observation
}

// Result 是一次生成的全部产物。
//
// 三块必须一起返回：只报 Policies 而不报 MissingBaselines 与
// Ungeneratable，等于宣称覆盖完整。
type Result struct {
	// Policies 是候选策略，按 (namespace, workload) 确定排序。
	Policies []CandidatePolicy `json:"policies"`
	// MissingBaselines 是本次涉及的 namespace 中尚未齐备的 Baseline 类型。
	MissingBaselines []MissingBaseline `json:"missingBaselines"`
	// Ungeneratable 是无法表达为规则的流量。
	Ungeneratable []UngeneratableItem `json:"ungeneratable"`
}

// MissingBaseline 是一个 namespace 缺失的 Baseline 类型。
type MissingBaseline struct {
	// Namespace 是缺失所在的命名空间。
	Namespace string `json:"namespace"`
	// Kinds 是缺失的类型，按登记顺序。
	Kinds []baseline.Kind `json:"kinds"`
}

// Generate 从流量证据与快照生成候选策略集。纯函数。
func Generate(in Input) Result {
	counts := map[aggKey]int{}
	var bad []UngeneratableItem

	for _, o := range in.Observations {
		items, gaps := classify(o, in.ClusterID)
		for _, it := range items {
			if in.Namespace != "" && it.key.SubjectNS != in.Namespace {
				continue
			}
			counts[it.key]++
		}
		// 不可生成项按 flow 收集，不受 namespace 过滤影响：一条表达不了的
		// 流量在哪个 namespace 视图下都同样表达不了，按视图裁剪会让这个
		// 缺口随筛选条件时隐时现。
		bad = append(bad, gaps...)
	}

	byWorkload := map[subject][]Rule{}
	for k, n := range counts {
		byWorkload[subject{namespace: k.SubjectNS, workload: k.Subject}] = append(
			byWorkload[subject{namespace: k.SubjectNS, workload: k.Subject}],
			learnedRule(k, n),
		)
	}

	// Baseline 无条件追加，永不参与去重、永不被学习结果覆盖（spec §7.1）。
	// 即使某个 namespace 一条学习规则都没有，只要它有 workload 就得有 Baseline。
	nsWithWorkload := map[string]bool{}
	for s := range byWorkload {
		nsWithWorkload[s.namespace] = true
	}
	baselineByNS := map[string][]Rule{}
	for ns := range nsWithWorkload {
		set := baseline.Derive(in.Assets, ns)
		for _, br := range set.Rules {
			baselineByNS[ns] = append(baselineByNS[ns], baselineRule(br))
		}
	}

	res := Result{Ungeneratable: dedupeGaps(bad)}
	for s, learned := range byWorkload {
		rules := append([]Rule{}, learned...)
		rules = append(rules, baselineByNS[s.namespace]...)
		sortRules(rules)
		res.Policies = append(res.Policies, CandidatePolicy{
			Cluster: in.ClusterID, Namespace: s.namespace, Workload: s.workload, Rules: rules,
		})
	}
	sort.Slice(res.Policies, func(i, j int) bool {
		a, b := res.Policies[i], res.Policies[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Workload < b.Workload
	})

	for ns := range nsWithWorkload {
		if missing := baseline.Derive(in.Assets, ns).Missing(); len(missing) > 0 {
			res.MissingBaselines = append(res.MissingBaselines,
				MissingBaseline{Namespace: ns, Kinds: missing})
		}
	}
	sort.Slice(res.MissingBaselines, func(i, j int) bool {
		return res.MissingBaselines[i].Namespace < res.MissingBaselines[j].Namespace
	})

	return res
}

// learnedRule 把一个聚合键变成一条学习规则。
//
// Enabled 的判定是纯函数、无参数、无阈值：只有身份可信、集群内、当前
// 放行、且端口不在风险清单里的规则才默认启用。其余全部生成、可见、
// 待人工确认 —— 学是学，推荐是推荐。
func learnedRule(k aggKey, flowCount int) Rule {
	rp, risky := risk.Lookup(k.Port)
	r := Rule{
		Origin: OriginLearned, Evidence: k.Evidence, Direction: k.Direction,
		FlowCount: flowCount,
		Enabled:   k.Evidence == EvidenceTrustedAllow && !risky,
	}
	if risky {
		copied := rp
		r.Risk = &copied
	}

	proto := corev1.Protocol(k.Protocol)
	port := intstr.FromInt32(k.Port)
	ports := []networkingv1.NetworkPolicyPort{{Protocol: &proto, Port: &port}}
	peer := peerSpec(k)

	if k.Direction == replay.DirectionEgress {
		r.Egress = &networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{peer}, Ports: ports,
		}
	} else {
		r.Ingress = &networkingv1.NetworkPolicyIngressRule{
			From: []networkingv1.NetworkPolicyPeer{peer}, Ports: ports,
		}
	}
	return r
}

// peerSpec 把聚合键里的对端表达成 NetworkPolicy peer。
func peerSpec(k aggKey) networkingv1.NetworkPolicyPeer {
	if k.PeerCIDR != "" {
		return networkingv1.NetworkPolicyPeer{
			IPBlock: &networkingv1.IPBlock{CIDR: k.PeerCIDR},
		}
	}
	return networkingv1.NetworkPolicyPeer{
		NamespaceSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{nsNameLabel: k.PeerNamespace},
		},
		PodSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{workloadLabel: k.PeerWorkload},
		},
	}
}

// baselineRule 把一条 Baseline 包装成候选策略里的规则。
//
// Baseline 恒为 Enabled：它们是必备项，不是建议项。
func baselineRule(br baseline.Rule) Rule {
	kind := br.Kind
	return Rule{
		Origin: OriginBaseline, Baseline: &kind, Derivations: br.Derivations,
		Direction: br.Direction, Enabled: true,
		Ingress: br.Ingress, Egress: br.Egress,
	}
}

// sortRules 给规则定序：Baseline 在前，其后按方向、证据、端口。
//
// 确定排序不是美观问题：同一份输入两次生成必须逐字节相同，
// 否则产物 diff 全是噪声，review 随之失效。
func sortRules(rules []Rule) {
	sort.SliceStable(rules, func(i, j int) bool {
		a, b := rules[i], rules[j]
		if (a.Origin == OriginBaseline) != (b.Origin == OriginBaseline) {
			return a.Origin == OriginBaseline
		}
		if a.Direction != b.Direction {
			return a.Direction < b.Direction
		}
		if a.Evidence != b.Evidence {
			return a.Evidence < b.Evidence
		}
		return rulePort(a) < rulePort(b)
	})
}

// rulePort 取规则的第一个端口，供排序使用；无端口或命名端口时返回 0。
//
// 直接读 IntVal 而非走 IntValue()：后者对命名端口会做字符串转数字，
// 排序不需要这一步，而 int -> int32 的强转会触发 gosec 溢出告警。
// IntOrString.IntVal 本身就是 int32，端口号构造时也从未越界过。
func rulePort(r Rule) int32 {
	var ports []networkingv1.NetworkPolicyPort
	switch {
	case r.Ingress != nil:
		ports = r.Ingress.Ports
	case r.Egress != nil:
		ports = r.Egress.Ports
	}
	if len(ports) == 0 || ports[0].Port == nil || ports[0].Port.Type != intstr.Int {
		return 0
	}
	return ports[0].Port.IntVal
}

// dedupeGaps 按 (flowID, reason) 去重并定序。
//
// 一条流量的两侧可能报出同一个原因，界面上重复列出会把缺口的规模
// 显示成实际的两倍。
func dedupeGaps(in []UngeneratableItem) []UngeneratableItem {
	type gapKey struct {
		flowID string
		reason UngeneratableReason
	}
	seen := map[gapKey]bool{}
	out := make([]UngeneratableItem, 0, len(in))
	for _, it := range in {
		k := gapKey{it.FlowID, it.Reason}
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Reason != out[j].Reason {
			return out[i].Reason < out[j].Reason
		}
		return out[i].FlowID < out[j].FlowID
	})
	return out
}

// EnabledPolicies 把启用的规则渲染成可供回放的 NetworkPolicy 列表。
//
// 只吐启用规则：待确认的风险规则不进入生效策略集，否则 dry-run 预测
// 出的就不是"默认推荐上线后会怎样"，而是"把所有敞口都放开后会怎样"。
//
// 每条策略固定 policyTypes 为 Ingress + Egress：规则为空即 default-deny，
// 不另造一条独立的 default-deny 策略。
func (r Result) EnabledPolicies() []networkingv1.NetworkPolicy {
	out := make([]networkingv1.NetworkPolicy, 0, len(r.Policies))
	for _, p := range r.Policies {
		np := networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: p.Namespace,
				Name:      "candidate-" + p.Workload,
			},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{
					MatchLabels: map[string]string{workloadLabel: p.Workload},
				},
				PolicyTypes: []networkingv1.PolicyType{
					networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress,
				},
			},
		}
		for _, rule := range p.Rules {
			if !rule.Enabled {
				continue
			}
			if rule.Ingress != nil {
				np.Spec.Ingress = append(np.Spec.Ingress, *rule.Ingress)
			}
			if rule.Egress != nil {
				np.Spec.Egress = append(np.Spec.Egress, *rule.Egress)
			}
		}
		out = append(out, np)
	}
	return out
}
