package fixture_test

import (
	"context"
	"testing"

	"github.com/imkerbos/Distill/internal/fixture"
	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/replay"
	"github.com/imkerbos/Distill/internal/store"
)

// memSource 是内存版 store.ClusterSource：reader 现在每个请求向注册表
// 求证，测试给它一份内存里的注册信息即可，不必为此接一个数据库。
type memSource struct {
	clusters []registry.Cluster
}

func (m memSource) Clusters(context.Context) ([]registry.Cluster, error) {
	return m.clusters, nil
}

func (m memSource) Cluster(_ context.Context, id string) (registry.Cluster, bool, error) {
	for _, c := range m.clusters {
		if c.ID == id {
			return c, true, nil
		}
	}
	return registry.Cluster{}, false, nil
}

func fixtureSource() memSource { return memSource{clusters: fixtureClusters()} }

// fixtureClusters 镜像 internal/fixture/asset.go 里两个集群的注册信息。
// 值必须与那里的 Registry/APIServers 逐字一致。
func fixtureClusters() []registry.Cluster {
	return []registry.Cluster{
		{
			ID: "prod-asia-1", DisplayName: "Asia Prod",
			PodCIDR: "10.4.0.0/14", NodeCIDR: "10.128.0.0/20",
			State:              registry.StateReady,
			APIServers:         []registry.APIServer{{Host: "10.9.0.2", CIDR: "10.9.0.0/28", Port: 443}},
			HealthCheckSources: []string{"35.191.0.0/16", "130.211.0.0/22"},
		},
		{
			ID: "prod-eu-1", DisplayName: "EU Prod",
			PodCIDR: "10.4.0.0/14", NodeCIDR: "10.132.0.0/20",
			State:              registry.StateReady,
			APIServers:         []registry.APIServer{{Host: "10.13.0.2", CIDR: "10.13.0.0/28", Port: 443}},
			HealthCheckSources: []string{"35.191.0.0/16", "130.211.0.0/22"},
		},
	}
}

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

// 数据集的验收标准不是"字段长得对"，而是"跑完引擎真的产出了这些结论"。
// 形状测试挡不住有人改掉 buildFlows 后 demo 悄悄变成全绿 —— 而全绿正是
// 这个平台最不该给人的印象。断言非零而不锁定具体数量：锁死计数会让
// 以后每次调数据都变成一次假报警。
//
// 求值走 store.NewFixtureReader 而不是在测试里另搭一套求值器：自己搭的
// 那套可以和生产装配（CCNP 选项、按目的端集群取求值器）慢慢分叉，
// 到那时这道守卫验的就不再是产品实际跑的东西了。
func TestDatasetProducesAllDeliberateGaps(t *testing.T) {
	f := fixture.Load()
	// 不带 Cluster 筛选的 Flows 现在也按注册状态过滤（两端都未注册的
	// 记录不出现，见 FixtureReader.hasRegisteredEndpoint）；两个 fixture
	// 集群都要注册，这条断言才能看到全量求值结果。
	r := store.NewFixtureReader(f, fixtureSource())
	page, err := r.Flows(context.Background(),
		store.FlowFilter{Limit: len(f.Flows), Window: r.DataWindow()})
	if err != nil {
		t.Fatalf("Flows: %v", err)
	}
	if len(page.Items) != len(f.Flows) {
		t.Fatalf("evaluated %d of %d flows", len(page.Items), len(f.Flows))
	}

	var degraded, crossCluster int
	reasons := map[string]int{}

	for _, rec := range page.Items {
		if rec.Confidence == string(replay.ConfidenceDegraded) {
			degraded++
		}
		if rec.CrossCluster {
			crossCluster++
		}
		if rec.UnknownReason != "" {
			reasons[rec.UnknownReason]++
		}
	}

	if degraded == 0 {
		t.Error("no DEGRADED verdicts; the mesh namespace no longer demonstrates obscured L4 identity")
	}
	if crossCluster == 0 {
		t.Error("no cross-cluster flows; the known enforcement gap is no longer demonstrated")
	}
	for _, want := range []replay.UnknownReason{
		replay.ReasonPolicyMalformed,
		replay.ReasonSnapshotMissing,
	} {
		if reasons[string(want)] == 0 {
			t.Errorf("no %s verdicts; that gap is no longer demonstrated", want)
		}
	}
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

// 数据集必须包含真实可疑的流量，否则高风险端口报告只能渲染成空 ——
// 而一块永远空着的安全报告，读者读到的是"这套集群很干净"，
// 实际含义却是"这个数据集没造这类流量"。两者差得很远。
//
// 断言的是场景而非条数：条数可以调，场景不能消失。
func TestDatasetContainsHighRiskTraffic(t *testing.T) {
	f := fixture.Load()

	var crossNSDatabase, crossNSAdmin, egressAdmin int
	for _, fl := range f.Flows {
		src, dst := fl.Flow.Source, fl.Flow.Dest
		switch {
		// 出公网的管理端口：目的端没有 Pod 身份也不属于任何集群。
		case dst.Pod == nil && dst.ClusterID == "" && fl.Flow.Port == 22:
			egressAdmin++
		case src.Pod != nil && dst.Pod != nil && src.Pod.Namespace != dst.Pod.Namespace:
			switch fl.Flow.Port {
			case 3306:
				crossNSDatabase++
			case 22:
				crossNSAdmin++
			}
		}
	}

	if crossNSDatabase == 0 {
		t.Error("没有跨 namespace 的数据库直连流量，高风险端口报告失去这一类样本")
	}
	if crossNSAdmin == 0 {
		t.Error("没有跨 namespace 的管理端口流量，高风险端口报告失去这一类样本")
	}
	if egressAdmin == 0 {
		t.Error("没有出公网的管理端口流量 —— 这是报告里最该出现的一类")
	}
}
