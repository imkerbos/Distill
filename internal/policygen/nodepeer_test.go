package policygen_test

import (
	"testing"

	"github.com/imkerbos/Distill/internal/policygen"
	"github.com/imkerbos/Distill/internal/replay"
	"github.com/imkerbos/Distill/internal/snapshot"
)

// nodePeerAssets 是一份只登记了网段的最小资产。
func nodePeerAssets() snapshot.Assets {
	return snapshot.Assets{
		ClusterID: "c1",
		Registry: snapshot.ClusterRegistry{
			ClusterID: "c1", PodCIDR: "172.16.0.0/16", NodeCIDR: "10.170.48.0/24",
		},
	}
}

func nodePeerPod(ns, name, ip string, labels map[string]string, hostNet bool) *replay.PodRef {
	return &replay.PodRef{
		ClusterID: "c1", Namespace: ns, Name: name, IP: ip,
		Labels: labels, HostNetwork: hostNet,
	}
}

// **对端是节点地址时要写成 ipBlock，不能丢掉这条规则。**
//
// 「对端不受 NetworkPolicy 管控」说的是没有策略能作用在它身上；它不等于
// 没有策略需要提到它。A → B 的连接里 A 受管时，A 的出站策略必须放行 B。
//
// UAT 实测：monitoring/prometheus 有 240 条出站规则、没有一条是 9100，
// 于是下发后 Prometheus 抓 node-exporter 直接断，而 dry-run 把它算成
// WOULD_BREAK 是对的 —— 断它的是 Prometheus 自己的出站策略。
func TestAnEgressPeerOnANodeAddressBecomesAnIPBlock(t *testing.T) {
	src := nodePeerPod("monitoring", "prometheus-0", "172.16.5.15",
		map[string]string{"app.kubernetes.io/name": "prometheus"}, false)
	dst := nodePeerPod("monitoring", "node-exporter-x", "10.170.48.94", nil, true)

	res := policygen.Generate(policygen.Input{
		ClusterID: "c1", Assets: nodePeerAssets(),
		Pods: []replay.PodRef{*src},
		Observations: []policygen.Observation{{
			FlowID: "f1", IdentityTrusted: true,
			Flow: replay.Flow{
				Source:   replay.Endpoint{ClusterID: "c1", IP: src.IP, Pod: src},
				Dest:     replay.Endpoint{ClusterID: "c1", IP: dst.IP, Pod: dst},
				Protocol: replay.ProtocolTCP, Port: 9100,
			},
			Decision: replay.Decision{Verdict: replay.VerdictAllow, Confidence: replay.ConfidenceTrusted},
		}},
	})

	var got []string
	for _, p := range res.Policies {
		for _, r := range p.Rules {
			if r.Direction == replay.DirectionEgress {
				got = append(got, r.Peers...)
			}
		}
	}
	if len(got) == 0 {
		t.Fatalf("没有生成出站规则 —— 那条放行表达不出来，下发后 Prometheus 抓 node-exporter 会断。"+
			"ungeneratable=%+v", res.Ungeneratable)
	}
	if got[0] != "10.170.48.0/24" {
		t.Errorf("对端 = %q, want 10.170.48.0/24（包含该地址的那一段登记网段）", got[0])
	}
}

// **地址不在登记的 node CIDR 内时照旧丢弃。**
// 那可能是别的集群的节点、同子网的另一台机器；写成本集群的 node CIDR
// 会放开一片与这条流量无关的地址。
func TestAHostNetworkPeerOutsideTheNodeCIDRIsStillDropped(t *testing.T) {
	src := nodePeerPod("monitoring", "prometheus-0", "172.16.5.15",
		map[string]string{"app.kubernetes.io/name": "prometheus"}, false)
	dst := nodePeerPod("other", "stranger", "192.168.9.9", nil, true)

	res := policygen.Generate(policygen.Input{
		ClusterID: "c1", Assets: nodePeerAssets(),
		Pods: []replay.PodRef{*src},
		Observations: []policygen.Observation{{
			FlowID: "f2", IdentityTrusted: true,
			Flow: replay.Flow{
				Source:   replay.Endpoint{ClusterID: "c1", IP: src.IP, Pod: src},
				Dest:     replay.Endpoint{ClusterID: "c1", IP: dst.IP, Pod: dst},
				Protocol: replay.ProtocolTCP, Port: 9100,
			},
			Decision: replay.Decision{Verdict: replay.VerdictAllow, Confidence: replay.ConfidenceTrusted},
		}},
	})
	for _, p := range res.Policies {
		for _, r := range p.Rules {
			if r.Direction == replay.DirectionEgress {
				t.Errorf("给一个登记网段外的地址生成了规则: %+v", r.Peers)
			}
		}
	}
	var found bool
	for _, u := range res.Ungeneratable {
		if u.Reason == policygen.ReasonUnmanagedEndpoint {
			found = true
		}
	}
	if !found {
		t.Errorf("没有报出缺口: %+v", res.Ungeneratable)
	}
}

// node CIDR 没登记时照旧丢弃：推不出对端就不臆造，与 KUBELET_PROBE 同一条。
func TestWithoutANodeCIDRTheHostNetworkPeerIsStillDropped(t *testing.T) {
	a := nodePeerAssets()
	a.Registry.NodeCIDR = ""
	src := nodePeerPod("monitoring", "prometheus-0", "172.16.5.15",
		map[string]string{"app.kubernetes.io/name": "prometheus"}, false)
	dst := nodePeerPod("monitoring", "node-exporter-x", "10.170.48.94", nil, true)

	res := policygen.Generate(policygen.Input{
		ClusterID: "c1", Assets: a, Pods: []replay.PodRef{*src},
		Observations: []policygen.Observation{{
			FlowID: "f3", IdentityTrusted: true,
			Flow: replay.Flow{
				Source:   replay.Endpoint{ClusterID: "c1", IP: src.IP, Pod: src},
				Dest:     replay.Endpoint{ClusterID: "c1", IP: dst.IP, Pod: dst},
				Protocol: replay.ProtocolTCP, Port: 9100,
			},
			Decision: replay.Decision{Verdict: replay.VerdictAllow, Confidence: replay.ConfidenceTrusted},
		}},
	})
	for _, p := range res.Policies {
		for _, r := range p.Rules {
			if r.Direction == replay.DirectionEgress {
				t.Errorf("没有网段登记却生成了规则: %+v", r.Peers)
			}
		}
	}
}

// **主体是 hostNetwork 的那一支不动。** 那个 workload 不受 NetworkPolicy
// 管控，给它生成策略是一条谁都匹配不到的幽灵规则。
func TestAHostNetworkSubjectStillProducesNoPolicy(t *testing.T) {
	src := nodePeerPod("monitoring", "node-exporter-x", "10.170.48.94",
		map[string]string{"app": "node-exporter"}, true)
	dst := nodePeerPod("demo-game", "api-0", "172.16.3.9",
		map[string]string{"app": "api"}, false)

	res := policygen.Generate(policygen.Input{
		ClusterID: "c1", Assets: nodePeerAssets(),
		Pods: []replay.PodRef{*src, *dst},
		Observations: []policygen.Observation{{
			FlowID: "f4", IdentityTrusted: true,
			Flow: replay.Flow{
				Source:   replay.Endpoint{ClusterID: "c1", IP: src.IP, Pod: src},
				Dest:     replay.Endpoint{ClusterID: "c1", IP: dst.IP, Pod: dst},
				Protocol: replay.ProtocolTCP, Port: 8080,
			},
			Decision: replay.Decision{Verdict: replay.VerdictAllow, Confidence: replay.ConfidenceTrusted},
		}},
	})
	for _, p := range res.Policies {
		if p.Namespace == "monitoring" {
			t.Errorf("给 hostNetwork 主体生成了策略: %s/%s", p.Namespace, p.Workload)
		}
	}
}
