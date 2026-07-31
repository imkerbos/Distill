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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ipBlockMatches(tt.block, tt.ip)
			if err != nil {
				t.Fatalf("ipBlockMatches: %v", err)
			}
			if got != tt.want {
				t.Errorf("ipBlockMatches = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIPBlockMatchesRejectsMalformedInput(t *testing.T) {
	if _, err := ipBlockMatches(&networkingv1.IPBlock{CIDR: "not-a-cidr"}, "10.0.0.1"); err == nil {
		t.Error("malformed CIDR must produce an error, not a silent false")
	}
	if _, err := ipBlockMatches(&networkingv1.IPBlock{CIDR: "10.0.0.0/8"}, "not-an-ip"); err == nil {
		t.Error("malformed IP must produce an error, not a silent false")
	}
	if _, err := ipBlockMatches(&networkingv1.IPBlock{CIDR: "10.0.0.0/8", Except: []string{"bad"}}, "10.0.0.1"); err == nil {
		t.Error("malformed except entry must produce an error")
	}
}

func TestIPBlockMatchesNilBlock(t *testing.T) {
	got, err := ipBlockMatches(nil, "10.0.0.1")
	if err != nil {
		t.Fatalf("ipBlockMatches: %v", err)
	}
	if got {
		t.Error("nil ipBlock must not match")
	}
}

// peerMatches 是统一入口：selector 型与 ipBlock 型 peer 互斥。
func TestPeerMatchesDispatchesToIPBlock(t *testing.T) {
	peer := networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: "8.8.8.0/24"}}
	ep := Endpoint{IP: "8.8.8.8"}

	got, err := peerMatches(peer, "payment", "c1", ep, nil)
	if err != nil {
		t.Fatalf("peerMatches: %v", err)
	}
	if !got {
		t.Error("ipBlock peer must match an external endpoint by ip alone")
	}
}
