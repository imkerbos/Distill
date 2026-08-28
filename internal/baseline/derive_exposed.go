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
// 判不出对端范围的 Service（LoadBalancer 拿不到入口地址）不生成规则 ——
// 交给 Missing() 如实报出缺口，不臆造一条看起来齐备实则错误的放行。
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

		peers, peerDerivation, ok := exposedPeers(a, svc)
		if !ok {
			continue
		}

		ports := make([]networkingv1.NetworkPolicyPort, 0, len(svc.Ports))
		for _, p := range svc.Ports {
			proto := corev1.Protocol(p.Protocol)
			target := targetPortOf(p)
			ports = append(ports, networkingv1.NetworkPolicyPort{Protocol: &proto, Port: &target})
		}

		derivations := []Derivation{
			peerDerivation,
			{
				SourceKind: SourceService, Cluster: a.ClusterID, Namespace: svc.Namespace,
				Name: svc.Name, Field: "spec.ports.targetPort",
			},
		}

		rule, err := NewRule(
			KindExposedIngress, replay.DirectionIngress,
			&networkingv1.NetworkPolicyIngressRule{From: peers, Ports: ports}, nil,
			derivations,
		)
		if err != nil {
			continue
		}
		out = append(out, rule)
	}
	return out
}

// exposedPeers 按 spec §3.3 的两支五条推出一个暴露型 Service 的入站对端。
//
// 第二个返回值只标「判不出对端」，不是「判不出这个 Service 有没有暴露」——
// NodePort 与 LoadBalancer 走到这里之前已经确认过 Type，因此这里的 false
// 专指 §3.3 第 4 条：LoadBalancer 拿不到入口地址。
func exposedPeers(a snapshot.Assets, svc snapshot.Service) ([]networkingv1.NetworkPolicyPeer, Derivation, bool) {
	if svc.Type == serviceTypeNodePort {
		// NodePort 没有入口地址，但流量经 kube-proxy 到达 Pod 时源地址被
		// SNAT 成节点地址（externalTrafficPolicy 缺省为 Cluster）—— 对端
		// 因而是确定的，不走「判不出」那条路径。
		if a.Registry.NodeCIDR == "" {
			return nil, Derivation{}, false
		}
		return []networkingv1.NetworkPolicyPeer{{
				IPBlock: &networkingv1.IPBlock{CIDR: a.Registry.NodeCIDR},
			}}, Derivation{
				SourceKind: SourceService, Cluster: a.ClusterID, Namespace: svc.Namespace,
				Name: svc.Name, Field: "spec.type",
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
		return peers, Derivation{
			SourceKind: SourceService, Cluster: a.ClusterID, Namespace: svc.Namespace,
			Name: svc.Name, Field: "spec.loadBalancerSourceRanges",
		}, true
	}

	// 第 4 条：取不到入口地址就判不出范围，不生成、不臆造。
	if len(svc.LoadBalancerIngressIPs) == 0 {
		return nil, Derivation{}, false
	}

	// 第 2/3 条：入口地址落在集群已登记网段内就用那个网段，否则面向公网。
	// 只取第一个入口地址 —— 一个 LB Service 通常只有一个入口地址，
	// 云厂商偶尔给出多个时目前没有依据判定该取并集还是交集。
	cidr, inRegistered := registeredCIDRContaining(a.Registry, svc.LoadBalancerIngressIPs[0])
	if !inRegistered {
		cidr = "0.0.0.0/0"
	}
	return []networkingv1.NetworkPolicyPeer{{
			IPBlock: &networkingv1.IPBlock{CIDR: cidr},
		}}, Derivation{
			SourceKind: SourceService, Cluster: a.ClusterID, Namespace: svc.Namespace,
			Name: svc.Name, Field: "status.loadBalancer.ingress",
		}, true
}

// registeredCIDRContaining 返回集群登记网段（node_cidr、pod_cidr）中包含该
// 地址的那一个。
//
// 只查这两类：它们是 ClusterRegistry 登记的集群内网段，命中说明这个入口
// 地址只在 VPC 内可达；两者都不命中，说明它面向公网（design doc
// 2026-08-28 §2）。地址或网段解析失败一律按未命中处理 —— 判不出就不猜。
func registeredCIDRContaining(reg snapshot.ClusterRegistry, ip string) (string, bool) {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return "", false
	}
	for _, cidr := range []string{reg.NodeCIDR, reg.PodCIDR} {
		if cidr == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil {
			continue
		}
		if prefix.Contains(addr) {
			return cidr, true
		}
	}
	return "", false
}
