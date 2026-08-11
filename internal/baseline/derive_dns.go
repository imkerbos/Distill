package baseline

import (
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/imkerbos/Distill/internal/replay"
	"github.com/imkerbos/Distill/internal/snapshot"
)

// nsNameLabel 是 Kubernetes 自动给每个 namespace 打上的名称标签。
//
// 用它而非自定义标签作为 namespaceSelector 的依据：它由 kube-apiserver
// 保证存在，不依赖集群管理员的标签规范。依赖自定义标签的规则会在
// 标签缺失的 namespace 上静默失效。
const nsNameLabel = "kubernetes.io/metadata.name"

// dnsServiceName 是集群 DNS 的 Service 名。CoreDNS 在 GKE 上同样以
// kube-dns 为 Service 名，两者不必分别处理。
const dnsServiceName = "kube-dns"

// deriveDNS 推导 DNS 出向 Baseline。
//
// peer 取 Service 的 selector 而非 ClusterIP：NetworkPolicy 的 peer 只能是
// selector 或 ipBlock，ClusterIP 两者都不是，写进去永远匹配不上，且看起来
// 完全正常。Endpoints 用于确认后端非空——一条指向空集的放行规则，
// 比缺这条规则更危险，因为它让齐备性校验通过了。
func deriveDNS(a snapshot.Assets) []Rule {
	svc, ok := a.Service(metav1.NamespaceSystem, dnsServiceName)
	if !ok || len(svc.Selector) == 0 {
		return nil
	}
	ep, ok := a.EndpointsFor(metav1.NamespaceSystem, dnsServiceName)
	if !ok || len(ep.Addresses) == 0 {
		return nil
	}

	udp, tcp := corev1.ProtocolUDP, corev1.ProtocolTCP
	port53 := intstr.FromInt32(53)
	rule, err := NewRule(
		KindDNS, replay.DirectionEgress, nil,
		&networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{nsNameLabel: svc.Namespace},
				},
				PodSelector: &metav1.LabelSelector{MatchLabels: copyLabels(svc.Selector)},
			}},
			// UDP 与 TCP 两个 53 都要放行：响应超过 512 字节时解析器回落到
			// TCP，只放 UDP 的症状是偶发解析失败，最难查的一类。
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: &udp, Port: &port53},
				{Protocol: &tcp, Port: &port53},
			},
		},
		[]Derivation{
			{SourceKind: SourceService, Cluster: a.ClusterID, Namespace: svc.Namespace,
				Name: svc.Name, Field: "spec.selector"},
			{SourceKind: SourceEndpoints, Cluster: a.ClusterID, Namespace: ep.Namespace,
				Name: ep.Name, Field: "subsets.addresses"},
		},
	)
	if err != nil {
		return nil
	}
	return []Rule{rule}
}

// deriveControlPlane 推导 kube-apiserver 出向 Baseline。
//
// 网段取自注册信息而非常量：apiserver 端点在私有集群上因集群而异，
// 硬编码一个网段会在半数集群上生成一条永不匹配的规则。
func deriveControlPlane(a snapshot.Assets) []Rule {
	var out []Rule
	for _, api := range a.APIServers {
		if api.CIDR == "" {
			continue
		}
		tcp := corev1.ProtocolTCP
		port := intstr.FromInt32(api.Port)
		rule, err := NewRule(
			KindControlPlane, replay.DirectionEgress, nil,
			&networkingv1.NetworkPolicyEgressRule{
				To: []networkingv1.NetworkPolicyPeer{
					{IPBlock: &networkingv1.IPBlock{CIDR: api.CIDR}},
				},
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}},
			},
			[]Derivation{{
				SourceKind: SourceAPIServerEndpoint, Cluster: a.ClusterID,
				Name: api.Host, Field: "cidr",
			}},
		)
		if err != nil {
			continue
		}
		out = append(out, rule)
	}
	return out
}

// copyLabels 复制标签表。
//
// 规则持有快照对象的 map 会让两者共享底层引用：任何一方后续被修改，
// 另一方悄悄跟着变，而这类污染在测试里几乎不可能被发现。
func copyLabels(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
