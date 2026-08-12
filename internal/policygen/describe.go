package policygen

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
	r.Fingerprint = FingerprintOf(*r)
}

// FingerprintOf 计算一条规则的内容指纹。
//
// 导出是为了让测试能对手工构造的规则求指纹 —— 「改了内容指纹必须变」
// 这条性质只有对着两份仅差一个字段的规则才验证得了。
//
// 对端与端口取自规则体（Ingress/Egress），**不取** Peers/Ports 那两个
// 展示串：展示串是写给人看的，为此做了简化 —— describeSelector 命中
// workloadLabelKeys 时只显示标签值，于是 {app: foo} 与 {k8s-app: foo}
// 都渲染成 ns/foo，MatchExpressions 更是整块塌缩成字面量
// "matchExpressions"。让身份挂在这种有损渲染上，两条选中范围完全不同
// 的规则会得到同一个指纹、同一行 rule_override，人工确认只对其中一条
// 生效，而界面上那两行长得一模一样 —— 操作者看到的是「我确认了，它没反应」。
// 简化是展示层的自由，一旦它同时是身份，展示层就再也不能改。
//
// 序列化整个规则体而不是逐字段手写：手写的那份必须随 NetworkPolicy 类型
// 演进同步维护，漏掉一个字段的后果是两条不同的规则碰撞成一个指纹 ——
// 朝着不安全的方向失败，且不报错。json.Marshal 对 map 键排序，因此结果
// 是确定的（同一份输入两次生成逐字节相同这条性质依赖于此）。
//
// 各字段之间用 \x00 分隔而非直接拼接：直接拼接会让 ["a","bc"] 与
// ["ab","c"] 得到同一个指纹。
func FingerprintOf(r Rule) string {
	h := sha256.New()
	write := func(parts ...string) {
		for _, p := range parts {
			_, _ = h.Write([]byte(p))
			_, _ = h.Write([]byte{0})
		}
	}
	write(string(r.Origin), string(r.Evidence), string(r.Direction))
	if r.Baseline != nil {
		write(string(*r.Baseline))
	} else {
		write("")
	}
	write(ruleBody(r))
	return hex.EncodeToString(h.Sum(nil))
}

// ruleBody 把规则体的对端与端口序列化成用于指纹的规范形式。
//
// FlowCount 与 Enabled 都不在规则体里，因此天然不会进指纹：前者每天
// 都在变，后者恰恰是 Apply 要改的字段 —— 把它算进去会让一条规则在被
// 确认的那一刻换掉指纹，覆盖立刻指向不存在的规则。
func ruleBody(r Rule) string {
	var body struct {
		Peers []networkingv1.NetworkPolicyPeer `json:"peers"`
		Ports []networkingv1.NetworkPolicyPort `json:"ports"`
	}
	switch {
	case r.Ingress != nil:
		body.Peers, body.Ports = r.Ingress.From, r.Ingress.Ports
	case r.Egress != nil:
		body.Peers, body.Ports = r.Egress.To, r.Egress.Ports
	}
	b, err := json.Marshal(body)
	if err != nil {
		// 走不到：这两个都是纯数据结构。真走到了也不能返回一个常量 ——
		// 那会让所有规则碰撞成同一个指纹。fmt 的 map 输出同样按键排序。
		return fmt.Sprintf("unserializable:%v:%#v", err, body)
	}
	return string(b)
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
	pod := describeSelector(p.PodSelector, workloadLabelKeys...)
	return ns + "/" + pod
}

// describeSelector 渲染一个 label selector。
//
// 命中约定标签（namespace 的名称标签，或 Pod 的 workloadLabelKeys 之一）
// 时只显示取值，那是绝大多数规则的形态；其余情况把 matchLabels 按键
// 排序后原样列出，不做简化 —— 一个被简化掉的标签会让两条选中范围不同
// 的规则看起来一样。
//
// shorthand 接受多个候选键而非单个：真实集群的 workload 归属键不止
// app 一种，podSelector 的单键 matchLabels 命中 k8s-app 或 component
// 时同样该显示成简写，而不是退化成 k8s-app=kube-dns 这种更啰嗦、
// 与其余规则形态不一致的写法。
func describeSelector(sel *metav1.LabelSelector, shorthand ...string) string {
	if sel == nil || (len(sel.MatchLabels) == 0 && len(sel.MatchExpressions) == 0) {
		return anySelector
	}
	if len(sel.MatchLabels) == 1 && len(sel.MatchExpressions) == 0 {
		for k, v := range sel.MatchLabels {
			for _, sh := range shorthand {
				if k == sh {
					return v
				}
			}
		}
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
