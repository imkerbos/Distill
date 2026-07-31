package replay

import (
	networkingv1 "k8s.io/api/networking/v1"
)

// Evaluator 对单个集群的策略集求值。
//
// 每个集群一个实例：NetworkPolicy 是集群本地对象，跨集群策略集混用
// 会让本集群策略"选中"其他集群的 Pod。
type Evaluator struct {
	clusterID  string
	policies   []networkingv1.NetworkPolicy
	namespaces map[string]NamespaceRef
}

// NewEvaluator 构造针对指定集群的求值器。
func NewEvaluator(clusterID string, policies []networkingv1.NetworkPolicy, namespaces []NamespaceRef) *Evaluator {
	idx := make(map[string]NamespaceRef, len(namespaces))
	for _, ns := range namespaces {
		if ns.ClusterID == clusterID {
			idx[ns.Name] = ns
		}
	}
	return &Evaluator{clusterID: clusterID, policies: policies, namespaces: idx}
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
	d := Decision{Verdict: VerdictAllow, Confidence: ConfidenceTrusted}
	d.Reason.MatchedRuleIdx = -1

	if src := e.localPod(f.Source); src != nil {
		out := e.evaluateSide(*src, f.Dest, f, DirectionEgress)
		if out.Verdict != VerdictAllow {
			return out
		}
		// 出向放行仍要保留其 Reason（如匹配到的规则），否则最终结论会
		// 丢失"为什么放行"，解释器就只能展示一个空理由。
		d = out
	}
	if dst := e.localPod(f.Dest); dst != nil {
		in := e.evaluateSide(*dst, f.Source, f, DirectionIngress)
		if in.Verdict != VerdictAllow {
			return in
		}
		d = in
	}
	return d
}

// localPod 返回该端点在本集群内的 Pod；不属于本集群时返回 nil。
//
// 本集群的 NetworkPolicy 无法选中其他集群的 Pod，对端只能通过
// ipBlock 匹配其 IP。
func (e *Evaluator) localPod(endpoint Endpoint) *PodRef {
	if endpoint.Pod == nil || endpoint.Pod.ClusterID != e.clusterID {
		return nil
	}
	return endpoint.Pod
}

// evaluateSide 判定单个方向：subject 是被策略选中的主体，peer 是对端。
func (e *Evaluator) evaluateSide(subject PodRef, peer Endpoint, f Flow, dir Direction) Decision {
	reason := NewReason(dir)

	isIsolated, err := isolated(e.policies, e.clusterID, subject, dir)
	if err != nil {
		reason.Detail = err.Error()
		return Decision{Verdict: VerdictUnknown, Confidence: ConfidenceTrusted,
			Reason: reason, UnknownReason: ReasonSnapshotMissing}
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
		if err != nil || !selected {
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
	for _, p := range r.peers {
		matched, err := peerMatches(p, policyNamespace, peer, e.namespaces)
		if err != nil {
			return false, ReasonNone, err
		}
		if matched {
			return true, ReasonNone, nil
		}
	}
	return false, ReasonNone, nil
}
