package replay

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func numericPort(p int32, proto corev1.Protocol) networkingv1.NetworkPolicyPort {
	v := intstr.FromInt32(p)
	return networkingv1.NetworkPolicyPort{Port: &v, Protocol: &proto}
}

func portRange(from, to int32, proto corev1.Protocol) networkingv1.NetworkPolicyPort {
	v := intstr.FromInt32(from)
	return networkingv1.NetworkPolicyPort{Port: &v, EndPort: &to, Protocol: &proto}
}

func namedPortRule(name string, proto corev1.Protocol) networkingv1.NetworkPolicyPort {
	v := intstr.FromString(name)
	return networkingv1.NetworkPolicyPort{Port: &v, Protocol: &proto}
}

func TestPortMatchesEmptyListAllowsEverything(t *testing.T) {
	ok, reason := portMatches(nil, ProtocolTCP, 8080, nil)
	if !ok {
		t.Error("an empty ports list must allow all ports")
	}
	if reason != ReasonNone {
		t.Errorf("reason = %q, want none", reason)
	}
}

func TestPortMatchesNumeric(t *testing.T) {
	ports := []networkingv1.NetworkPolicyPort{numericPort(8080, corev1.ProtocolTCP)}

	if ok, _ := portMatches(ports, ProtocolTCP, 8080, nil); !ok {
		t.Error("exact port and protocol must match")
	}
	if ok, _ := portMatches(ports, ProtocolTCP, 9090, nil); ok {
		t.Error("different port must not match")
	}
	if ok, _ := portMatches(ports, ProtocolUDP, 8080, nil); ok {
		t.Error("different protocol must not match")
	}
}

// Port 未设置时表示该协议的所有端口。
func TestPortMatchesProtocolOnlyRule(t *testing.T) {
	proto := corev1.ProtocolUDP
	ports := []networkingv1.NetworkPolicyPort{{Protocol: &proto}}

	if ok, _ := portMatches(ports, ProtocolUDP, 53, nil); !ok {
		t.Error("protocol-only rule must match any port of that protocol")
	}
	if ok, _ := portMatches(ports, ProtocolTCP, 53, nil); ok {
		t.Error("protocol-only rule must not match a different protocol")
	}
}

// Protocol 未设置时默认为 TCP。
func TestPortMatchesDefaultsToTCP(t *testing.T) {
	v := intstr.FromInt32(8080)
	ports := []networkingv1.NetworkPolicyPort{{Port: &v}}

	if ok, _ := portMatches(ports, ProtocolTCP, 8080, nil); !ok {
		t.Error("omitted protocol must default to TCP")
	}
	if ok, _ := portMatches(ports, ProtocolUDP, 8080, nil); ok {
		t.Error("omitted protocol must not match UDP")
	}
}

func TestPortMatchesRange(t *testing.T) {
	ports := []networkingv1.NetworkPolicyPort{portRange(8000, 8100, corev1.ProtocolTCP)}

	for _, p := range []int32{8000, 8050, 8100} {
		if ok, _ := portMatches(ports, ProtocolTCP, p, nil); !ok {
			t.Errorf("port %d must be inside the inclusive range 8000-8100", p)
		}
	}
	for _, p := range []int32{7999, 8101} {
		if ok, _ := portMatches(ports, ProtocolTCP, p, nil); ok {
			t.Errorf("port %d must be outside the range 8000-8100", p)
		}
	}
}

func TestPortMatchesSCTP(t *testing.T) {
	ports := []networkingv1.NetworkPolicyPort{numericPort(9999, corev1.ProtocolSCTP)}
	if ok, _ := portMatches(ports, ProtocolSCTP, 9999, nil); !ok {
		t.Error("SCTP must be supported")
	}
}

func TestPortMatchesNamedPort(t *testing.T) {
	dest := &PodRef{
		NamedPorts: []NamedPort{{Name: "http", Port: 8080, Protocol: ProtocolTCP}},
	}
	ports := []networkingv1.NetworkPolicyPort{namedPortRule("http", corev1.ProtocolTCP)}

	if ok, reason := portMatches(ports, ProtocolTCP, 8080, dest); !ok || reason != ReasonNone {
		t.Errorf("named port must resolve to 8080; ok=%v reason=%q", ok, reason)
	}
	if ok, _ := portMatches(ports, ProtocolTCP, 9090, dest); ok {
		t.Error("named port must not match a different resolved port")
	}
}

// 命名端口无法解析时必须返回 UNKNOWN 原因，不能静默判 false ——
// 静默 false 会变成一条错误的 DENY 预测，属于最危险的漏报方向。
func TestPortMatchesUnresolvedNamedPort(t *testing.T) {
	ports := []networkingv1.NetworkPolicyPort{namedPortRule("http", corev1.ProtocolTCP)}

	_, reason := portMatches(ports, ProtocolTCP, 8080, nil)
	if reason != ReasonNamedPortUnresolved {
		t.Errorf("reason = %q, want %q when the destination pod is unknown", reason, ReasonNamedPortUnresolved)
	}

	dest := &PodRef{NamedPorts: []NamedPort{{Name: "grpc", Port: 9090, Protocol: ProtocolTCP}}}
	_, reason = portMatches(ports, ProtocolTCP, 8080, dest)
	if reason != ReasonNamedPortUnresolved {
		t.Errorf("reason = %q, want %q when the name is absent from the pod spec", reason, ReasonNamedPortUnresolved)
	}
}

// 一条规则匹配即整体匹配；未解析的命名端口不应掩盖已成立的匹配。
func TestPortMatchesResolvedRuleWinsOverUnresolvedOne(t *testing.T) {
	ports := []networkingv1.NetworkPolicyPort{
		namedPortRule("http", corev1.ProtocolTCP),
		numericPort(8080, corev1.ProtocolTCP),
	}

	ok, reason := portMatches(ports, ProtocolTCP, 8080, nil)
	if !ok {
		t.Error("a numeric rule that matches must win over an unresolvable named rule")
	}
	if reason != ReasonNone {
		t.Errorf("reason = %q, want none once a rule matched", reason)
	}
}

// 没有任何规则匹配时，未解析的命名端口必须浮出来 —— 静默判 false
// 会变成一条错误的 DENY 预测，是本平台最危险的漏报方向。
func TestPortMatchesUnresolvedSurfacesWhenNothingMatched(t *testing.T) {
	ports := []networkingv1.NetworkPolicyPort{
		namedPortRule("http", corev1.ProtocolTCP),
		numericPort(8080, corev1.ProtocolTCP),
	}

	ok, reason := portMatches(ports, ProtocolTCP, 9090, nil)
	if ok {
		t.Error("no rule matches port 9090; want false")
	}
	if reason != ReasonNamedPortUnresolved {
		t.Errorf("reason = %q, want %q — the accumulator must survive the loop", reason, ReasonNamedPortUnresolved)
	}
}

// 命名端口必须同时匹配名字与协议：只对上名字就解析，会放行本应阻断的流量。
func TestPortMatchesNamedPortRequiresProtocolMatch(t *testing.T) {
	dest := &PodRef{
		NamedPorts: []NamedPort{{Name: "http", Port: 8080, Protocol: ProtocolUDP}},
	}
	ports := []networkingv1.NetworkPolicyPort{namedPortRule("http", corev1.ProtocolTCP)}

	ok, reason := portMatches(ports, ProtocolTCP, 8080, dest)
	if ok {
		t.Error("named port declared on UDP must not resolve for a TCP rule")
	}
	if reason != ReasonNamedPortUnresolved {
		t.Errorf("reason = %q, want %q", reason, ReasonNamedPortUnresolved)
	}
}
