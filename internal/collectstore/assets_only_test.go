package collectstore_test

import (
	"context"
	"errors"
	"testing"

	"github.com/imkerbos/Distill/internal/collectstore"
	"github.com/imkerbos/Distill/internal/snapshot"
	"github.com/imkerbos/Distill/internal/store"
)

// 一个采过资产、还没摄入过流量的集群，必须答得出资产已经能回答的那部分。
//
// **这条用例来自一次真实接入**：uat-k8s-cluster-01 接进来之后，库里有 653 个
// 身份区间、5 条 NetworkPolicy、652 个 Pod，而五屏全部答「还没有可用的采集
// 数据」——平台在扣着自己已经算得出的答案（design doc 2026-08-18 §1）。
//
// 拓扑的**节点**来自身份区间与锚点快照的策略，只有**边**需要流量。
func TestTopologyAnswersFromAssetsWhenNoTrafficWasIngested(t *testing.T) {
	r, s := newTestReader(t)
	ctx := context.Background()
	saveRun(t, s, "run-assets-only", firstRunAt, assetOnlyPods())

	topo, err := r.Topology(ctx, collectedID, store.LevelNamespace)
	if err != nil {
		t.Fatalf("Topology() error = %v, want an answer built from assets", err)
	}
	if len(topo.Nodes) == 0 {
		t.Error("Topology() returned no nodes — 身份区间与策略都在库里，" +
			"这些节点是算得出来的")
	}
	if len(topo.Edges) != 0 {
		t.Errorf("Topology() returned %d edges with no traffic ingested — "+
			"边只能来自观测，凭空画出来的边会被读成「这两个工作负载在通信」",
			len(topo.Edges))
	}
	// **这一条是整份改动的安全绳。** 边为空绝不能读成「这些工作负载之间
	// 没有通信」——那正是本平台的核心失败方向（CLAUDE.md §3）：没看见 ≠
	// 不存在，而漏看的后果是那条规则被判「无流量、可收紧」。
	if topo.TrafficObserved {
		t.Error("Topology().TrafficObserved = true with no ingest — 调用方会把" +
			"一张没有边的图读成「这个集群里没有通信」")
	}
}

func TestSecurityAnswersWithNakedPodsWhenNoTrafficWasIngested(t *testing.T) {
	// 裸奔 Pod 来自资产快照，代码里本来就写着「不受本窗口约束」。
	r, s := newTestReader(t)
	ctx := context.Background()
	saveRun(t, s, "run-assets-only", firstRunAt, assetOnlyPods())

	rep, err := r.Security(ctx, collectedID, store.TimeWindow{})
	if err != nil {
		t.Fatalf("Security() error = %v, want an answer built from assets", err)
	}
	if len(rep.NakedPods) == 0 {
		t.Error("Security() returned no naked pods — 锚点快照里有没被策略覆盖的 Pod")
	}
	if len(rep.RiskyFlows) != 0 || len(rep.EgressTargets) != 0 {
		t.Errorf("Security() returned %d risky flows / %d egress targets with no traffic",
			len(rep.RiskyFlows), len(rep.EgressTargets))
	}
	if rep.TrafficObserved {
		t.Error("Security().TrafficObserved = true with no ingest — 一份空的风险" +
			"清单会被读成「这个集群没有风险连接」")
	}
}

// 一个**从没被采过**的集群仍然整份拒绝。
//
// 这一条与上面两条不是重复，是那两条的边界：本轮放开的是「资产有、流量没有」，
// 不是「一无所知」。两者合并之后，「一无所知」会走上按资产作答那条路，而那条
// 路上没有资产可用 —— 失败方式会从一次明确的拒绝变成一份空报告。
func TestAClusterNeverCollectedStillRefuses(t *testing.T) {
	r, _ := newTestReader(t)
	ctx := context.Background()

	topo, err := r.Topology(ctx, silentID, store.LevelNamespace)
	if !errors.Is(err, collectstore.ErrNoCollection) {
		t.Errorf("Topology() error = %v, want ErrNoCollection", err)
	}
	if len(topo.Nodes) != 0 {
		t.Errorf("Topology() returned %d nodes alongside the refusal", len(topo.Nodes))
	}
	rep, err := r.Security(ctx, silentID, store.TimeWindow{})
	if !errors.Is(err, collectstore.ErrNoCollection) {
		t.Errorf("Security() error = %v, want ErrNoCollection", err)
	}
	if len(rep.NakedPods) != 0 {
		t.Errorf("Security() returned %d naked pods alongside the refusal", len(rep.NakedPods))
	}
}

// 有流量时 TrafficObserved 必须为 true —— 否则这个字段恒为 false，
// 上面那两条断言就再也证明不了任何事。
func TestTrafficObservedIsTrueOnceThereIsIngest(t *testing.T) {
	r, s := newTestReaderWithSource(t, testSource())
	ctx := context.Background()
	seedRecycledAddress(t, s)

	topo, err := r.Topology(ctx, collectedID, store.LevelNamespace)
	if err != nil {
		t.Fatalf("Topology() error = %v", err)
	}
	if !topo.TrafficObserved {
		t.Error("Topology().TrafficObserved = false although a window was ingested")
	}
}

// assetOnlyPods 是一对活着的 Pod：一个落在有策略的命名空间里，一个没有。
//
// 后者是这份改动要能答出来的那个 —— 「没有任何 NetworkPolicy 覆盖它」这句话
// 只需要资产，不需要看过一条流量。
func assetOnlyPods() []snapshot.Pod {
	return []snapshot.Pod{
		podAt("payment", "api-1", "3c9d2b1a-0000-4000-8000-00000000000a", recycledIP, "api"),
		podAt("shop", "web-1", "3c9d2b1a-0000-4000-8000-00000000000b", peerIP, "web"),
	}
}
