package baseline

import (
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/imkerbos/Distill/internal/replay"
	"github.com/imkerbos/Distill/internal/snapshot"
)

// deriveLBHealth 推导负载均衡健康检查入向 Baseline。
//
// 健康检查流量在学习窗口中极可能被当作噪声过滤掉（spec 锚点 2），
// 所以必须由 Baseline 显式给出，不能指望从流量里学到。
func deriveLBHealth(a snapshot.Assets) []Rule {
	if len(a.Registry.HealthCheckSources) == 0 {
		return nil
	}
	var out []Rule
	for _, gw := range a.Gateways {
		svc, ok := a.Service(gw.Namespace, gw.BackendService)
		if !ok || len(svc.Ports) == 0 {
			continue
		}
		peers := make([]networkingv1.NetworkPolicyPeer, 0, len(a.Registry.HealthCheckSources))
		for _, cidr := range a.Registry.HealthCheckSources {
			peers = append(peers, networkingv1.NetworkPolicyPeer{
				IPBlock: &networkingv1.IPBlock{CIDR: cidr},
			})
		}
		// 端口取 targetPort 而非 Service port：健康检查直接打到 Pod，
		// 放行 Service port 会放开一个后端没监听的端口，真正的检查仍被挡。
		ports := make([]networkingv1.NetworkPolicyPort, 0, len(svc.Ports))
		for _, p := range svc.Ports {
			proto := corev1.Protocol(p.Protocol)
			target := intstr.FromInt32(p.TargetPort)
			ports = append(ports, networkingv1.NetworkPolicyPort{Protocol: &proto, Port: &target})
		}
		rule, err := NewRule(
			KindLBHealth, replay.DirectionIngress,
			&networkingv1.NetworkPolicyIngressRule{From: peers, Ports: ports}, nil,
			[]Derivation{
				{SourceKind: SourceGateway, Cluster: a.ClusterID, Namespace: gw.Namespace,
					Name: gw.Name, Field: "backendService"},
				{SourceKind: SourceService, Cluster: a.ClusterID, Namespace: svc.Namespace,
					Name: svc.Name, Field: "spec.ports.targetPort"},
				{SourceKind: SourceClusterRegistry, Cluster: a.ClusterID,
					Name: a.ClusterID, Field: "healthCheckSources"},
			},
		)
		if err != nil {
			continue
		}
		out = append(out, rule)
	}
	return out
}

// deriveMetrics 推导指定 namespace 的 metrics 抓取入向 Baseline。
//
// 抓取流量频率低、模式规整，容易被学习环节当作背景噪声剔除，
// 而它被断开的后果是监控盲区 —— 在事故发生时才显现，那时恰好看不到数据。
func deriveMetrics(a snapshot.Assets, namespace string) []Rule {
	var out []Rule
	for _, st := range a.ScrapeTargets {
		if st.TargetNamespace != namespace || len(st.ScraperLabels) == 0 {
			continue
		}
		tcp := corev1.ProtocolTCP
		port := intstr.FromInt32(st.TargetPort)
		rule, err := NewRule(
			KindMetrics, replay.DirectionIngress,
			&networkingv1.NetworkPolicyIngressRule{
				From: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{nsNameLabel: st.ScraperNamespace},
					},
					PodSelector: &metav1.LabelSelector{MatchLabels: copyLabels(st.ScraperLabels)},
				}},
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}},
			}, nil,
			[]Derivation{{
				SourceKind: SourceScrapeTarget, Cluster: a.ClusterID,
				Namespace: st.ScraperNamespace, Name: st.TargetNamespace,
				Field: "scraperLabels",
			}},
		)
		if err != nil {
			continue
		}
		out = append(out, rule)
	}
	return out
}

// deriveNodeAgent 推导节点级 agent 入向 Baseline。
//
// hostNetwork agent 必须走 node CIDR：它使用宿主网络命名空间，源地址是
// 节点 IP，podSelector 永远选不中它（spec §6.2）。写成 podSelector 会得到
// 一条看起来正确、实际从不匹配的规则，症状是日志与监控静默中断。
func deriveNodeAgent(a snapshot.Assets) []Rule {
	var out []Rule
	for _, ag := range a.NodeAgents {
		var peer networkingv1.NetworkPolicyPeer
		derivations := []Derivation{{
			SourceKind: SourceNodeAgent, Cluster: a.ClusterID,
			Namespace: ag.Namespace, Name: ag.App, Field: "hostNetwork",
		}}
		if ag.HostNetwork {
			if a.Registry.NodeCIDR == "" {
				continue
			}
			peer = networkingv1.NetworkPolicyPeer{
				IPBlock: &networkingv1.IPBlock{CIDR: a.Registry.NodeCIDR},
			}
			derivations = append(derivations, Derivation{
				SourceKind: SourceClusterRegistry, Cluster: a.ClusterID,
				Name: a.ClusterID, Field: "nodeCIDR",
			})
		} else {
			peer = networkingv1.NetworkPolicyPeer{
				NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{nsNameLabel: ag.Namespace},
				},
				PodSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"app": ag.App},
				},
			}
		}
		tcp := corev1.ProtocolTCP
		port := intstr.FromInt32(ag.TargetPort)
		rule, err := NewRule(
			KindNodeAgent, replay.DirectionIngress,
			&networkingv1.NetworkPolicyIngressRule{
				From:  []networkingv1.NetworkPolicyPeer{peer},
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}},
			}, nil, derivations,
		)
		if err != nil {
			continue
		}
		out = append(out, rule)
	}
	return out
}
