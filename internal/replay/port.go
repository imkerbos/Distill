package replay

import (
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// portMatches 判断流量的协议与端口是否被规则的端口列表允许。
//
// ports 为空表示该规则允许所有端口。返回的 UnknownReason 非 ReasonNone
// 时表示存在无法解析的规则，调用方必须把结论降级为 UNKNOWN —— 静默
// 判 false 会产出错误的 DENY 预测，是最危险的漏报方向。
//
// 命名端口按目的 Pod 的容器端口声明解析：Kubernetes 中 ingress 与
// egress 规则的命名端口都指向接收流量的那一端。
func portMatches(
	ports []networkingv1.NetworkPolicyPort,
	proto Protocol,
	port int32,
	destPod *PodRef,
) (bool, UnknownReason) {
	if len(ports) == 0 {
		return true, ReasonNone
	}

	unresolved := ReasonNone
	for _, p := range ports {
		if ruleProtocol(p) != proto {
			continue
		}

		// Port 未设置：该协议的所有端口。
		if p.Port == nil {
			return true, ReasonNone
		}

		if p.Port.Type == intstr.String {
			resolved, ok := resolveNamedPort(destPod, p.Port.StrVal, proto)
			if !ok {
				unresolved = ReasonNamedPortUnresolved
				continue
			}
			if resolved == port {
				return true, ReasonNone
			}
			continue
		}

		start := p.Port.IntVal
		end := start
		if p.EndPort != nil {
			end = *p.EndPort
		}
		if port >= start && port <= end {
			return true, ReasonNone
		}
	}
	return false, unresolved
}

// ruleProtocol 返回规则的协议，未设置时按 Kubernetes 默认取 TCP。
func ruleProtocol(p networkingv1.NetworkPolicyPort) Protocol {
	if p.Protocol == nil {
		return ProtocolTCP
	}
	switch *p.Protocol {
	case corev1.ProtocolUDP:
		return ProtocolUDP
	case corev1.ProtocolSCTP:
		return ProtocolSCTP
	default:
		return ProtocolTCP
	}
}

// resolveNamedPort 在目的 Pod 的容器端口声明中解析命名端口。
func resolveNamedPort(destPod *PodRef, name string, proto Protocol) (int32, bool) {
	if destPod == nil {
		return 0, false
	}
	for _, np := range destPod.NamedPorts {
		if np.Name == name && np.Protocol == proto {
			return np.Port, true
		}
	}
	return 0, false
}
