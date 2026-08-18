package collectstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/collectstore"
	"github.com/imkerbos/Distill/internal/flow"
	"github.com/imkerbos/Distill/internal/policygen"
	"github.com/imkerbos/Distill/internal/snapshot"
	"github.com/imkerbos/Distill/internal/snapshotstore"
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

// 候选策略在没有流量时也要给得出来。
//
// **policygen 的生成单位是 Pod 名册，不是流量**（generate.go 的注释写得很
// 明确）：Baseline 是按 workload 无条件注入的，依据是资产。挡住它的只是
// generate() 里那句 readTraffic —— 而那一句要的是一个流量窗口。
//
// 这一屏正是操作者问「那你推荐我加什么策略」时要看的东西。
func TestPolicyPreviewGivesBaselineCandidatesWithoutTraffic(t *testing.T) {
	r, s := newTestReader(t)
	ctx := context.Background()
	saveRun(t, s, "run-assets-only", firstRunAt, assetOnlyPods())

	pv, err := r.PolicyPreview(ctx, collectedID, "", store.TimeWindow{})
	if err != nil {
		t.Fatalf("PolicyPreview() error = %v, want baseline candidates built from assets", err)
	}
	if len(pv.Candidates) == 0 {
		t.Error("PolicyPreview() returned no candidates — Baseline 是按 workload " +
			"无条件注入的，名册在库里就该有")
	}
	if pv.TrafficObserved {
		t.Error("PolicyPreview().TrafficObserved = true with no ingest")
	}
}

// **没有流量时，dry-run 的数字不得被当成上线影响数。**
//
// 零条连接下每一项预测都是 0，而「会拦断 0 条连接」读起来是「可以放心下发」
// —— 那是这个平台最不能给出的错觉。零不是一次评估的结果，是没有评估过
// （real-reader design §4.1 记的就是这笔账）。
func TestPolicyPreviewWithoutTrafficDoesNotLookLikeAVerifiedZero(t *testing.T) {
	r, s := newTestReader(t)
	ctx := context.Background()
	saveRun(t, s, "run-assets-only", firstRunAt, assetOnlyPods())

	pv, err := r.PolicyPreview(ctx, collectedID, "", store.TimeWindow{})
	if err != nil {
		t.Fatalf("PolicyPreview() error = %v", err)
	}
	if pv.Prediction.TotalEvaluated != 0 {
		t.Errorf("Prediction.TotalEvaluated = %d with no traffic, want 0",
			pv.Prediction.TotalEvaluated)
	}
	// 完整度不得是 COMPLETE：一次都没观测过，说不出这段时间漏没漏。
	if pv.WindowCompleteness == flow.CompletenessComplete {
		t.Errorf("WindowCompleteness = %q with no ingest — 宣称了一个没建立过的完整度",
			pv.WindowCompleteness)
	}
}

// 人工逐条确认必须能落在采集集群上。
//
// **此前 EnsureRuleExists 在这个 Reader 上一律拒绝**（notyet.go：「尚未接到
// 采集数据上」）。于是操作者在 UAT 上看得见 304 条推荐，却一条都确认不了 ——
// 而「人工逐条确认」正是需求第三段的核心动作。
//
// 校验逻辑与 fixture 侧逐字同源：指纹要在当前候选集里对得上，BASELINE 来源
// 的规则不许 DISABLE。共用同一个 generate，两个端点因此不可能对着不同的
// 候选集给出互相矛盾的答案。
func TestEnsureRuleExistsWorksOnACollectedCluster(t *testing.T) {
	r, s := newTestReader(t)
	ctx := context.Background()
	// 带上 kube-dns 的 Service 与后端：DNS Baseline 的依据就是它
	// （baseline/derive_dns.go）。没有依据资产时候选策略只是一份
	// default-deny，一条规则都不带 —— 那样这条用例证明不了任何事。
	saveRunWithDNS(t, s, "run-assets-only", firstRunAt, assetOnlyPods())

	pv, err := r.PolicyPreview(ctx, collectedID, "", store.TimeWindow{})
	if err != nil {
		t.Fatalf("PolicyPreview() error = %v", err)
	}
	var ns, wl, fp string
	var origin policygen.RuleOrigin
	for _, p := range pv.Candidates {
		for _, rule := range p.Rules {
			ns, wl, fp, origin = p.Namespace, p.Workload, rule.Fingerprint, rule.Origin
			break
		}
		if fp != "" {
			break
		}
	}
	if fp == "" {
		t.Fatal("候选集里一条规则都没有，这条用例证明不了任何事")
	}

	// 一条真实存在的规则：确认（ENABLE）必须通过。
	if err := r.EnsureRuleExists(ctx, collectedID, ns, wl, fp,
		policygen.DecisionEnable, store.TimeWindow{}); err != nil {
		t.Errorf("EnsureRuleExists(existing rule) = %v, want nil", err)
	}

	// 一个对不上的指纹必须被拒：拿着过期页面提交的覆盖写进去不会报错，
	// 只会永远待在「已失效」那一节，而它从来就没生效过。
	err = r.EnsureRuleExists(ctx, collectedID, ns, wl, "not-a-real-fingerprint",
		policygen.DecisionEnable, store.TimeWindow{})
	if err == nil {
		t.Error("EnsureRuleExists(stale fingerprint) = nil, want a rejection")
	}

	// BASELINE 来源的规则不许 DISABLE：policygen.Apply 面对同一种输入本就会
	// 把它判成失效，这里只是把同一个必然结论挪到写库之前。
	if origin == policygen.OriginBaseline {
		err = r.EnsureRuleExists(ctx, collectedID, ns, wl, fp,
			policygen.DecisionDisable, store.TimeWindow{})
		if !errors.Is(err, policygen.ErrBaselineNotDisablable) {
			t.Errorf("EnsureRuleExists(disable a baseline) = %v, want ErrBaselineNotDisablable", err)
		}
	}
}

// saveRunWithDNS 落一次带 kube-dns 依据的采集。
func saveRunWithDNS(
	t *testing.T, s *snapshotstore.Store, runID string, at time.Time, pods []snapshot.Pod,
) {
	t.Helper()
	run := snapshot.Run{
		Status:     snapshot.RunOK,
		StartedAt:  at.Add(-30 * time.Second),
		FinishedAt: at.Add(5 * time.Second),
		Observation: snapshot.Observation{
			ClusterID: collectedID, RunID: runID, ObservedAt: at,
			Namespaces: []snapshot.Namespace{
				{ClusterID: collectedID, Name: "payment"},
				{ClusterID: collectedID, Name: "shop"},
				{ClusterID: collectedID, Name: "kube-system"},
			},
			Pods: pods,
			Services: []snapshot.Service{{
				ClusterID: collectedID, Namespace: "kube-system", Name: "kube-dns",
				Type: "ClusterIP", Selector: map[string]string{"k8s-app": "kube-dns"},
				ClusterIP: "10.8.0.10",
				Ports: []snapshot.ServicePort{
					{Name: "dns", Port: 53, TargetPort: 53, Protocol: "UDP"},
				},
			}},
			Endpoints: []snapshot.Endpoints{{
				ClusterID: collectedID, Namespace: "kube-system", Name: "kube-dns",
				Addresses: []string{"10.4.9.9"}, Ports: []int32{53},
			}},
		},
	}
	ctx := context.Background()
	if err := s.Save(ctx, run); err != nil {
		t.Fatalf("Save(%s) error = %v", runID, err)
	}
	if err := s.DeriveIdentityIntervals(ctx, collectedID, runID); err != nil {
		t.Fatalf("DeriveIdentityIntervals(%s) error = %v", runID, err)
	}
}
