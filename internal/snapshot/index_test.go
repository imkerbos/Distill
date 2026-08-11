package snapshot_test

import (
	"testing"

	"github.com/imkerbos/Distill/internal/snapshot"
)

func testAssets() snapshot.Assets {
	return snapshot.Assets{
		ClusterID: "c1",
		Services: []snapshot.Service{
			{ClusterID: "c1", Namespace: "kube-system", Name: "kube-dns", Type: "ClusterIP"},
			{ClusterID: "c1", Namespace: "gateway", Name: "gateway-lb", Type: "LoadBalancer"},
		},
		Endpoints: []snapshot.Endpoints{
			{ClusterID: "c1", Namespace: "kube-system", Name: "kube-dns", Addresses: []string{"10.4.0.20"}},
		},
	}
}

func TestServiceFoundByNamespaceAndName(t *testing.T) {
	svc, ok := testAssets().Service("kube-system", "kube-dns")
	if !ok {
		t.Fatal("Service(kube-system, kube-dns) not found, want found")
	}
	if svc.Type != "ClusterIP" {
		t.Errorf("Type = %q, want ClusterIP", svc.Type)
	}
}

// 同名对象分属不同 namespace 时不得串台。
func TestServiceDoesNotMatchAcrossNamespaces(t *testing.T) {
	if _, ok := testAssets().Service("gateway", "kube-dns"); ok {
		t.Error("Service(gateway, kube-dns) found, want not found")
	}
}

func TestEndpointsForFound(t *testing.T) {
	ep, ok := testAssets().EndpointsFor("kube-system", "kube-dns")
	if !ok {
		t.Fatal("EndpointsFor(kube-system, kube-dns) not found, want found")
	}
	if len(ep.Addresses) != 1 {
		t.Errorf("Addresses = %v, want 1 entry", ep.Addresses)
	}
}

func TestEndpointsForMissing(t *testing.T) {
	if _, ok := testAssets().EndpointsFor("gateway", "gateway-lb"); ok {
		t.Error("EndpointsFor(gateway, gateway-lb) found, want not found")
	}
}
