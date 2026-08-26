package replay_test

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/imkerbos/Distill/internal/replay"
)

// scopedFlow 造一条源端带标签的集群内连接。
func scopedFlow(ns, name string, labels map[string]string) replay.Flow {
	return replay.Flow{
		Source: replay.Endpoint{
			ClusterID: "c1", IP: "10.0.0.1",
			Pod: &replay.PodRef{ClusterID: "c1", Namespace: ns, Name: name, Labels: labels},
		},
		Dest: replay.Endpoint{
			ClusterID: "c1", IP: "10.0.0.2",
			Pod: &replay.PodRef{ClusterID: "c1", Namespace: "other", Name: "peer"},
		},
		Protocol: replay.ProtocolTCP, Port: 8080,
	}
}

// **只有被第二平面策略选中的主体降级，其余照常可信。**
//
// 在这之前，集群里只要存在一条 CiliumNetworkPolicy，整个集群的每一条判定
// 都会被标成 DEGRADED —— 粒度粗到等于宣布这个集群完全不可信，而实际上
// 那条 CNP 可能只选中了一个 namespace 里的一个 workload。
//
// 降级面越大，操作者越会习惯性忽略它；而这个标记的全部意义就是让他在真的
// 该停手的地方停手。
func TestOnlySubjectsCoveredByAForeignPolicyDegrade(t *testing.T) {
	ev := replay.NewEvaluator("c1", nil, []replay.NamespaceRef{
		{ClusterID: "c1", Name: "payment"}, {ClusterID: "c1", Name: "shop"},
		{ClusterID: "c1", Name: "other"},
	}, replay.WithForeignPlaneScopes([]replay.ForeignScope{{
		Namespace: "payment",
		Selector:  metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
	}}))

	covered := ev.Evaluate(scopedFlow("payment", "api-1", map[string]string{"app": "api"}))
	if covered.Confidence != replay.ConfidenceDegraded {
		t.Errorf("被 CNP 选中的主体 Confidence = %s, want DEGRADED —— "+
			"平台不解释那个平面，它的结论可能是「看起来对」的错", covered.Confidence)
	}

	// 同 namespace、不同标签：那条 CNP 选不中它。
	sibling := ev.Evaluate(scopedFlow("payment", "worker-1", map[string]string{"app": "worker"}))
	if sibling.Confidence != replay.ConfidenceTrusted {
		t.Errorf("同 namespace 但没被选中的主体也降级了（%s）—— "+
			"整片降级等于宣布这个集群完全不可信", sibling.Confidence)
	}

	// 另一个 namespace：CNP 是 namespaced 的，管不到。
	elsewhere := ev.Evaluate(scopedFlow("shop", "web-1", map[string]string{"app": "api"}))
	if elsewhere.Confidence != replay.ConfidenceTrusted {
		t.Errorf("另一个 namespace 的同名标签被降级了（%s）—— "+
			"CNP 是 namespaced，标签相同不等于被它选中", elsewhere.Confidence)
	}
}

// 集群级 scope（CiliumClusterwideNetworkPolicy）跨 namespace 生效。
func TestClusterWideForeignScopeCoversEveryNamespace(t *testing.T) {
	ev := replay.NewEvaluator("c1", nil, []replay.NamespaceRef{
		{ClusterID: "c1", Name: "payment"}, {ClusterID: "c1", Name: "shop"},
		{ClusterID: "c1", Name: "other"},
	}, replay.WithForeignPlaneScopes([]replay.ForeignScope{{
		// Namespace 为空 = 集群级。
		Selector: metav1.LabelSelector{MatchLabels: map[string]string{"tier": "edge"}},
	}}))

	for _, ns := range []string{"payment", "shop"} {
		got := ev.Evaluate(scopedFlow(ns, "x", map[string]string{"tier": "edge"}))
		if got.Confidence != replay.ConfidenceDegraded {
			t.Errorf("%s 里被集群级策略选中的主体没降级（%s）", ns, got.Confidence)
		}
	}
	free := ev.Evaluate(scopedFlow("payment", "y", map[string]string{"tier": "core"}))
	if free.Confidence != replay.ConfidenceTrusted {
		t.Errorf("集群级策略选不中的主体也降级了（%s）", free.Confidence)
	}
}

// 空 selector 选中该范围内**全部**主体，与 podSelector 语义一致。
func TestEmptyForeignSelectorCoversEverythingInScope(t *testing.T) {
	ev := replay.NewEvaluator("c1", nil, []replay.NamespaceRef{
		{ClusterID: "c1", Name: "payment"}, {ClusterID: "c1", Name: "other"},
	}, replay.WithForeignPlaneScopes([]replay.ForeignScope{{Namespace: "payment"}}))

	got := ev.Evaluate(scopedFlow("payment", "anything", nil))
	if got.Confidence != replay.ConfidenceDegraded {
		t.Errorf("空 selector 没有选中该 namespace 的全部主体（%s）—— "+
			"那是 endpointSelector: {} 的语义", got.Confidence)
	}
}

// **对端被选中时同样降级。**
//
// 判定是双向的：对端被一个平台不解释的策略管着，这条连接的结论一样不可信。
func TestForeignScopeOnThePeerAlsoDegrades(t *testing.T) {
	ev := replay.NewEvaluator("c1", nil, []replay.NamespaceRef{
		{ClusterID: "c1", Name: "payment"}, {ClusterID: "c1", Name: "other"},
	}, replay.WithForeignPlaneScopes([]replay.ForeignScope{{Namespace: "other"}}))

	got := ev.Evaluate(scopedFlow("payment", "api-1", map[string]string{"app": "api"}))
	if got.Confidence != replay.ConfidenceDegraded {
		t.Errorf("对端被 CNP 选中却没降级（%s）：判定是双向的", got.Confidence)
	}
}

// **身份解不出的端点，在存在第二平面策略时降级。**
//
// 解不出就不知道它有没有被那些策略选中，而"不知道有没有东西在覆盖我的结论"
// 承担的正是这个标记要表达的风险。
func TestUnresolvedEndpointsDegradeWhenForeignScopesExist(t *testing.T) {
	ev := replay.NewEvaluator("c1", nil, []replay.NamespaceRef{{ClusterID: "c1", Name: "payment"}},
		replay.WithForeignPlaneScopes([]replay.ForeignScope{{Namespace: "payment"}}))

	f := replay.Flow{
		Source:   replay.Endpoint{ClusterID: "c1", IP: "10.0.0.9"}, // Pod 解不出
		Dest:     replay.Endpoint{IP: "198.51.100.1"},
		Protocol: replay.ProtocolTCP, Port: 443,
	}
	if got := ev.Evaluate(f); got.Confidence != replay.ConfidenceDegraded {
		t.Errorf("身份解不出时 Confidence = %s, want DEGRADED —— "+
			"不知道它有没有被第二平面覆盖，就是不可信", got.Confidence)
	}
}

// 没有任何 scope 时不降级：一个恒降级的实现照样能让上面几条通过，
// 而那等于把这个标记变成永远为真、因而没有信息的一句话。
func TestNoForeignScopesMeansNoDegradation(t *testing.T) {
	ev := replay.NewEvaluator("c1", nil, []replay.NamespaceRef{
		{ClusterID: "c1", Name: "payment"}, {ClusterID: "c1", Name: "other"},
	})
	got := ev.Evaluate(scopedFlow("payment", "api-1", map[string]string{"app": "api"}))
	if got.Confidence != replay.ConfidenceTrusted {
		t.Errorf("没有第二平面策略却降级了（%s）", got.Confidence)
	}
}

// **「没查过」仍然整集群降级。**
//
// 三态里的 UNKNOWN 说的是"我不知道这个集群有没有第二平面"，那时任何精确
// 到主体的说法都是编出来的 —— 只能整片降级。这一条与按 scope 降级并存，
// 不是被它取代。
func TestUncheckedPlanesStillDegradeEverything(t *testing.T) {
	ev := replay.NewEvaluator("c1", nil, []replay.NamespaceRef{
		{ClusterID: "c1", Name: "payment"}, {ClusterID: "c1", Name: "other"},
	}, replay.WithForeignPlane(true))
	got := ev.Evaluate(scopedFlow("payment", "api-1", map[string]string{"app": "api"}))
	if got.Confidence != replay.ConfidenceDegraded {
		t.Errorf("平面状态未知时没有整片降级（%s）", got.Confidence)
	}
}
