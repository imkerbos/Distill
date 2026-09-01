package baseline

import (
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/imkerbos/Distill/internal/cluster"
	"github.com/imkerbos/Distill/internal/replay"
	"github.com/imkerbos/Distill/internal/snapshot"
)

// deriveKubeletProbe 推导 kubelet 探针入向 Baseline。
//
// 对端是节点网段，与 deriveNodeAgent 的 hostNetwork 分支同一条理由：kubelet
// 用宿主网络命名空间，探测的源地址是节点 IP，podSelector 永远选不中它。
//
// 每条规则都带 Subject，端口取自**这一个** workload 自己声明的探针。广播给
// 整个 namespace 会把 A 服务的 8088 放行给同 namespace 里毫不相干的 B——
// 而 B 的探针端口是 80，那条 8088 对它只是一个凭空开的口子
// （同 EXPOSED_INGRESS 的 design review C1，2026-08-28）。
func deriveKubeletProbe(a snapshot.Assets, namespace string) []Rule {
	var out []Rule
	for _, t := range a.ProbeTargets {
		if t.Namespace != namespace {
			continue
		}
		if len(t.Ports) == 0 {
			// 一个走网络的探针都没有：不需要放行，也没有端口可写。
			// 这一档由 notApplicable 判成「不需要」，不进缺失清单。
			continue
		}
		if t.WorkloadKey == "" || t.Workload == "" {
			// 解不出归属标签的 workload 挂不上任何主体。不生成一条
			// Subject 为空的规则——下游会把它读成「广播」，于是这个
			// workload 的探针端口被放行给整个 namespace。
			continue
		}
		// 一条登记可以是逗号分隔的多段（双栈，见 cluster.ParsePrefixes），
		// 一段一条对端。没登记 node CIDR 就推不出对端：不臆造，让 Missing()
		// 如实把这一类报成缺口，处置是去把网段填上。
		nodes, ok := cluster.ParsePrefixes(a.Registry.NodeCIDR)
		if !ok {
			continue
		}

		tcp := corev1.ProtocolTCP
		ports := make([]networkingv1.NetworkPolicyPort, 0, len(t.Ports))
		for _, p := range t.Ports {
			// 端口在采集侧就解析成了数字：NetworkPolicy 的端口名由 CNI 对着
			// 被选中的 Pod 解析，而这条规则的对端是节点网段，名字在这里
			// 没有解析依据。
			port := intstr.FromInt32(p.Port)
			ports = append(ports, networkingv1.NetworkPolicyPort{Protocol: &tcp, Port: &port})
		}

		rule, err := NewRule(
			KindKubeletProbe, replay.DirectionIngress,
			&networkingv1.NetworkPolicyIngressRule{From: peersOf(nodes), Ports: ports}, nil,
			[]Derivation{
				// 两条缺一不可：探针说明「为什么是这几个端口」，nodeCIDR 才是
				// 对端网段的出处。只记其一会把审计的人指向错的地方
				// （同 deriveNodeAgent 对 hostNetwork agent 的两条溯源）。
				{SourceKind: SourcePodProbe, Cluster: a.ClusterID,
					Namespace: t.Namespace, Name: t.Workload,
					Field: "spec.containers[].readinessProbe/livenessProbe/startupProbe"},
				{SourceKind: SourceClusterRegistry, Cluster: a.ClusterID,
					Name: a.ClusterID, Field: "nodeCIDR"},
			},
		)
		if err != nil {
			continue
		}
		rule.Subject = map[string]string{t.WorkloadKey: t.Workload}
		out = append(out, rule)
	}
	return out
}

// probeDeclared 报告这个 namespace 里有没有 workload 声明过走网络的探针。
//
// 判据是**端口非空**，不是「有没有 ProbeTarget 这一行」：一个只用 exec
// 探针的 workload 会有行、但没有端口，它确实不需要这条放行。
//
// 与 scrapeDeclared 同一个形状：声明决定适不适用，登记（node CIDR）决定
// 推不推得出规则。拿后者当适用性判据的话，一个没登记网段的集群会显得
// 「不需要放行探针」，下发之后 kubelet 被挡，Pod 滚动重启。
func probeDeclared(a snapshot.Assets, namespace string) bool {
	for _, t := range a.ProbeTargets {
		if t.Namespace == namespace && len(t.Ports) > 0 {
			return true
		}
	}
	return false
}
