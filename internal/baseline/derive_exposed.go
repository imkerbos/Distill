package baseline

import (
	"net/netip"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/imkerbos/Distill/internal/replay"
	"github.com/imkerbos/Distill/internal/snapshot"
)

// targetPortOf 把 Service 端口映射成 NetworkPolicy 端口。
//
// 命名端口原样写进规则，由 CNI 对着 Pod 解析 —— intstr.FromInt32(TargetPort)
// 在命名端口下取到的是 0，而 0 是合法端口值：一条指向端口 0 的规则永远
// 匹配不上，外观却完全正常。
func targetPortOf(p snapshot.ServicePort) intstr.IntOrString {
	if p.TargetPortName != "" {
		return intstr.FromString(p.TargetPortName)
	}
	return intstr.FromInt32(p.TargetPort)
}

// deriveExposedIngress 推导指定 namespace 的暴露型入站放行 Baseline。
//
// 只看 LoadBalancer 与 NodePort：它们的入站来自集群外，学习环节要么学不出
// 稳定的对端（源地址每天在变），要么根本不产生流量（LB 尚未接入真实用户）。
// 判不出对端范围的 Service（LoadBalancer 拿不到入口地址、或入口地址判不出
// 归属）不生成规则 —— 交给 Missing() 如实报出缺口，不臆造一条看起来齐备
// 实则错误的放行。
//
// 每条规则都带 Subject（Service 的 selector）：这一类描述的是**这一个**
// Service 声明的暴露范围，不是整个 namespace 的基础设施事实，broadcast
// 给 namespace 里所有 workload 会让没有暴露对象的 workload 也拿到一条
// EXPOSED_INGRESS peers=[0.0.0.0/0]（design review C1，2026-08-28）。
func deriveExposedIngress(a snapshot.Assets, namespace string) []Rule {
	var out []Rule
	for _, svc := range a.Services {
		if svc.Namespace != namespace {
			continue
		}
		if svc.Type != serviceTypeLoadBalancer && svc.Type != serviceTypeNodePort {
			continue
		}
		if len(svc.Ports) == 0 {
			continue
		}

		peers, peerDerivations, ok := exposedPeers(a, svc)
		if !ok {
			continue
		}

		ports := make([]networkingv1.NetworkPolicyPort, 0, len(svc.Ports))
		for _, p := range svc.Ports {
			proto := corev1.Protocol(p.Protocol)
			target := targetPortOf(p)
			ports = append(ports, networkingv1.NetworkPolicyPort{Protocol: &proto, Port: &target})
		}

		derivations := append(append([]Derivation{}, peerDerivations...), Derivation{
			SourceKind: SourceService, Cluster: a.ClusterID, Namespace: svc.Namespace,
			Name: svc.Name, Field: "spec.ports.targetPort",
		})

		rule, err := NewRule(
			KindExposedIngress, replay.DirectionIngress,
			&networkingv1.NetworkPolicyIngressRule{From: peers, Ports: ports}, nil,
			derivations,
		)
		if err != nil {
			continue
		}
		rule.Subject = copyLabels(svc.Selector)
		out = append(out, rule)
	}
	return out
}

// exposedPeers 按 spec §3.3 的两支五条推出一个暴露型 Service 的入站对端，
// 连同解释这个对端从哪来的推导依据。
//
// 第三个返回值只标「判不出对端」，不是「判不出这个 Service 有没有暴露」——
// NodePort 与 LoadBalancer 走到这里之前已经确认过 Type，因此这里的 false
// 专指 §3.3 第 4 条：LoadBalancer 拿不到入口地址，或入口地址判不出归属
// （地址解析失败、登记网段解析失败、或多个入口地址的判定结果不一致）。
func exposedPeers(a snapshot.Assets, svc snapshot.Service) ([]networkingv1.NetworkPolicyPeer, []Derivation, bool) {
	if svc.Type == serviceTypeNodePort {
		// NodePort 没有入口地址，但流量经 kube-proxy 到达 Pod 时源地址被
		// SNAT 成节点地址（externalTrafficPolicy 缺省为 Cluster）—— 对端
		// 因而是确定的，不走「判不出」那条路径。
		if a.Registry.NodeCIDR == "" {
			return nil, nil, false
		}
		return []networkingv1.NetworkPolicyPeer{{
				IPBlock: &networkingv1.IPBlock{CIDR: a.Registry.NodeCIDR},
			}}, []Derivation{
				// spec.type 说明「为什么走节点网段」，nodeCIDR 才是那个值
				// 实际的出处——两条缺一都会把审计的人指向错的地方（同
				// deriveNodeAgent 对 hostNetwork agent 的两条溯源）。
				{SourceKind: SourceService, Cluster: a.ClusterID, Namespace: svc.Namespace,
					Name: svc.Name, Field: "spec.type"},
				{SourceKind: SourceClusterRegistry, Cluster: a.ClusterID,
					Name: a.ClusterID, Field: "nodeCIDR"},
			}, true
	}

	// LoadBalancer，第 1 条：运维显式声明过的范围，比平台推出来的更准，
	// 优先于按入口地址推导 —— 两者不一致时推导的那个只会更宽。
	if len(svc.LoadBalancerSourceRanges) > 0 {
		peers := make([]networkingv1.NetworkPolicyPeer, 0, len(svc.LoadBalancerSourceRanges))
		for _, cidr := range svc.LoadBalancerSourceRanges {
			peers = append(peers, networkingv1.NetworkPolicyPeer{
				IPBlock: &networkingv1.IPBlock{CIDR: cidr},
			})
		}
		return peers, []Derivation{{
			SourceKind: SourceService, Cluster: a.ClusterID, Namespace: svc.Namespace,
			Name: svc.Name, Field: "spec.loadBalancerSourceRanges",
		}}, true
	}

	// 第 4 条：取不到入口地址就判不出范围，不生成、不臆造。
	if len(svc.LoadBalancerIngressIPs) == 0 {
		return nil, nil, false
	}

	// 第 2/3 条：全部入口地址的归属判定必须一致才能用；不一致、或任意一个
	// 判不出，都走「不生成」——见 classifyIngressIPs 的说明。
	verdict, ok := classifyIngressIPs(a.Registry, svc.LoadBalancerIngressIPs)
	if !ok {
		return nil, nil, false
	}

	derivations := []Derivation{{
		SourceKind: SourceService, Cluster: a.ClusterID, Namespace: svc.Namespace,
		Name: svc.Name, Field: "status.loadBalancer.ingress",
	}}
	if verdict.field != "" {
		// 只在命中了某个已登记网段时才追加：面向公网（0.0.0.0/0）不是从
		// 注册信息里查出来的，是「查过、哪个都不命中」这件事本身，没有
		// 第二条依据可指。
		derivations = append(derivations, Derivation{
			SourceKind: SourceClusterRegistry, Cluster: a.ClusterID,
			Name: a.ClusterID, Field: verdict.field,
		})
	}
	return []networkingv1.NetworkPolicyPeer{{
		IPBlock: &networkingv1.IPBlock{CIDR: verdict.cidr},
	}}, derivations, true
}

// ingressAddrStatus 是单个入口地址归属判定的三态结果。
//
// 不是二态（命中/未命中）：地址解析失败与「查过、不在任何注册网段」是两件
// 不同的事，前者判不出，后者是一个确定的结论（面向公网）。把前者读成后者
// 会把一次判定失败误报成「更宽」的那一侧（design review I4）。
type ingressAddrStatus int

const (
	// addrUnknown 表示判不出——地址本身解析失败，或某个已登记网段本身
	// 解析失败（因而无法排除该地址落在其中的可能）。
	addrUnknown ingressAddrStatus = iota
	// addrPublic 表示地址解析成功，且不在任何一个（可解析的）已登记网段内。
	addrPublic
	// addrRegistered 表示地址落在某个已登记网段内。
	addrRegistered
)

// cidrField 是登记网段的一个字段名与它当前的取值，用于同时驱动匹配与
// 推导依据的 Field——两者必须来自同一处，否则命中 pod_cidr 时依据却写着
// node_cidr（design review I5）。
type cidrField struct {
	field string
	cidr  string
}

// registryCIDRFields 按 §3.3 判定顺序列出 ClusterRegistry 里参与归属判定
// 的两个网段。顺序即优先级：真实集群里两段不重叠，顺序不产生歧义，重叠
// 只可能来自一次错误的登记（同 internal/cluster 的 scopeOf 注释）。
func registryCIDRFields(reg snapshot.ClusterRegistry) []cidrField {
	return []cidrField{
		{field: "nodeCIDR", cidr: reg.NodeCIDR},
		{field: "podCIDR", cidr: reg.PodCIDR},
	}
}

// classifyIngressIP 判定单个入口地址落在哪个已登记网段，或面向公网，
// 或判不出。
func classifyIngressIP(reg snapshot.ClusterRegistry, ip string) (cidrField, ingressAddrStatus) {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return cidrField{}, addrUnknown
	}
	sawMalformed := false
	for _, f := range registryCIDRFields(reg) {
		if f.cidr == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(f.cidr)
		if err != nil {
			// 登记的网段本身解析不了：不是「这个地址不在这段里」，是这段
			// 登记本身有问题，判不出这个地址落不落在它应该表示的范围内。
			// 跳过去比较下一段是可以的（那一段依然是可信的判据），但不能
			// 让「跳过」的后果变成「都不命中 → 面向公网」——一次打错的
			// 登记会把这个集群里每一个真正内部的 LB 都判成 0.0.0.0/0
			// （design review I4）。
			sawMalformed = true
			continue
		}
		if prefix.Contains(addr) {
			return f, addrRegistered
		}
	}
	if sawMalformed {
		return cidrField{}, addrUnknown
	}
	return cidrField{}, addrPublic
}

// ingressVerdict 是一组入口地址达成一致后的判定结果。
type ingressVerdict struct {
	// cidr 是最终对端 CIDR：命中的注册网段，或 "0.0.0.0/0"。
	cidr string
	// field 是该 CIDR 对应的 ClusterRegistry 字段名；面向公网时为空——
	// 那不是从注册信息里查出来的值，没有第二条依据可指。
	field string
}

// classifyIngressIPs 判定一组入口地址的归属，只在**全部地址给出同一个
// 答案**时才采纳。
//
// 一个 Service 可以有多个入口地址（双栈按 ipFamilies、多可用区按云厂商
// 实现），这份列表本身无序。若不同地址落在不同范围——一个在 node_cidr、
// 一个面向公网——两个方向都危险：认内网的那个会让候选策略切断真实的公网
// 入口，认公网的那个会开一条不该开的 0.0.0.0/0。这里没有依据断言该信哪一
// 个，本平台对"无依据"的回答是报缺口，不是挑列表里的第一个
// （design review I3；对照 CLAUDE.md §3"判不出就 UNKNOWN"）。
//
// 任意一个地址判不出（addrUnknown）也让整体判不出：与其在部分信息下拼出
// 一个可能错的结论，不如照实报缺口，见 classifyIngressIP 的说明。
func classifyIngressIPs(reg snapshot.ClusterRegistry, ips []string) (ingressVerdict, bool) {
	if len(ips) == 0 {
		return ingressVerdict{}, false
	}
	var verdict ingressVerdict
	for i, ip := range ips {
		f, status := classifyIngressIP(reg, ip)
		if status == addrUnknown {
			return ingressVerdict{}, false
		}
		v := ingressVerdict{cidr: "0.0.0.0/0"}
		if status == addrRegistered {
			v = ingressVerdict{cidr: f.cidr, field: f.field}
		}
		if i == 0 {
			verdict = v
			continue
		}
		if v != verdict {
			return ingressVerdict{}, false
		}
	}
	return verdict, true
}
