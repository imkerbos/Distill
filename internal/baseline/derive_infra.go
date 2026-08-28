package baseline

import (
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/imkerbos/Distill/internal/cluster"
	"github.com/imkerbos/Distill/internal/replay"
	"github.com/imkerbos/Distill/internal/snapshot"
)

// exposedService 是一个对外暴露、要放行健康检查的后端 Service。
//
// gateway 指向它时非 nil：那条溯源要记 SourceGateway，好回答「这条规则从
// 哪来」。为 nil 表示它是被一个 LoadBalancer/NodePort Service 直接暴露的。
type exposedService struct {
	svc     snapshot.Service
	gateway *snapshot.Gateway
}

// deriveLBHealth 推导负载均衡健康检查入向 Baseline。
//
// 健康检查流量在学习窗口中极可能被当作噪声过滤掉（spec 锚点 2），
// 所以必须由 Baseline 显式给出，不能指望从流量里学到。
//
// **暴露面有两种，都要覆盖**：Ingress/Gateway 的后端 Service，以及
// type=LoadBalancer / NodePort 的 Service 本身。少了后一种，一个只用
// LoadBalancer Service 暴露、没有 Ingress 的 namespace 会永远推不出这条
// Baseline，而 applicability.exposed() 认它暴露、要求这条 —— 写回 gate 于是
// 永久卡死这个 namespace（kind 集群 gateway ns 实测出的这个缺口）。两种
// 归到同一个后端 Service 上去重：一个既被 Gateway 指向、又是 LoadBalancer
// 的 Service 只放行一次，且保留 Gateway 那条溯源。
func deriveLBHealth(a snapshot.Assets) []Rule {
	if len(a.Registry.HealthCheckSources) == 0 {
		return nil
	}

	// 按 (namespace, service) 去重，保序：先扫 Gateway 后端（好让 SourceGateway
	// 溯源被记下），再扫 LoadBalancer/NodePort Service。
	seen := map[string]int{}
	var order []string
	var exposed []exposedService
	add := func(svc snapshot.Service, gw *snapshot.Gateway) {
		if len(svc.Ports) == 0 {
			return
		}
		key := svc.Namespace + "/" + svc.Name
		if _, dup := seen[key]; dup {
			return
		}
		seen[key] = len(exposed)
		order = append(order, key)
		exposed = append(exposed, exposedService{svc: svc, gateway: gw})
	}
	for i := range a.Gateways {
		gw := a.Gateways[i]
		if svc, ok := a.Service(gw.Namespace, gw.BackendService); ok {
			add(svc, &gw)
		}
	}
	for i := range a.Services {
		svc := a.Services[i]
		if svc.Type == serviceTypeLoadBalancer || svc.Type == serviceTypeNodePort {
			add(svc, nil)
		}
	}

	peers := make([]networkingv1.NetworkPolicyPeer, 0, len(a.Registry.HealthCheckSources))
	for _, cidr := range a.Registry.HealthCheckSources {
		peers = append(peers, networkingv1.NetworkPolicyPeer{
			IPBlock: &networkingv1.IPBlock{CIDR: cidr},
		})
	}

	var out []Rule
	for _, key := range order {
		es := exposed[seen[key]]
		svc := es.svc
		// 端口取 targetPort 而非 Service port：健康检查直接打到 Pod，
		// 放行 Service port 会放开一个后端没监听的端口，真正的检查仍被挡。
		// 命名端口经 targetPortOf 原样写成字符串 —— intstr.FromInt32 在命名
		// 端口下取到的是 TargetPort 的零值 0，而 0 是合法端口值，一条指向
		// 端口 0 的规则永远匹配不上、外观却完全正常（UAT 的 kafka-0-external
		// 正是这个形态）。
		ports := make([]networkingv1.NetworkPolicyPort, 0, len(svc.Ports))
		for _, p := range svc.Ports {
			proto := corev1.Protocol(p.Protocol)
			target := targetPortOf(p)
			ports = append(ports, networkingv1.NetworkPolicyPort{Protocol: &proto, Port: &target})
		}
		// 溯源：有 Gateway 就记 Gateway 那一跳；Service 与 Registry 始终记。
		derivations := make([]Derivation, 0, 3)
		if es.gateway != nil {
			derivations = append(derivations, Derivation{
				SourceKind: SourceGateway, Cluster: a.ClusterID, Namespace: es.gateway.Namespace,
				Name: es.gateway.Name, Field: "backendService"})
		}
		derivations = append(derivations,
			Derivation{SourceKind: SourceService, Cluster: a.ClusterID, Namespace: svc.Namespace,
				Name: svc.Name, Field: "spec.ports.targetPort"},
			Derivation{SourceKind: SourceClusterRegistry, Cluster: a.ClusterID,
				Name: a.ClusterID, Field: "healthCheckSources"},
		)
		rule, err := NewRule(
			KindLBHealth, replay.DirectionIngress,
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
		var peers []networkingv1.NetworkPolicyPeer
		derivations := []Derivation{{
			SourceKind: SourceNodeAgent, Cluster: a.ClusterID,
			Namespace: ag.Namespace, Name: ag.App, Field: "hostNetwork",
		}}
		if ag.HostNetwork {
			// 一条登记可以是逗号分隔的多段（双栈，见 cluster.ParsePrefixes），
			// 一段一条对端。原样塞进 IPBlock.CIDR 会产出一个 NetworkPolicy
			// 不认的值，而候选策略在被 apply 之前谁都不会发现。
			nodes, ok := cluster.ParsePrefixes(a.Registry.NodeCIDR)
			if !ok {
				continue
			}
			peers = peersOf(nodes)
			derivations = append(derivations, Derivation{
				SourceKind: SourceClusterRegistry, Cluster: a.ClusterID,
				Name: a.ClusterID, Field: "nodeCIDR",
			})
		} else {
			peers = []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{nsNameLabel: ag.Namespace},
				},
				PodSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"app": ag.App},
				},
			}}
		}
		tcp := corev1.ProtocolTCP
		port := intstr.FromInt32(ag.TargetPort)
		rule, err := NewRule(
			KindNodeAgent, replay.DirectionIngress,
			&networkingv1.NetworkPolicyIngressRule{
				From:  peers,
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
