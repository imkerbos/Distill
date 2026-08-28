package collectstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/flow"
	"github.com/imkerbos/Distill/internal/replay"
	"github.com/imkerbos/Distill/internal/snapshot"
	"github.com/imkerbos/Distill/internal/snapshotstore"
	"github.com/imkerbos/Distill/internal/store"
)

// lbBackendIP 是被测 LoadBalancer 转发到的那个真实 Pod 地址。
const lbBackendIP = "10.4.9.7"

// seedClusterWithLoadBalancer 造出"一个 Service 的 LoadBalancer 入口地址是
// ip，转发到一个真实 Pod"的采集快照。
//
// ip 落在 collectedID 登记的 node_cidr（10.128.0.0/20）里 —— 与真实事故
// 现场同一形状：入口地址天生落在节点网段内，正是它此前被网段判定误读成
// SNAPSHOT_MISSING 的原因（design doc 2026-08-28 §1 症状二）。
func seedClusterWithLoadBalancer(t *testing.T, s *snapshotstore.Store, ip string) {
	t.Helper()
	run := snapshot.Run{
		Status:     snapshot.RunOK,
		StartedAt:  firstRunAt.Add(-30 * time.Second),
		FinishedAt: firstRunAt.Add(5 * time.Second),
		Observation: snapshot.Observation{
			ClusterID:  collectedID,
			RunID:      "run-lb",
			ObservedAt: firstRunAt,
			Namespaces: []snapshot.Namespace{
				{ClusterID: collectedID, Name: "payment"},
				{ClusterID: collectedID, Name: "shop"},
				{ClusterID: collectedID, Name: "batch"},
			},
			Pods: []snapshot.Pod{
				podAt("shop", "web-1", "3c9d2b1a-0000-4000-8000-0000000020ab", lbBackendIP, "web"),
			},
			Services: []snapshot.Service{{
				ClusterID: collectedID, Namespace: "shop", Name: "web-external",
				Type: "LoadBalancer", Selector: map[string]string{"app": "web"},
				LoadBalancerIngressIPs: []string{ip},
			}},
			Policies: []snapshot.NetworkPolicy{{
				ClusterID: collectedID, Namespace: "payment", Name: "allow-api",
				UID:      "8f14e45f-ceea-467a-9ba5-7b5f0f1f0f01",
				Manifest: apiPolicy,
			}},
		},
	}
	if err := s.Save(context.Background(), run); err != nil {
		t.Fatalf("Save(run-lb) error = %v", err)
	}
	if err := s.DeriveIdentityIntervals(context.Background(), collectedID, "run-lb"); err != nil {
		t.Fatalf("DeriveIdentityIntervals(run-lb) error = %v", err)
	}
}

// saveIngestWithConnection 落一条从 srcIP 到 dstIP 的连接。
func saveIngestWithConnection(t *testing.T, s *snapshotstore.Store, srcIP, dstIP string, port int32) {
	t.Helper()
	saveIngest(t, s, []flow.Connection{conn(srcIP, dstIP, port)})
}

// LoadBalancer 入口地址不得再报「快照缺失」。
//
// UAT 上 14 个解不出主体的节点网段地址里，8 个是 LB 入口地址。报成
// SNAPSHOT_MISSING 等于说「我们数据里有个洞」，而真相是「这是一个 LB 入口，
// NetworkPolicy 里它只能是 ipBlock 对端」—— 两句话对下一步做什么的含义
// 完全不同（design doc 2026-08-28 §1 症状二）。
func TestLoadBalancerIngressAddressIsAConclusionNotAGap(t *testing.T) {
	r, s := newTestReader(t)
	const lbIP = "10.128.0.193"
	seedClusterWithLoadBalancer(t, s, lbIP)
	saveIngestWithConnection(t, s, lbIP, lbBackendIP, 9095)

	page, err := r.Flows(context.Background(), store.FlowFilter{
		Cluster: collectedID, Window: describedWindow(),
	})
	if err != nil {
		t.Fatalf("Flows() = %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("拿到 %d 条流量，want 1", len(page.Items))
	}
	if got := page.Items[0].UnknownReason; got != string(replay.ReasonLBIngressAddress) {
		t.Errorf("unknownReason = %q, want LB_INGRESS_ADDRESS", got)
	}
}

// 节点网段里**不是** LB 入口的地址，行为不变。
//
// 守住本轮没有顺手把整个节点网段的分类一起改掉：UAT 上还有 6 个解不出
// 主体的节点网段地址至今没有解释，必须继续留在 SNAPSHOT_MISSING 里，
// 不能被这次改动一并带走。
func TestNonLoadBalancerNodeRangeAddressKeepsItsReason(t *testing.T) {
	r, s := newTestReader(t)
	seedClusterWithLoadBalancer(t, s, "10.128.0.193")
	// 同一节点网段里的另一个地址，但从未被任何 Service 登记为入口。
	saveIngestWithConnection(t, s, "10.128.0.249", lbBackendIP, 8080)

	page, err := r.Flows(context.Background(), store.FlowFilter{
		Cluster: collectedID, Window: describedWindow(),
	})
	if err != nil {
		t.Fatalf("Flows() = %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("拿到 %d 条流量，want 1", len(page.Items))
	}
	if got := page.Items[0].UnknownReason; got == string(replay.ReasonLBIngressAddress) {
		t.Error("非 LB 的节点网段地址也被判成了 LB 入口")
	}
	if got := page.Items[0].UnknownReason; got != string(replay.ReasonSnapshotMissing) {
		t.Errorf("unknownReason = %q, want SNAPSHOT_MISSING: 这是节点网段里一个真实的缺口，"+
			"不是 LB 入口", got)
	}
}
