package policygen

import (
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// anySelector 是 selector 为空（选中全部）时的展示写法。
const anySelector = "*"

// describe 用规则体填充 Rule 的对端与端口展示视图。
//
// 原始的 networkingv1 结构不出 API：把 NetworkPolicyPeer 原样抛给界面，
// 等于要求界面自己解释 selector 语义，两边迟早各解释一套。但只给出
// "4 条启用规则 · LEARNED · EGRESS" 同样不成立 —— 读的人分不出
// payment:8080 与 0.0.0.0/0:443，而这是两件性质完全不同的事。
// 因此在生成侧渲染成字符串，与规则同源返回。
func (r *Rule) describe() {
	var peers []networkingv1.NetworkPolicyPeer
	var ports []networkingv1.NetworkPolicyPort
	switch {
	case r.Ingress != nil:
		peers, ports = r.Ingress.From, r.Ingress.Ports
	case r.Egress != nil:
		peers, ports = r.Egress.To, r.Egress.Ports
	}

	// 一律初始化为空切片而非 nil：JSON 里 [] 与 null 在界面上是
	// "这条规则没有对端限制" 与 "后端没给这个字段" 两种意思。
	r.Peers = make([]string, 0, len(peers))
	for _, p := range peers {
		r.Peers = append(r.Peers, describePeer(p))
	}
	r.Ports = make([]string, 0, len(ports))
	for _, p := range ports {
		r.Ports = append(r.Ports, describePort(p))
	}
}

// describePeer 把一个 peer 渲染成 namespace/workload 或 CIDR。
func describePeer(p networkingv1.NetworkPolicyPeer) string {
	if p.IPBlock != nil {
		if len(p.IPBlock.Except) > 0 {
			// except 必须一并显示：一条 0.0.0.0/0 与一条挖掉了内网段的
			// 0.0.0.0/0 敞口完全不同，省掉 except 会把后者读成前者。
			return p.IPBlock.CIDR + " except " + strings.Join(p.IPBlock.Except, ",")
		}
		return p.IPBlock.CIDR
	}
	ns := describeSelector(p.NamespaceSelector, nsNameLabel)
	pod := describeSelector(p.PodSelector, workloadLabel)
	return ns + "/" + pod
}

// describeSelector 渲染一个 label selector。
//
// 命中约定标签（namespace 的名称标签、Pod 的 app 标签）时只显示取值，
// 那是绝大多数规则的形态；其余情况把 matchLabels 按键排序后原样列出，
// 不做简化 —— 一个被简化掉的标签会让两条选中范围不同的规则看起来一样。
func describeSelector(sel *metav1.LabelSelector, shorthand string) string {
	if sel == nil || (len(sel.MatchLabels) == 0 && len(sel.MatchExpressions) == 0) {
		return anySelector
	}
	if v, ok := sel.MatchLabels[shorthand]; ok && len(sel.MatchLabels) == 1 &&
		len(sel.MatchExpressions) == 0 {
		return v
	}
	parts := make([]string, 0, len(sel.MatchLabels))
	for k, v := range sel.MatchLabels {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	if len(sel.MatchExpressions) > 0 {
		parts = append(parts, "matchExpressions")
	}
	return strings.Join(parts, ",")
}

// describePort 把一个端口渲染成 TCP/8080；端口范围渲染成 TCP/8000-9000。
//
// 协议缺省按 Kubernetes 语义补 TCP，不留空：留空会被读成"任意协议"，
// 而它实际只放行 TCP。
func describePort(p networkingv1.NetworkPolicyPort) string {
	proto := string(corev1.ProtocolTCP)
	if p.Protocol != nil {
		proto = string(*p.Protocol)
	}
	if p.Port == nil {
		return proto + "/" + anySelector
	}
	out := proto + "/" + p.Port.String()
	if p.EndPort != nil {
		out += "-" + strconv.FormatInt(int64(*p.EndPort), 10)
	}
	return out
}
