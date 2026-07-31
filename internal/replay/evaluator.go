package replay

import (
	networkingv1 "k8s.io/api/networking/v1"
)

// Evaluator 对单个集群的策略集求值。
//
// 每个集群一个实例：NetworkPolicy 是集群本地对象，跨集群策略集混用
// 会让本集群策略"选中"其他集群的 Pod。
type Evaluator struct {
	clusterID   string
	policies    []networkingv1.NetworkPolicy
	namespaces  map[string]NamespaceRef
	ccnpPresent bool
}

// Option 调整求值器的行为。
type Option func(*Evaluator)

// WithCCNPPresent 声明集群中存在 Cilium 策略。
//
// CCNP 具有 deny 语义，与标准 NetworkPolicy 的 additive-allow 不同：
// 存在 CCNP 时，仅基于标准策略的判定可能是错的，且是"看起来对"的错。
// 平台不管控 CCNP，但必须把这类结论降级。
func WithCCNPPresent(present bool) Option {
	return func(e *Evaluator) { e.ccnpPresent = present }
}

// NewEvaluator 构造针对指定集群的求值器。
func NewEvaluator(
	clusterID string,
	policies []networkingv1.NetworkPolicy,
	namespaces []NamespaceRef,
	opts ...Option,
) *Evaluator {
	idx := make(map[string]NamespaceRef, len(namespaces))
	for _, ns := range namespaces {
		if ns.ClusterID == clusterID {
			idx[ns.Name] = ns
		}
	}
	e := &Evaluator{clusterID: clusterID, policies: policies, namespaces: idx}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// confidenceFor 判断该连接的结论可信度。
//
// 降级不改变结论本身：DENY 仍然是 DENY，只是不得作为策略推荐的依据。
func (e *Evaluator) confidenceFor(f Flow) Confidence {
	if e.ccnpPresent {
		return ConfidenceDegraded
	}
	for _, endpoint := range []Endpoint{f.Source, f.Dest} {
		if endpoint.Pod != nil && endpoint.Pod.InMesh {
			return ConfidenceDegraded
		}
	}
	return ConfidenceTrusted
}

// rule 是 ingress 与 egress 规则的统一视图。
//
// 两个方向共用一条求值路径，避免各写一份导致语义漂移 —— 双向语义
// 不一致是这类引擎最典型的缺陷。
type rule struct {
	peers []networkingv1.NetworkPolicyPeer
	ports []networkingv1.NetworkPolicyPort
}

// rulesOf 提取策略在指定方向上的规则。
func rulesOf(p networkingv1.NetworkPolicy, dir Direction) []rule {
	if dir == DirectionIngress {
		out := make([]rule, 0, len(p.Spec.Ingress))
		for _, r := range p.Spec.Ingress {
			out = append(out, rule{peers: r.From, ports: r.Ports})
		}
		return out
	}
	out := make([]rule, 0, len(p.Spec.Egress))
	for _, r := range p.Spec.Egress {
		out = append(out, rule{peers: r.To, ports: r.Ports})
	}
	return out
}

// Evaluate 判定一条 flow。
//
// 顺序：先判出向（源侧），再判入向（目的侧）。任一方向阻断即整体阻断；
// 任一方向数据不足即整体 UNKNOWN —— 宁可承认不知道，也不给出可能
// 错误的 ALLOW，后者会变成一次漏报的策略推荐。
func (e *Evaluator) Evaluate(f Flow) Decision {
	d := Decision{Verdict: VerdictAllow, Confidence: e.confidenceFor(f)}
	d.Reason.MatchedRuleIdx = -1
	d.Reason.Unmanaged = e.hasUnmanagedEndpoint(f)
	d.CrossCluster = isCrossCluster(f)

	if src := e.localPod(f.Source); src != nil {
		out := e.evaluateSide(*src, f.Dest, f, DirectionEgress)
		if out.Verdict != VerdictAllow {
			return carryFlags(out, d)
		}
		// 出向放行仍要保留其 Reason（如匹配到的规则），否则最终结论会
		// 丢失"为什么放行"，解释器就只能展示一个空理由。
		d = carryFlags(out, d)
	}
	if dst := e.localPod(f.Dest); dst != nil {
		in := e.evaluateSide(*dst, f.Source, f, DirectionIngress)
		if in.Verdict != VerdictAllow {
			return carryFlags(in, d)
		}
		// 放行侧的 Reason 必须保留，否则最终结论会丢失"为什么放行"，
		// 解释器就只能展示一个空理由。
		d = carryFlags(in, d)
	}
	return d
}

// isCrossCluster 判断连接两端是否分属不同集群。
//
// V4 对跨集群流量只做可见性、不做 enforce：标准 NetworkPolicy 跨集群
// 只能靠 ipBlock 表达，过于粗粒度。这个标记让该敞口在指标中持续可见。
func isCrossCluster(f Flow) bool {
	a, b := f.Source.ClusterID, f.Dest.ClusterID
	return a != "" && b != "" && a != b
}

// carryFlags 把连接级别的标记（Unmanaged、CrossCluster、Confidence）从 base
// 透传到方向级判定结果 side 上，同时保留 side 自身的 Verdict/Reason/UnknownReason。
//
// 这些标记只在 Evaluate 顶层依据整条 flow 算一次；evaluateSide 看不到
// 对端的集群归属或是否处于 mesh 中，不能自己算，所以必须由调用方在每条
// 返回路径上补回去 —— 否则一个 DEGRADED 的判定会在早退路径上悄悄退回
// TRUSTED，而这正是本该被拦下的情形。
func carryFlags(side, base Decision) Decision {
	side.CrossCluster = base.CrossCluster
	side.Confidence = base.Confidence
	side.Reason.Unmanaged = base.Reason.Unmanaged
	return side
}

// localPod 返回该端点在本集群内、且受 NetworkPolicy 管控的 Pod。
//
// 返回 nil 的两种情况：
//   - 属于其他集群：本集群策略无法选中它，只能靠 ipBlock 匹配 IP
//   - hostNetwork Pod：使用 Node IP、不在 Pod 网络内，策略对其不生效
func (e *Evaluator) localPod(endpoint Endpoint) *PodRef {
	if endpoint.Pod == nil || endpoint.Pod.ClusterID != e.clusterID {
		return nil
	}
	if IsUnmanaged(*endpoint.Pod) {
		return nil
	}
	return endpoint.Pod
}

// hasUnmanagedEndpoint 判断连接是否有任一端不受 NetworkPolicy 管控。
func (e *Evaluator) hasUnmanagedEndpoint(f Flow) bool {
	for _, endpoint := range []Endpoint{f.Source, f.Dest} {
		if endpoint.Pod != nil && endpoint.Pod.ClusterID == e.clusterID && IsUnmanaged(*endpoint.Pod) {
			return true
		}
	}
	return false
}

// IsUnmanaged 判断 Pod 是否不受 NetworkPolicy 管控。
//
// hostNetwork Pod 使用宿主网络命名空间，其流量不经过 Pod 网络，
// NetworkPolicy 对其基本不生效。这类 Pod 必须排除出覆盖率统计，
// 否则"已保护比例"是虚假的 —— 而它们往往正是特权组件。
func IsUnmanaged(pod PodRef) bool {
	return pod.HostNetwork
}

// evaluateSide 判定单个方向：subject 是被策略选中的主体，peer 是对端。
func (e *Evaluator) evaluateSide(subject PodRef, peer Endpoint, f Flow, dir Direction) Decision {
	reason := NewReason(dir)

	// isolated 唯一的错误来源是 selectsPod -> selectorMatches：某条策略的
	// PodSelector 本身写错了。这不是快照缺失，是策略格式非法，必须归类为
	// ReasonPolicyMalformed，否则 UNKNOWN 构成的仪表盘会把人指向快照管线
	// 去排查一个其实是 YAML 手误的问题。
	isIsolated, err := isolated(e.policies, e.clusterID, subject, dir)
	if err != nil {
		reason.Detail = err.Error()
		return Decision{Verdict: VerdictUnknown, Confidence: ConfidenceTrusted,
			Reason: reason, UnknownReason: ReasonPolicyMalformed}
	}
	if !isIsolated {
		return Decision{Verdict: VerdictAllow, Confidence: ConfidenceTrusted, Reason: reason}
	}
	reason.Isolated = true

	unresolved := ReasonNone
	for _, p := range e.policies {
		if !policyCovers(p, dir) {
			continue
		}
		selected, err := selectsPod(p, e.clusterID, subject)
		if err != nil {
			// 策略的 PodSelector 无法解析时不能当作"没选中"跳过：那会让一条
			// 本可放行的策略凭空消失，最终输出一个可信的 DENY —— 建立在
			// 无法解析的策略之上的可信结论，是本引擎最危险的输出。
			unresolved = ReasonPolicyMalformed
			continue
		}
		if !selected {
			continue
		}

		for idx, r := range rulesOf(p, dir) {
			matched, reasonCode, err := e.ruleAllows(r, p.Namespace, peer, f)
			if err != nil {
				// 策略无法求值时不能当作"规则不匹配"跳过：那会退化成
				// 静默 false，把本该放行的流量判成 DENY。
				unresolved = ReasonPolicyMalformed
				continue
			}
			if reasonCode != ReasonNone {
				unresolved = reasonCode
			}
			if matched {
				reason.MatchedPolicy = p.Namespace + "/" + p.Name
				reason.MatchedRuleIdx = idx
				return Decision{Verdict: VerdictAllow, Confidence: ConfidenceTrusted, Reason: reason}
			}
		}
	}

	if unresolved != ReasonNone {
		return Decision{Verdict: VerdictUnknown, Confidence: ConfidenceTrusted,
			Reason: reason, UnknownReason: unresolved}
	}
	return Decision{Verdict: VerdictDeny, Confidence: ConfidenceTrusted, Reason: reason}
}

// ruleAllows 判断单条规则是否放行该流量。
//
// 空 peer 列表表示"任意对端"，空端口列表表示"任意端口"，两者都要
// 成立才算规则命中。
func (e *Evaluator) ruleAllows(r rule, policyNamespace string, peer Endpoint, f Flow) (bool, UnknownReason, error) {
	portOK, portReason := portMatches(r.ports, f.Protocol, f.Port, f.Dest.Pod)
	if !portOK {
		return false, portReason, nil
	}

	if len(r.peers) == 0 {
		return true, ReasonNone, nil
	}
	// 与 portMatches 相同的累积模式：一个不可判定的 peer（如命名空间快照
	// 缺失）不能让整条规则静默 false——但一旦后面某个 peer 真正匹配，
	// ALLOW 必须照常成立，不可判定的 peer 不能倒过来拖累一个有效放行。
	unresolved := ReasonNone
	for _, p := range r.peers {
		matched, reasonCode, err := peerMatches(p, policyNamespace, e.clusterID, peer, e.namespaces)
		if err != nil {
			return false, ReasonNone, err
		}
		if matched {
			return true, ReasonNone, nil
		}
		if reasonCode != ReasonNone {
			unresolved = reasonCode
		}
	}
	return false, unresolved, nil
}
