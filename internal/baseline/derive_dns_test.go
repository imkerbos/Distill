package baseline

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/imkerbos/Distill/internal/replay"
	"github.com/imkerbos/Distill/internal/snapshot"
)

func dnsAssets() snapshot.Assets {
	return snapshot.Assets{
		ClusterID: "c1",
		Services: []snapshot.Service{{
			ClusterID: "c1", Namespace: "kube-system", Name: "kube-dns",
			Type: "ClusterIP", ClusterIP: "10.8.0.10",
			Selector: map[string]string{"k8s-app": "kube-dns"},
			Ports: []snapshot.ServicePort{
				{Name: "dns", Port: 53, TargetPort: 53, Protocol: "UDP"},
				{Name: "dns-tcp", Port: 53, TargetPort: 53, Protocol: "TCP"},
			},
		}},
		Endpoints: []snapshot.Endpoints{{
			ClusterID: "c1", Namespace: "kube-system", Name: "kube-dns",
			Addresses: []string{"10.4.0.18"}, Ports: []int32{53},
		}},
		APIServers: []snapshot.APIServerEndpoint{
			{ClusterID: "c1", Host: "10.9.0.2", CIDR: "10.9.0.0/28", Port: 443},
		},
	}
}

// DNS 的 peer 必须是后端 Pod 的 selector，不能是 ClusterIP。
// NetworkPolicy 的 peer 只能是 selector 或 ipBlock，ClusterIP 两者都不是，
// 写进去的规则永远匹配不上 —— 而它看起来完全正常。
func TestDeriveDNSSelectsBackendPodsNotClusterIP(t *testing.T) {
	rules := deriveDNS(dnsAssets())
	if len(rules) != 1 {
		t.Fatalf("deriveDNS returned %d rules, want 1", len(rules))
	}
	r := rules[0]
	if r.Direction != replay.DirectionEgress || r.Egress == nil {
		t.Fatalf("rule = %+v, want an egress rule", r)
	}
	if len(r.Egress.To) != 1 {
		t.Fatalf("egress peers = %d, want 1", len(r.Egress.To))
	}
	peer := r.Egress.To[0]
	if peer.IPBlock != nil {
		t.Error("DNS peer uses ipBlock; ClusterIP is not selectable by NetworkPolicy")
	}
	if peer.PodSelector == nil || peer.PodSelector.MatchLabels["k8s-app"] != "kube-dns" {
		t.Errorf("PodSelector = %+v, want matchLabels k8s-app=kube-dns", peer.PodSelector)
	}
	if peer.NamespaceSelector == nil ||
		peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "kube-system" {
		t.Errorf("NamespaceSelector = %+v, want kube-system", peer.NamespaceSelector)
	}
}

// UDP 与 TCP 两个 53 都要放行。只放 UDP 在响应超过 512 字节时静默失败，
// 症状是"偶发解析失败"，最难查的一类。
func TestDeriveDNSAllowsBothUDPAndTCP53(t *testing.T) {
	r := deriveDNS(dnsAssets())[0]
	seen := map[corev1.Protocol]bool{}
	for _, p := range r.Egress.Ports {
		if p.Protocol == nil || p.Port == nil {
			t.Fatalf("port entry %+v has nil protocol or port", p)
		}
		if p.Port.IntValue() != 53 {
			t.Errorf("port = %v, want 53", p.Port)
		}
		seen[*p.Protocol] = true
	}
	if !seen[corev1.ProtocolUDP] || !seen[corev1.ProtocolTCP] {
		t.Errorf("protocols = %v, want both UDP and TCP", seen)
	}
}

// 后端为空的 Service 不得生成规则：那条规则指向空集，
// 看起来齐备、实际什么都没放行，比缺失更危险。
func TestDeriveDNSSkipsServiceWithNoEndpoints(t *testing.T) {
	a := dnsAssets()
	a.Endpoints = nil
	if rules := deriveDNS(a); len(rules) != 0 {
		t.Errorf("deriveDNS returned %d rules for a backend-less service, want 0", len(rules))
	}
}

func TestDeriveDNSRecordsDerivations(t *testing.T) {
	r := deriveDNS(dnsAssets())[0]
	if len(r.Derivations) != 2 {
		t.Fatalf("derivations = %d, want 2 (service selector + endpoints)", len(r.Derivations))
	}
	var kinds []SourceKind
	for _, d := range r.Derivations {
		kinds = append(kinds, d.SourceKind)
		if d.Cluster != "c1" || d.Namespace != "kube-system" || d.Name != "kube-dns" {
			t.Errorf("derivation = %+v, want cluster c1 / kube-system / kube-dns", d)
		}
		if d.Field == "" {
			t.Error("derivation has empty Field; cannot trace which field produced the rule")
		}
	}
	if kinds[0] != SourceService || kinds[1] != SourceEndpoints {
		t.Errorf("derivation kinds = %v, want [SERVICE ENDPOINTS]", kinds)
	}
}

func TestDeriveControlPlaneUsesRegisteredCIDR(t *testing.T) {
	rules := deriveControlPlane(dnsAssets())
	if len(rules) != 1 {
		t.Fatalf("deriveControlPlane returned %d rules, want 1", len(rules))
	}
	r := rules[0]
	if r.Direction != replay.DirectionEgress || r.Egress == nil {
		t.Fatalf("rule = %+v, want an egress rule", r)
	}
	if len(r.Egress.To) != 1 || r.Egress.To[0].IPBlock == nil {
		t.Fatalf("peers = %+v, want a single ipBlock", r.Egress.To)
	}
	if got := r.Egress.To[0].IPBlock.CIDR; got != "10.9.0.0/28" {
		t.Errorf("CIDR = %q, want 10.9.0.0/28", got)
	}
	if len(r.Egress.Ports) != 1 || r.Egress.Ports[0].Port.IntValue() != 443 {
		t.Errorf("ports = %+v, want single 443", r.Egress.Ports)
	}
	if len(r.Derivations) != 1 || r.Derivations[0].SourceKind != SourceAPIServerEndpoint {
		t.Errorf("derivations = %+v, want one APISERVER_ENDPOINT", r.Derivations)
	}
}

func TestDeriveControlPlaneSkipsWhenNoEndpointRegistered(t *testing.T) {
	a := dnsAssets()
	a.APIServers = nil
	if rules := deriveControlPlane(a); len(rules) != 0 {
		t.Errorf("deriveControlPlane returned %d rules with no endpoint, want 0", len(rules))
	}
}
