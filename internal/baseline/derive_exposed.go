package baseline

import (
	"net/netip"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/imkerbos/Distill/internal/cluster"
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
		if len(svc.Selector) == 0 {
			// 没有 selector 是手工维护 Endpoints 的合法形态（外部后端），
			// 但这样一来没有任何 workload 可挂——生成一条 Subject 为空的
			// 规则会被下游读成"广播"，把 peers=[0.0.0.0/0] 发给这个
			// namespace 里毫不相干的 workload，正是 NC1 复现的那个 bug。
			// 不生成，把这个 Service 交给 UnresolvedExposureSubjects：
			// 这仍然是一次真实的暴露，得由调用方（policygen）报成看得见
			// 的缺口，不能悄悄消失（design review NC1，2026-08-28）。
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
// （地址解析失败、没有一个可用的登记网段、或多个入口地址的判定结果不一致）。
func exposedPeers(a snapshot.Assets, svc snapshot.Service) ([]networkingv1.NetworkPolicyPeer, []Derivation, bool) {
	if svc.Type == serviceTypeNodePort {
		// NodePort 没有入口地址，但流量经 kube-proxy 到达 Pod 时源地址被
		// SNAT 成节点地址（externalTrafficPolicy 缺省为 Cluster）—— 对端
		// 因而是确定的，不走「判不出」那条路径。
		// 登记用不了（没登记、或登记本身解析不出来）就推不出对端。判不出
		// 时不生成，交给 Missing() 报缺口 —— 少了这个判断，peers 会变成
		// 一条 ipBlock.cidr="" 的规则：`kubectl apply` 会拒，但那已经是在
		// GitOps 合并之后，症状是一份推不上去的策略文件，而成因在这里。
		nodes, ok := cluster.ParsePrefixes(a.Registry.NodeCIDR)
		if !ok {
			return nil, nil, false
		}
		return peersOf(nodes), []Derivation{
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
	return peersOf(verdict.prefixes), derivations, true
}

// peersOf 把一组网段摊成一组 ipBlock 对端，一段一条。
//
// 一条登记可以是逗号分隔的多段（双栈集群的两个协议族，见
// cluster.ParsePrefixes）。原样把登记字符串塞进 IPBlock.CIDR 会产出
// `cidr: "10.128.0.0/20,fd00:10:128::/64"` —— 一个 NetworkPolicy 不认的值，
// 而候选策略在被 apply 之前谁都不会发现。
func peersOf(prefixes []netip.Prefix) []networkingv1.NetworkPolicyPeer {
	out := make([]networkingv1.NetworkPolicyPeer, 0, len(prefixes))
	for _, p := range prefixes {
		out = append(out, networkingv1.NetworkPolicyPeer{
			IPBlock: &networkingv1.IPBlock{CIDR: p.String()},
		})
	}
	return out
}

// ingressAddrStatus 是单个入口地址归属判定的三态结果。
//
// 不是二态（命中/未命中）：地址解析失败与「查过、不在任何注册网段」是两件
// 不同的事，前者判不出，后者是一个确定的结论（面向公网）。把前者读成后者
// 会把一次判定失败误报成「更宽」的那一侧（design review I4）。
type ingressAddrStatus int

const (
	// addrUnknown 表示判不出——地址本身解析失败，或没有一个可用的已登记
	// 网段（因而无法排除该地址落在某个内部网段里的可能）。
	addrUnknown ingressAddrStatus = iota
	// addrPublic 表示地址解析成功，且不在任何一个（可用的）已登记网段内。
	addrPublic
	// addrRegistered 表示地址落在某个已登记网段内。
	addrRegistered
)

// registeredCIDR 是登记里的一个网段字段名与它的原始取值。
//
// 名字与取值成对传递，是为了同时驱动匹配与推导依据的 Field —— 两者必须
// 来自同一处，否则命中 pod_cidr 时依据却写着 node_cidr（design review I5）。
type registeredCIDR struct {
	field string
	raw   string
}

// registryCIDRFields 按 §3.3 判定顺序列出 ClusterRegistry 里参与归属判定
// 的两个网段。顺序即优先级：真实集群里两段不重叠，顺序不产生歧义，重叠
// 只可能来自一次错误的登记（同 internal/cluster 的 scopeOf 注释）。
func registryCIDRFields(reg snapshot.ClusterRegistry) []registeredCIDR {
	return []registeredCIDR{
		{field: "nodeCIDR", raw: reg.NodeCIDR},
		{field: "podCIDR", raw: reg.PodCIDR},
	}
}

// cidrField 是命中的那个登记字段名，连同它解析出来的全部网段。
type cidrField struct {
	field    string
	prefixes []netip.Prefix
}

// classifyIngressIP 判定单个入口地址落在哪个已登记网段，或面向公网，
// 或判不出。
func classifyIngressIP(reg snapshot.ClusterRegistry, ip string) (cidrField, ingressAddrStatus) {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return cidrField{}, addrUnknown
	}
	sawUnusable := false
	for _, f := range registryCIDRFields(reg) {
		prefixes, ok := cluster.ParsePrefixes(f.raw)
		if !ok {
			// 这一段登记用不了：没登记，或登记本身解析不出来。两种都不是
			// 「这个地址不在这段里」——是根本没有判据说它在不在。跳过去比较
			// 下一段可以（那一段依然可信），但不能让「跳过」的后果变成
			// 「都不命中 → 面向公网」：一个没登记 node_cidr 的集群会因此把
			// 每一个 10.x 的内部 LB 都判成 0.0.0.0/0，而那是这个平台能犯的
			// 最宽的错。**没登记与登记打错走同一条路**：后者当初就是按这条
			// 理由判成 addrUnknown 的（design review I4），前者是同一个危险
			// 更常见的版本，没有理由反着走。
			sawUnusable = true
			continue
		}
		for _, prefix := range prefixes {
			if prefix.Contains(addr) {
				return cidrField{field: f.field, prefixes: prefixes}, addrRegistered
			}
		}
	}
	if sawUnusable {
		return cidrField{}, addrUnknown
	}
	return cidrField{}, addrPublic
}

// ingressVerdict 是一组入口地址达成一致后的判定结果。
type ingressVerdict struct {
	// prefixes 是最终对端网段：命中的那个登记解析出来的全部段（双栈登记
	// 是两段），或面向公网时的 0.0.0.0/0。
	prefixes []netip.Prefix
	// field 是该网段对应的 ClusterRegistry 字段名；面向公网时为空——
	// 那不是从注册信息里查出来的值，没有第二条依据可指。
	field string
}

// publicPrefix 是面向公网时的对端。
var publicPrefix = netip.MustParsePrefix("0.0.0.0/0")

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
//
// **一致与否只比字段名**：命中同一个字段的两个地址，对端就是那个字段解析
// 出来的同一组网段，比不出差别；面向公网的字段名恒为空，与任何命中都不
// 相等。比字段名而不是比网段切片，也让这个判断不必依赖切片可比性。
// 双栈 LB 的两个入口地址（一个 v4、一个 v6）因此仍然一致 —— 它们命中的是
// 同一条 node_cidr 登记的两段。
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
		v := ingressVerdict{prefixes: []netip.Prefix{publicPrefix}}
		if status == addrRegistered {
			v = ingressVerdict{prefixes: f.prefixes, field: f.field}
		}
		if i == 0 {
			verdict = v
			continue
		}
		if v.field != verdict.field {
			return ingressVerdict{}, false
		}
	}
	return verdict, true
}

// UnattachableExposure 是一个暴露型 Service：EXPOSED_INGRESS 能推出它的
// 放行范围，但因为它没有 selector，推不出该挂在哪个 workload 上。
type UnattachableExposure struct {
	// Namespace 是该 Service 所在命名空间。
	Namespace string
	// Name 是该 Service 名。
	Name string
}

// UnresolvedExposureSubjects 返回指定 namespace 里、因为没有 selector 而
// 被 deriveExposedIngress 跳过的暴露型 Service。
//
// **这不是 Missing() 的第二套机制。** Missing() 回答的是"这个 namespace
// 缺不缺 EXPOSED_INGRESS"，是 kind 粒度的；这里回答的是 Missing() 答不出
// 的下一个问题——"具体是哪个 Service"，在同一个 namespace 里有另一个正常
// Service 也生成了 EXPOSED_INGRESS 规则时，Missing() 会显示"齐备"，而这个
// 没有 selector 的 Service 依然什么都没有（design review NC1/NC2，
// 2026-08-28）。调用方（policygen）拿着这份名单负责把它报成看得见的缺口。
func UnresolvedExposureSubjects(a snapshot.Assets, namespace string) []UnattachableExposure {
	var out []UnattachableExposure
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
		if len(svc.Selector) > 0 {
			continue
		}
		out = append(out, UnattachableExposure{Namespace: svc.Namespace, Name: svc.Name})
	}
	return out
}
