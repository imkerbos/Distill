package replay

import (
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
)

func TestIPBlockMatches(t *testing.T) {
	tests := []struct {
		name  string
		block *networkingv1.IPBlock
		ip    string
		want  bool
	}{
		{
			name:  "ip inside cidr",
			block: &networkingv1.IPBlock{CIDR: "10.0.0.0/8"},
			ip:    "10.4.0.1",
			want:  true,
		},
		{
			name:  "ip outside cidr",
			block: &networkingv1.IPBlock{CIDR: "10.0.0.0/8"},
			ip:    "192.168.1.1",
			want:  false,
		},
		{
			name:  "ip excluded by except",
			block: &networkingv1.IPBlock{CIDR: "10.0.0.0/8", Except: []string{"10.4.0.0/16"}},
			ip:    "10.4.0.1",
			want:  false,
		},
		{
			name:  "ip inside cidr but outside except",
			block: &networkingv1.IPBlock{CIDR: "10.0.0.0/8", Except: []string{"10.4.0.0/16"}},
			ip:    "10.5.0.1",
			want:  true,
		},
		{
			name:  "multiple except entries",
			block: &networkingv1.IPBlock{CIDR: "10.0.0.0/8", Except: []string{"10.4.0.0/16", "10.5.0.0/16"}},
			ip:    "10.5.0.1",
			want:  false,
		},
		{
			name:  "public ip against public cidr",
			block: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: []string{"10.0.0.0/8"}},
			ip:    "8.8.8.8",
			want:  true,
		},
		{
			// IPv4-mapped IPv6 与其 IPv4 形式是同一个地址。netip 把它当作
			// 另一个族，不归一化就会静默判出一个错误的 DENY。
			name:  "ipv4-mapped ipv6 matches the ipv4 cidr",
			block: &networkingv1.IPBlock{CIDR: "10.0.0.0/8"},
			ip:    "::ffff:10.0.0.1",
			want:  true,
		},
		{
			name:  "ipv4-mapped ipv6 is still excluded by except",
			block: &networkingv1.IPBlock{CIDR: "10.0.0.0/8", Except: []string{"10.4.0.0/16"}},
			ip:    "::ffff:10.4.0.1",
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason, err := ipBlockMatches(tt.block, tt.ip)
			if err != nil {
				t.Fatalf("ipBlockMatches: %v", err)
			}
			if reason != ReasonNone {
				t.Errorf("reason = %q, want ReasonNone; every input here is well formed", reason)
			}
			if got != tt.want {
				t.Errorf("ipBlockMatches = %v, want %v", got, tt.want)
			}
		})
	}
}

// 策略里的 CIDR / except 写错才是 POLICY_MALFORMED：修复方式是改 YAML。
func TestIPBlockMatchesRejectsMalformedPolicy(t *testing.T) {
	if _, _, err := ipBlockMatches(&networkingv1.IPBlock{CIDR: "not-a-cidr"}, "10.0.0.1"); err == nil {
		t.Error("malformed CIDR must produce an error, not a silent false")
	}
	if _, _, err := ipBlockMatches(&networkingv1.IPBlock{CIDR: "10.0.0.0/8", Except: []string{"bad"}}, "10.0.0.1"); err == nil {
		t.Error("malformed except entry must produce an error")
	}
}

// 端点 IP 无法解析是流量数据的问题，不是策略写错。报成 POLICY_MALFORMED
// 会把人送去 grep YAML 里的 CIDR 笔误，而真正该查的是流日志接入。
func TestIPBlockMatchesBadEndpointIPIsNotPolicyMalformed(t *testing.T) {
	for _, ip := range []string{"", "not-an-ip"} {
		got, reason, err := ipBlockMatches(&networkingv1.IPBlock{CIDR: "10.0.0.0/8"}, ip)
		if err != nil {
			t.Errorf("endpoint ip %q: err = %v, want nil; a bad flow field is not a malformed policy", ip, err)
		}
		if got {
			t.Errorf("endpoint ip %q: must not match", ip)
		}
		if reason != ReasonSnapshotMissing {
			t.Errorf("endpoint ip %q: reason = %q, want %q", ip, reason, ReasonSnapshotMissing)
		}
	}
}

func TestIPBlockMatchesNilBlock(t *testing.T) {
	got, reason, err := ipBlockMatches(nil, "10.0.0.1")
	if err != nil {
		t.Fatalf("ipBlockMatches: %v", err)
	}
	if got {
		t.Error("nil ipBlock must not match")
	}
	if reason != ReasonNone {
		t.Errorf("reason = %q, want ReasonNone", reason)
	}
}

// peerMatches 是统一入口：selector 型与 ipBlock 型 peer 互斥。
func TestPeerMatchesDispatchesToIPBlock(t *testing.T) {
	peer := networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: "8.8.8.0/24"}}
	ep := Endpoint{IP: "8.8.8.8"}

	got, reason, err := peerMatches(peer, "payment", "c1", ep, nil)
	if err != nil {
		t.Fatalf("peerMatches: %v", err)
	}
	if !got {
		t.Error("ipBlock peer must match an external endpoint by ip alone")
	}
	if reason != ReasonNone {
		t.Errorf("reason = %q, want ReasonNone; ipBlock matching never touches the namespace snapshot", reason)
	}
}
