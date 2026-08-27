package replay

import (
	"cmp"
	"fmt"
	"net/netip"
	"slices"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	npav1 "sigs.k8s.io/network-policy-api/apis/v1alpha1"
)

// 管理面策略（AdminNetworkPolicy 一族）的求值。
//
// 与标准 NetworkPolicy 的关系是**有序短路**，不是叠加（CRD 里 action 字段
// 的原文）：
//
//	1. ANP 按 priority 升序、同一策略内按规则顺序，第一条命中的规则定局 ——
//	   Allow 直接放行，Deny 直接阻断，Pass 跳过剩余 ANP 规则、交给下一段；
//	2. NetworkPolicy 照常求值（本文件不碰）；
//	3. **只有主体没被任何 NetworkPolicy 选中时**才轮到 BANP，它只有
//	   Allow / Deny 两种动作，都不命中则放行。
//
// Pass 与「一条 ANP 都没命中」在第 2、3 段上表现完全一样 —— 前者只是让
// 剩下的 ANP 规则不再被看。**这一点先前想错过**：以为 Pass 会连 BANP 一起
// 跳过。按错的那个版本实现，一个本该被 BANP 兜底 deny 的连接会被判成放行。
// 依据是 CRD 里 action 的原文：「pass execution to any NetworkPolicies that
// select the pod. If the pod is not selected by any NetworkPolicies then
// execution is passed to any BaselineAdminNetworkPolicies」。

// adminOutcome 是 ANP 那一段的结论。
type adminOutcome int

const (
	// adminNoMatch 表示没有任何 ANP 规则命中，继续走 NetworkPolicy。
	adminNoMatch adminOutcome = iota
	// adminAllow 是终局放行：ANP 的 Allow 压过 NetworkPolicy。
	adminAllow
	// adminDeny 是终局阻断。
	adminDeny
	// adminPass 表示交给 NetworkPolicy，且不再看剩余的 ANP 规则。
	adminPass
	// adminUnknown 表示这一段答不出来，整条判定必须是 UNKNOWN。
	adminUnknown
)

// WithAdminPolicies 交给求值器一组管理面策略。
//
// **不传就等于这个集群没有管理面策略**，求值退回到只有 NetworkPolicy 的
// 老路径，逐字节等价（golden_test 覆盖这一点）。是否该传由调用方按集群的
// EnforcedPlanes 声明决定 —— 装了 CRD 不等于 CNI 执行它，实测 Cilium 1.19
// 完全不实现 ANP，把那种集群上的 ANP 当真会让平台以为某条连接被拦着。
//
// banp 传 nil 表示这个集群没有 BaselineAdminNetworkPolicy。它是集群级单例。
func WithAdminPolicies(anp []npav1.AdminNetworkPolicy, banp *npav1.BaselineAdminNetworkPolicy) Option {
	return func(e *Evaluator) {
		e.adminPolicies = slices.Clone(anp)
		// 按 priority 升序排一次，之后求值不再关心传入顺序。同优先级
		// 再按名字排 —— 不是为了定出胜负（那由 sameAdminPriority 判成
		// UNKNOWN），而是让「哪一条被先看到」不随切片顺序变化，否则同一份
		// 策略集换个顺序就换一个 Detail。
		slices.SortStableFunc(e.adminPolicies, func(a, b npav1.AdminNetworkPolicy) int {
			if v := cmp.Compare(a.Spec.Priority, b.Spec.Priority); v != 0 {
				return v
			}
			return cmp.Compare(a.Name, b.Name)
		})
		e.baselinePolicy = banp
	}
}

// adminRule 是 ingress 与 egress 规则的统一视图，与 rule 同一取舍：
// 两个方向共用一条求值路径，避免各写一份导致语义漂移。
type adminRule struct {
	name   string
	action npav1.AdminNetworkPolicyRuleAction
	peers  []adminPeer
	ports  []npav1.AdminNetworkPolicyPort
}

// adminPeer 把 ingress 与 egress 两种 peer 摊平成一个形状。
//
// egress 独有的两项（networks、nodes）在 ingress 里恒为空，而不是分成两个
// 类型：分开会让 peer 匹配也分成两份，那正是双向语义漂移的入口。
type adminPeer struct {
	namespaces *metav1.LabelSelector
	pods       *npav1.NamespacedPod
	networks   []npav1.CIDR
	nodes      *metav1.LabelSelector
}

// adminRulesOf 提取一条 ANP 在指定方向上的规则。
func adminRulesOf(p npav1.AdminNetworkPolicy, dir Direction) []adminRule {
	if dir == DirectionIngress {
		out := make([]adminRule, 0, len(p.Spec.Ingress))
		for _, r := range p.Spec.Ingress {
			peers := make([]adminPeer, 0, len(r.From))
			for _, f := range r.From {
				peers = append(peers, adminPeer{namespaces: f.Namespaces, pods: f.Pods})
			}
			out = append(out, adminRule{name: r.Name, action: r.Action, peers: peers, ports: derefPorts(r.Ports)})
		}
		return out
	}
	out := make([]adminRule, 0, len(p.Spec.Egress))
	for _, r := range p.Spec.Egress {
		peers := make([]adminPeer, 0, len(r.To))
		for _, t := range r.To {
			peers = append(peers, adminPeer{
				namespaces: t.Namespaces, pods: t.Pods, networks: t.Networks, nodes: t.Nodes,
			})
		}
		out = append(out, adminRule{name: r.Name, action: r.Action, peers: peers, ports: derefPorts(r.Ports)})
	}
	return out
}

// baselineRulesOf 提取 BANP 在指定方向上的规则。
//
// BANP 只有 Allow / Deny，没有 Pass，也没有 priority —— 它是最后一段兜底。
func baselineRulesOf(p npav1.BaselineAdminNetworkPolicy, dir Direction) []adminRule {
	if dir == DirectionIngress {
		out := make([]adminRule, 0, len(p.Spec.Ingress))
		for _, r := range p.Spec.Ingress {
			peers := make([]adminPeer, 0, len(r.From))
			for _, f := range r.From {
				peers = append(peers, adminPeer{namespaces: f.Namespaces, pods: f.Pods})
			}
			out = append(out, adminRule{
				name: r.Name, action: npav1.AdminNetworkPolicyRuleAction(r.Action),
				peers: peers, ports: derefPorts(r.Ports),
			})
		}
		return out
	}
	out := make([]adminRule, 0, len(p.Spec.Egress))
	for _, r := range p.Spec.Egress {
		peers := make([]adminPeer, 0, len(r.To))
		for _, t := range r.To {
			peers = append(peers, adminPeer{
				namespaces: t.Namespaces, pods: t.Pods, networks: t.Networks, nodes: t.Nodes,
			})
		}
		out = append(out, adminRule{
			name: r.Name, action: npav1.AdminNetworkPolicyRuleAction(r.Action),
			peers: peers, ports: derefPorts(r.Ports),
		})
	}
	return out
}

// derefPorts 把可选的端口列表摊成切片；未设置表示"任意端口"。
func derefPorts(p *[]npav1.AdminNetworkPolicyPort) []npav1.AdminNetworkPolicyPort {
	if p == nil {
		return nil
	}
	return *p
}

// evaluateAdmin 走 ANP 那一段。
//
// 返回 adminUnknown 时，整条判定必须是 UNKNOWN：这一段答不出来，就不知道
// 后面两段该不该被执行 —— 一条被 ANP Deny 的连接与一条走到 NetworkPolicy
// 的连接，结论可以完全相反。
func (e *Evaluator) evaluateAdmin(
	subject PodRef, peer Endpoint, f Flow, dir Direction,
) (adminOutcome, Reason, UnknownReason) {
	reason := NewReason(dir)
	if len(e.adminPolicies) == 0 {
		return adminNoMatch, reason, ReasonNone
	}

	// 同优先级且都选中这个主体：集群的行为按 API 定义就是未定义的
	// （CRD 原文：「The behavior is undefined if two ANP objects have same
	// priority」）。平台不能替它挑一个 —— 挑中的那一半时间会给出一个自信的
	// 错答案。只在两条都真的选中主体时才升级，不因为集群里存在同优先级
	// 的策略就整片降级。
	if name, dup, err := e.duplicateAdminPriority(subject); err != nil {
		reason.Detail = err.Error()
		return adminUnknown, reason, ReasonPolicyMalformed
	} else if dup {
		reason.Detail = "two AdminNetworkPolicies share priority " + name
		return adminUnknown, reason, ReasonAdminPriorityAmbiguous
	}

	unresolved := ReasonNone
	for _, p := range e.adminPolicies {
		selected, err := e.adminSubjectSelects(p.Spec.Subject, subject)
		if err != nil {
			reason.Detail = fmt.Sprintf("adminnetworkpolicy %s subject: %s", p.Name, err)
			return adminUnknown, reason, ReasonPolicyMalformed
		}
		if !selected {
			continue
		}
		for idx, r := range adminRulesOf(p, dir) {
			matched, code, err := e.adminRuleMatches(r, peer, f)
			if err != nil {
				reason.Detail = fmt.Sprintf("adminnetworkpolicy %s rule %d: %s", p.Name, idx, err)
				return adminUnknown, reason, ReasonPolicyMalformed
			}
			if code != ReasonNone && !matched {
				// 与 NetworkPolicy 那一段相反：这里**不能**把不可判定的规则
				// 当成"没命中"接着往下看。ANP 是有序短路的，跳过一条可能命中
				// 的 Deny，后面任何一条 Allow 都会变成终局结论 —— 方向恰好是
				// 把一条其实被拦住的连接判成放行。
				reason.MatchedPolicy = adminPolicyRef(p.Name)
				reason.MatchedRuleIdx = idx
				return adminUnknown, reason, escalate(unresolved, code)
			}
			if !matched {
				continue
			}
			reason.MatchedPolicy = adminPolicyRef(p.Name)
			reason.MatchedRuleIdx = idx
			reason.Detail = adminRuleDetail(p.Name, r)
			switch r.action {
			case npav1.AdminNetworkPolicyRuleActionAllow:
				return adminAllow, reason, ReasonNone
			case npav1.AdminNetworkPolicyRuleActionDeny:
				return adminDeny, reason, ReasonNone
			case npav1.AdminNetworkPolicyRuleActionPass:
				return adminPass, reason, ReasonNone
			default:
				// 认不出的动作不能当作"不命中"跳过：那等于把一条 Deny
				// 读成放行。
				reason.Detail = fmt.Sprintf(
					"adminnetworkpolicy %s rule %d: unknown action %q", p.Name, idx, r.action)
				return adminUnknown, reason, ReasonPolicyMalformed
			}
		}
	}
	return adminNoMatch, NewReason(dir), ReasonNone
}

// evaluateBaseline 走 BANP 那一段。**只在主体未被任何 NetworkPolicy 选中时调用。**
func (e *Evaluator) evaluateBaseline(
	subject PodRef, peer Endpoint, f Flow, dir Direction,
) (adminOutcome, Reason, UnknownReason) {
	reason := NewReason(dir)
	if e.baselinePolicy == nil {
		return adminNoMatch, reason, ReasonNone
	}
	p := *e.baselinePolicy
	selected, err := e.adminSubjectSelects(p.Spec.Subject, subject)
	if err != nil {
		reason.Detail = fmt.Sprintf("baselineadminnetworkpolicy %s subject: %s", p.Name, err)
		return adminUnknown, reason, ReasonPolicyMalformed
	}
	if !selected {
		return adminNoMatch, reason, ReasonNone
	}
	for idx, r := range baselineRulesOf(p, dir) {
		matched, code, err := e.adminRuleMatches(r, peer, f)
		if err != nil {
			reason.Detail = fmt.Sprintf("baselineadminnetworkpolicy %s rule %d: %s", p.Name, idx, err)
			return adminUnknown, reason, ReasonPolicyMalformed
		}
		if code != ReasonNone && !matched {
			reason.MatchedPolicy = baselinePolicyRef(p.Name)
			reason.MatchedRuleIdx = idx
			return adminUnknown, reason, code
		}
		if !matched {
			continue
		}
		reason.MatchedPolicy = baselinePolicyRef(p.Name)
		reason.MatchedRuleIdx = idx
		reason.Detail = baselineRuleDetail(p.Name, r)
		switch r.action {
		case npav1.AdminNetworkPolicyRuleActionAllow:
			return adminAllow, reason, ReasonNone
		case npav1.AdminNetworkPolicyRuleActionDeny:
			return adminDeny, reason, ReasonNone
		default:
			// BANP 没有 Pass：出现它说明这份对象不是我们以为的那个形状。
			reason.Detail = fmt.Sprintf(
				"baselineadminnetworkpolicy %s rule %d: unknown action %q", p.Name, idx, r.action)
			return adminUnknown, reason, ReasonPolicyMalformed
		}
	}
	return adminNoMatch, NewReason(dir), ReasonNone
}

// duplicateAdminPriority 判断是否有两条同优先级的 ANP 同时选中这个主体。
func (e *Evaluator) duplicateAdminPriority(subject PodRef) (string, bool, error) {
	seen := map[int32]bool{}
	for _, p := range e.adminPolicies {
		selected, err := e.adminSubjectSelects(p.Spec.Subject, subject)
		if err != nil {
			return "", false, fmt.Errorf("adminnetworkpolicy %s subject: %w", p.Name, err)
		}
		if !selected {
			continue
		}
		if seen[p.Spec.Priority] {
			return fmt.Sprintf("%d", p.Spec.Priority), true, nil
		}
		seen[p.Spec.Priority] = true
	}
	return "", false, nil
}

// adminSubjectSelects 判断一条管理面策略是否选中这个 Pod。
//
// subject 恰好设置 namespaces 与 pods 之一（CRD 的 MaxProperties=1）。
// 两个都空或两个都给，说明这份对象不是我们以为的形状 —— 报错，不猜。
func (e *Evaluator) adminSubjectSelects(s npav1.AdminNetworkPolicySubject, pod PodRef) (bool, error) {
	switch {
	case s.Namespaces != nil && s.Pods != nil:
		return false, fmt.Errorf("subject sets both namespaces and pods; exactly one is allowed")
	case s.Namespaces != nil:
		ns, ok := e.namespaces[pod.Namespace]
		if !ok {
			// 命名空间标签缺失时不能当成"没选中"：那会让一条 Deny 静默消失。
			return false, fmt.Errorf("namespace %q is not in the snapshot", pod.Namespace)
		}
		return selectorMatches(s.Namespaces, ns.Labels)
	case s.Pods != nil:
		ns, ok := e.namespaces[pod.Namespace]
		if !ok {
			return false, fmt.Errorf("namespace %q is not in the snapshot", pod.Namespace)
		}
		nsOK, err := selectorMatchesValue(s.Pods.NamespaceSelector, ns.Labels)
		if err != nil || !nsOK {
			return false, err
		}
		return selectorMatchesValue(s.Pods.PodSelector, pod.Labels)
	default:
		return false, fmt.Errorf("subject sets neither namespaces nor pods")
	}
}

// selectorMatchesValue 是值类型 LabelSelector 的匹配。
//
// 与 selectorMatches 分开：那个把 nil 当作"字段没设"返回 false，而这里的
// 选择器是值类型 —— 空对象的含义是**选中全部**，不是"没设"。混用会让一条
// 选中全命名空间的 ANP 变成谁也选不中。
func selectorMatchesValue(sel metav1.LabelSelector, lbls map[string]string) (bool, error) {
	return selectorMatches(&sel, lbls)
}

// adminRuleMatches 判断一条管理面规则是否命中这条流量。
//
// 空 peer 列表在 ANP 里是**非法**的（CRD 要求 MinItems=1），与 NetworkPolicy
// 的"空表示任意对端"相反 —— 照 NetworkPolicy 的读法会把一条不完整的规则
// 当成匹配一切，而它的动作可能是 Deny。
func (e *Evaluator) adminRuleMatches(r adminRule, peer Endpoint, f Flow) (bool, UnknownReason, error) {
	portOK, portReason := adminPortMatches(r.ports, f.Protocol, f.Port, f.Dest.Pod)
	if !portOK {
		return false, portReason, nil
	}
	if len(r.peers) == 0 {
		return false, ReasonNone, fmt.Errorf("rule has no peers; the API requires at least one")
	}

	unresolved := ReasonNone
	for _, p := range r.peers {
		matched, code, err := e.adminPeerMatches(p, peer)
		if err != nil {
			return false, unresolved, err
		}
		if matched {
			return true, ReasonNone, nil
		}
		unresolved = escalate(unresolved, code)
	}
	return false, unresolved, nil
}

// adminPeerMatches 判断一个对端是否落在这个 peer 里。
func (e *Evaluator) adminPeerMatches(p adminPeer, ep Endpoint) (bool, UnknownReason, error) {
	switch {
	case p.nodes != nil:
		// 平台没有采集节点标签，这个 peer 算不出来。**不能当成"不匹配"**：
		// 一条 `to: [{nodes: ...}]` 的 Deny 会就此消失，而它拦的正是到节点的
		// 流量。要支持它得先采 Node 标签，那是另一件事。
		return false, ReasonAdminPolicyUnsupported, nil
	case len(p.networks) > 0:
		return adminNetworksMatch(p.networks, ep.IP)
	case p.namespaces != nil || p.pods != nil:
		return e.adminSelectorPeerMatches(p, ep)
	default:
		return false, ReasonNone, fmt.Errorf("peer sets none of namespaces, pods, networks or nodes")
	}
}

// adminSelectorPeerMatches 处理 namespaces / pods 两种按标签选的 peer。
func (e *Evaluator) adminSelectorPeerMatches(p adminPeer, ep Endpoint) (bool, UnknownReason, error) {
	if ep.Pod == nil {
		if ep.ClusterID == e.clusterID {
			// 本集群内但身份未还原：算不出来，不是"不匹配"。
			return false, ReasonSnapshotMissing, nil
		}
		// 外部地址或别的集群：按标签选的 peer 本就选不中它们，
		// 这是一个确定的"不匹配"，不是数据不足。
		return false, ReasonNone, nil
	}
	pod := ep.Pod
	if pod.ClusterID != e.clusterID {
		return false, ReasonNone, nil
	}
	if pod.HostNetwork {
		// CRD 原文：「host-networked pods are not included in this type of
		// peer」。按标签选中它是错的。
		return false, ReasonNone, nil
	}
	ns, ok := e.namespaces[pod.Namespace]
	if !ok {
		return false, ReasonSnapshotMissing, nil
	}
	if p.namespaces != nil {
		matched, err := selectorMatches(p.namespaces, ns.Labels)
		return matched, ReasonNone, err
	}
	nsOK, err := selectorMatchesValue(p.pods.NamespaceSelector, ns.Labels)
	if err != nil || !nsOK {
		return false, ReasonNone, err
	}
	matched, err := selectorMatchesValue(p.pods.PodSelector, pod.Labels)
	return matched, ReasonNone, err
}

// adminNetworksMatch 判断地址是否落在任一网段里。
func adminNetworksMatch(networks []npav1.CIDR, ip string) (bool, UnknownReason, error) {
	addr, ok := parseEndpointAddr(ip)
	if !ok {
		// 地址解析不出来时不能当成"不匹配"：那会让一条按网段写的 Deny
		// 静默消失。
		return false, ReasonSnapshotMissing, nil
	}
	for _, c := range networks {
		prefix, err := netip.ParsePrefix(string(c))
		if err != nil {
			return false, ReasonNone, fmt.Errorf("invalid CIDR %q: %w", c, err)
		}
		// 版本不同的地址一律不匹配，而不是报错：双栈集群里一条 IPv4 网段
		// 与一个 IPv6 地址并列是常态。
		if prefix.Addr().Is4() != addr.Is4() {
			continue
		}
		if prefix.Contains(addr) {
			return true, ReasonNone, nil
		}
	}
	return false, ReasonNone, nil
}

// adminPortMatches 判断端口是否落在规则声明的端口里。
//
// 与 NetworkPolicy 的 portMatches 同一形状但不能共用：ANP 的端口是三选一的
// 结构（portNumber / portRange / namedPort），协议是必填字段而不是可空指针。
func adminPortMatches(
	ports []npav1.AdminNetworkPolicyPort, proto Protocol, port int32, destPod *PodRef,
) (bool, UnknownReason) {
	if len(ports) == 0 {
		return true, ReasonNone
	}
	unresolved := ReasonNone
	for _, p := range ports {
		switch {
		case p.PortNumber != nil:
			if adminProtocol(p.PortNumber.Protocol) != proto {
				continue
			}
			if p.PortNumber.Port == port {
				return true, ReasonNone
			}
		case p.PortRange != nil:
			if adminProtocol(p.PortRange.Protocol) != proto {
				continue
			}
			if port >= p.PortRange.Start && port <= p.PortRange.End {
				return true, ReasonNone
			}
		case p.NamedPort != nil:
			// 命名端口解析在目的侧那个 Pod 上，与 NetworkPolicy 同一处理：
			// 端口说的始终是流量目的地的端口。
			resolved, ok := resolveNamedPort(destPod, *p.NamedPort, proto)
			if !ok {
				unresolved = escalate(unresolved, ReasonNamedPortUnresolved)
				continue
			}
			if resolved == port {
				return true, ReasonNone
			}
		}
	}
	return false, unresolved
}

// adminProtocol 把 API 的协议翻成本层的协议。
func adminProtocol(p corev1.Protocol) Protocol {
	switch p {
	case corev1.ProtocolUDP:
		return ProtocolUDP
	case corev1.ProtocolSCTP:
		return ProtocolSCTP
	default:
		return ProtocolTCP
	}
}

// adminPolicyRef / baselinePolicyRef 给命中的策略名加上种类前缀。
//
// 加前缀是必需的：这一栏同时装着 NetworkPolicy 的 `namespace/name`，
// 而 ANP 是集群级对象、没有 namespace。不区分的话，界面上一条 ANP 的
// 命中会读起来像一条 default 命名空间里的 NetworkPolicy。
func adminPolicyRef(name string) string    { return "anp/" + name }
func baselinePolicyRef(name string) string { return "banp/" + name }

// adminRuleDetail / baselineRuleDetail 给出可读的命中说明。
func adminRuleDetail(policy string, r adminRule) string {
	return fmt.Sprintf("AdminNetworkPolicy %s rule %q action %s", policy, r.name, r.action)
}

func baselineRuleDetail(policy string, r adminRule) string {
	return fmt.Sprintf("BaselineAdminNetworkPolicy %s rule %q action %s", policy, r.name, r.action)
}
