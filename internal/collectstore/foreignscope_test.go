package collectstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/flow"
	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/replay"
	"github.com/imkerbos/Distill/internal/snapshot"
	"github.com/imkerbos/Distill/internal/snapshotstore"
	"github.com/imkerbos/Distill/internal/store"
)

// cacheIP 是第三个主体的地址：验证"没被覆盖的主体仍然可信"需要一条两端
// 都解得开、且都不被 CNP 选中的连接。
const cacheIP = "10.4.0.7"

// saveRunWithScopes 落一次带第二平面覆盖范围的采集运行。
func saveRunWithScopes(
	t *testing.T, s *snapshotstore.Store, at time.Time,
	scopes []snapshot.ForeignScope, complete bool,
) {
	t.Helper()
	services, endpoints := dnsAssets()
	run := snapshot.Run{
		Status:     snapshot.RunOK,
		StartedAt:  at.Add(-30 * time.Second),
		FinishedAt: at.Add(5 * time.Second),
		Observation: snapshot.Observation{
			ClusterID: collectedID, RunID: previewRunID, ObservedAt: at,
			Namespaces: namespacesNamed("payment", "shop", "kube-system"),
			// 第三个 Pod：两端都解得开、且都不被那条 CNP 覆盖的连接需要它。
			// 只有 api 与 web 的话，"没被覆盖的主体仍然可信"就无从断言。
			Pods: withStable(podAt("shop", "cache-1",
				"3c9d2b1a-0000-4000-8000-0000000000c1", cacheIP, "cache")),
			Services:              services,
			Endpoints:             endpoints,
			ForeignScopes:         scopes,
			ForeignScopesComplete: complete,
		},
	}
	ctx := context.Background()
	if err := s.Save(ctx, run); err != nil {
		t.Fatalf("Save() = %v", err)
	}
	if err := s.DeriveIdentityIntervals(ctx, collectedID, previewRunID); err != nil {
		t.Fatalf("DeriveIdentityIntervals() = %v", err)
	}
}

// clusterWithPlanes 把测试集群登记成"确认存在第二平面"。
func clusterWithPlanes(t *testing.T) stubSource {
	t.Helper()
	src := testSource()
	for i, c := range src.clusters {
		if c.ID == collectedID {
			c.OtherPlanes = registry.PlanesPresent
			src.clusters[i] = c
		}
	}
	return src
}

// **降级收窄到真的被 CNP 选中的那些主体。**
//
// 在这之前，集群里只要存在一条 CiliumNetworkPolicy，每一条判定都会被标成
// DEGRADED —— 粒度粗到等于宣布这个集群完全不可信，而降级面越大，操作者越会
// 习惯性忽略它（design doc 2026-08-25 §2）。
func TestForeignScopesNarrowDegradationToCoveredSubjects(t *testing.T) {
	r, s := newTestReaderWithSource(t, clusterWithPlanes(t))
	// 那条 CNP 只选中 payment/api。
	saveRunWithScopes(t, s, firstRunAt, []snapshot.ForeignScope{{
		Namespace: "payment", MatchLabels: map[string]string{"app": "api"},
	}}, true)
	// 两条连接，**两端都解得开**：只有那样判定才来自求值器。一端解不出的
	// 连接走的是 UNKNOWN 那条路，可信度由窗口完整度决定、与第二平面无关
	// （UNKNOWN + TRUSTED 读作"我确信我不知道"，那句话不因 CNP 而改变）。
	saveIngest(t, s, []flow.Connection{
		// 目的端是被那条 CNP 选中的 payment/api。
		conn(peerIP, recycledIP, portResolved),
		// 两端都在 shop，都不被覆盖。
		conn(peerIP, cacheIP, portResolved),
	})

	flows, err := r.Flows(context.Background(), store.FlowFilter{
		Cluster: collectedID, Window: describedWindow(),
	})
	if err != nil {
		t.Fatalf("Flows() = %v", err)
	}
	if len(flows.Items) == 0 {
		t.Fatal("没有流量，下面的断言等于没跑")
	}

	var degraded, trusted int
	for _, f := range flows.Items {
		switch f.Confidence {
		case string(replay.ConfidenceDegraded):
			degraded++
		case string(replay.ConfidenceTrusted):
			trusted++
		}
	}
	if degraded == 0 {
		t.Error("被 CNP 选中的主体没有降级 —— 平台不解释那个平面，它的结论可能是错的")
	}
	if trusted == 0 {
		t.Errorf("没有任何一条判定保持可信（%d 条全降级）—— "+
			"那条 CNP 只选中 payment/api，整片降级等于宣布这个集群完全不可信",
			degraded)
	}
}

// **范围不完整时整片降级**，不拿读到的那一半作答。
//
// 不完整意味着有主体被覆盖而我们不知道是哪些，漏掉一个就是把一条真的被管着
// 的连接判成可信。
func TestIncompleteForeignScopesDegradeEverything(t *testing.T) {
	r, s := newTestReaderWithSource(t, clusterWithPlanes(t))
	// 读到了一条范围，但那份范围本身不完整（比如还有一条 CNP 用了
	// matchExpressions，平台解析不出来）。
	saveRunWithScopes(t, s, firstRunAt, []snapshot.ForeignScope{{
		Namespace: "payment", MatchLabels: map[string]string{"app": "api"},
	}}, false)
	saveIngest(t, s, []flow.Connection{
		conn(peerIP, recycledIP, portResolved),
		conn(peerIP, cacheIP, portResolved),
	})

	flows, err := r.Flows(context.Background(), store.FlowFilter{
		Cluster: collectedID, Window: describedWindow(),
	})
	if err != nil {
		t.Fatalf("Flows() = %v", err)
	}
	if len(flows.Items) == 0 {
		t.Fatal("没有流量，这条用例等于没跑")
	}
	for _, f := range flows.Items {
		if f.Confidence == string(replay.ConfidenceTrusted) {
			t.Errorf("范围不完整却有判定保持可信（%s -> %s）："+
				"不完整意味着有主体被覆盖而我们不知道是哪些",
				f.SourceLabel, f.DestLabel)
		}
	}
}
