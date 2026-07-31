package fixture_test

import (
	"testing"

	"github.com/imkerbos/Distill/internal/fixture"
	"github.com/imkerbos/Distill/internal/replay"
)

func TestLoadHasTwoClusters(t *testing.T) {
	f := fixture.Load()
	if len(f.Clusters) != 2 {
		t.Fatalf("got %d clusters, want 2", len(f.Clusters))
	}
	for _, want := range []string{"prod-asia-1", "prod-eu-1"} {
		if _, ok := f.Cluster(want); !ok {
			t.Errorf("cluster %q missing", want)
		}
	}
}

// 同名 namespace 存在于两个集群，用来验证平台不会把它们混为一谈。
func TestSameNamespaceExistsInBothClusters(t *testing.T) {
	f := fixture.Load()
	count := 0
	for _, c := range f.Clusters {
		for _, ns := range c.Namespaces {
			if ns.Name == "payment" {
				count++
			}
		}
	}
	if count != 2 {
		t.Errorf("namespace payment appears %d times, want once per cluster", count)
	}
}

func TestFlowsAreSubstantial(t *testing.T) {
	f := fixture.Load()
	if len(f.Flows) < 200 {
		t.Errorf("got %d flows, want at least 200 for a meaningful topology", len(f.Flows))
	}
}

func TestFlowIDsAreUnique(t *testing.T) {
	f := fixture.Load()
	seen := map[string]bool{}
	for _, fl := range f.Flows {
		if fl.ID == "" {
			t.Fatal("a flow has an empty ID")
		}
		if seen[fl.ID] {
			t.Fatalf("duplicate flow ID %q", fl.ID)
		}
		seen[fl.ID] = true
	}
}

// 数据集必须包含真实的"不完美"，否则 demo 会把平台包装成什么都知道，
// 而真实集群上线后必然大量 UNKNOWN，落差会直接损伤信任。
func TestDatasetContainsMeshPods(t *testing.T) {
	f := fixture.Load()
	for _, c := range f.Clusters {
		for _, p := range c.Pods {
			if p.InMesh {
				return
			}
		}
	}
	t.Error("no mesh pod in the dataset; DEGRADED verdicts cannot be demonstrated")
}

func TestDatasetContainsHostNetworkPods(t *testing.T) {
	f := fixture.Load()
	for _, c := range f.Clusters {
		for _, p := range c.Pods {
			if p.HostNetwork {
				return
			}
		}
	}
	t.Error("no hostNetwork pod in the dataset; unmanaged pods cannot be demonstrated")
}

func TestDatasetContainsMalformedPolicy(t *testing.T) {
	f := fixture.Load()
	for _, c := range f.Clusters {
		for _, p := range c.Policies {
			for _, rule := range p.Spec.Ingress {
				for _, peer := range rule.From {
					if peer.IPBlock != nil && peer.IPBlock.CIDR == "10.0.0/8" {
						return
					}
				}
			}
		}
	}
	t.Error("no malformed ipBlock in the dataset; POLICY_MALFORMED cannot be demonstrated")
}

// 部分 flow 的源身份故意未还原，模拟采集丢事件。
func TestDatasetContainsUnresolvedEndpoints(t *testing.T) {
	f := fixture.Load()
	for _, fl := range f.Flows {
		if fl.Flow.Source.Pod == nil && fl.Flow.Source.ClusterID != "" {
			return
		}
	}
	t.Error("no flow with an unresolved in-cluster source; SNAPSHOT_MISSING cannot be demonstrated")
}

func TestDatasetContainsCrossClusterFlows(t *testing.T) {
	f := fixture.Load()
	for _, fl := range f.Flows {
		a, b := fl.Flow.Source.ClusterID, fl.Flow.Dest.ClusterID
		if a != "" && b != "" && a != b {
			return
		}
	}
	t.Error("no cross-cluster flow in the dataset; the enforcement gap cannot be demonstrated")
}

// 每条 flow 的两端要么身份完整，要么明确是外部/未还原 —— 不能是半拉子数据。
func TestFlowEndpointsAreCoherent(t *testing.T) {
	f := fixture.Load()
	for _, fl := range f.Flows {
		for _, ep := range []replay.Endpoint{fl.Flow.Source, fl.Flow.Dest} {
			if ep.IP == "" {
				t.Fatalf("flow %s has an endpoint with no IP", fl.ID)
			}
			if ep.Pod != nil && ep.Pod.ClusterID != ep.ClusterID {
				t.Fatalf("flow %s: endpoint ClusterID %q disagrees with its Pod's %q",
					fl.ID, ep.ClusterID, ep.Pod.ClusterID)
			}
		}
	}
}
