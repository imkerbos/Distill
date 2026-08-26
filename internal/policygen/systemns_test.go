package policygen_test

import (
	"testing"

	"github.com/imkerbos/Distill/internal/policygen"
	"github.com/imkerbos/Distill/internal/replay"
)

func sysPods(cluster string) []replay.PodRef {
	return []replay.PodRef{
		{ClusterID: cluster, Namespace: "kube-system", Name: "coredns-1",
			Labels: map[string]string{"k8s-app": "kube-dns"}},
		{ClusterID: cluster, Namespace: "kube-public", Name: "x-1",
			Labels: map[string]string{"app": "x"}},
		{ClusterID: cluster, Namespace: "kube-node-lease", Name: "y-1",
			Labels: map[string]string{"app": "y"}},
		{ClusterID: cluster, Namespace: "payment", Name: "api-1",
			Labels: map[string]string{"app": "api"}},
	}
}

// **系统 namespace 默认不生成候选策略。**
//
// 候选集本质上是给每个 workload 装上 default-deny 再把观测到的连接放回去。
// 而观测窗口证明不了完整时，学出来的规则默认不启用 —— 于是 kube-dns 会拿到
// 一份"只放行 Baseline"的 default-deny ingress，全集群的 DNS 解析随之中断。
//
// 这不是假设：真集群上实测，kube-system/kube-dns 的候选里各 namespace 到
// UDP/53 的规则全部 enabled=false，而 dry-run 报出 14 条 DNS 会被拦断。
//
// 对最容易搞挂集群的那部分，默认不碰。
func TestSystemNamespacesGetNoCandidatesByDefault(t *testing.T) {
	res := policygen.Generate(policygen.Input{
		ClusterID: "c1", Pods: sysPods("c1"),
	})

	for _, c := range res.Policies {
		if policygen.IsSystemNamespace(c.Namespace) {
			t.Errorf("为系统 namespace %s 生成了候选策略 —— "+
				"下发它可能让整个集群失去 DNS", c.Namespace)
		}
	}
	// 业务 namespace 照常生成：一个把所有人都排除掉的实现同样能让上面通过。
	var sawBusiness bool
	for _, c := range res.Policies {
		if c.Namespace == "payment" {
			sawBusiness = true
		}
	}
	if !sawBusiness {
		t.Error("业务 namespace 也被排除了")
	}
}

// **被排除的必须报出来，不能静默消失。**
//
// 一个悄悄不见的 namespace，在界面上与"这个 namespace 没有 workload"长得
// 一样。操作者据此以为平台看过了、覆盖是完整的 —— 与 ExcludedWorkloads
// 同一条纪律。
func TestExcludedSystemNamespacesAreReported(t *testing.T) {
	res := policygen.Generate(policygen.Input{
		ClusterID: "c1", Pods: sysPods("c1"),
	})
	got := map[string]bool{}
	for _, ns := range res.ExcludedNamespaces {
		got[ns.Namespace] = true
		if ns.Reason == "" {
			t.Errorf("%s 被排除却没给原因", ns.Namespace)
		}
	}
	for _, want := range []string{"kube-system", "kube-public", "kube-node-lease"} {
		if !got[want] {
			t.Errorf("%s 被排除了却没有报出来", want)
		}
	}
}

// **显式纳入之后照常生成。**
//
// 默认不碰不等于永远不能碰：集群管理员可以明示要平台管某个系统 namespace，
// 那是一次有记录的判断。没有这条出口，这道保护就成了一堵没有门的墙。
func TestExplicitlyManagedSystemNamespaceIsGenerated(t *testing.T) {
	res := policygen.Generate(policygen.Input{
		ClusterID: "c1", Pods: sysPods("c1"),
		ManagedSystemNamespaces: []string{"kube-system"},
	})
	var sawKubeSystem bool
	for _, c := range res.Policies {
		if c.Namespace == "kube-system" {
			sawKubeSystem = true
		}
	}
	if !sawKubeSystem {
		t.Error("显式纳入的 kube-system 仍然没有生成候选策略")
	}
	// 纳入的那个不再出现在排除清单里；其余两个仍然排除。
	for _, ns := range res.ExcludedNamespaces {
		if ns.Namespace == "kube-system" {
			t.Error("显式纳入的 namespace 仍被报成排除")
		}
	}
}

// 纳入一个不存在的 namespace 不影响别的：它只是没有 Pod 而已。
func TestManagingAnUnknownNamespaceIsHarmless(t *testing.T) {
	res := policygen.Generate(policygen.Input{
		ClusterID: "c1", Pods: sysPods("c1"),
		ManagedSystemNamespaces: []string{"does-not-exist"},
	})
	if len(res.ExcludedNamespaces) != 3 {
		t.Errorf("排除清单 = %+v, want 三个系统 namespace 都还在", res.ExcludedNamespaces)
	}
}

// IsSystemNamespace 只认 Kubernetes 内置的那三个。
//
// **不把 argocd、istio-system 之类硬编码进来**：那些是用户装的，哪些算
// 基础设施只有集群管理员知道。硬编码一份清单，等于替他做一个他没做过的判断，
// 而漏掉的那个会被静默地下发 default-deny。要排除它们走的是另一条路
// （由操作者在登记里列出），不是猜。
func TestSystemNamespaceListIsOnlyTheBuiltIns(t *testing.T) {
	for _, ns := range []string{"kube-system", "kube-public", "kube-node-lease"} {
		if !policygen.IsSystemNamespace(ns) {
			t.Errorf("%s 不在系统 namespace 清单里", ns)
		}
	}
	for _, ns := range []string{"argocd", "istio-system", "monitoring", "payment", "kube-something"} {
		if policygen.IsSystemNamespace(ns) {
			t.Errorf("%s 被当成了系统 namespace —— 那是替集群管理员做了一个"+
				"他没做过的判断", ns)
		}
	}
}

// **有流量的系统命名空间同样不生成 —— 学习那条路也要挡住。**
//
// 名册那一处跳过之后，从观测流量学出来的规则会经由另一条路把主体加回名册
// （generate.go 里那句 workloads[s] = true）。只挡一处的话，kube-system 里
// 有流量的 workload 会绕回候选集，而排除清单照样报着它 ——
// **一个"报了却没生效"的保护，比没有保护更危险**：它让人以为已经防住了。
//
// 这是真集群实测发现的（2026-08-26）：excludedNamespaces 报了 kube-system，
// 而候选集里 kube-system 仍在，dry-run 照样报 14 条 DNS 会断。
func TestSystemNamespaceWithTrafficStillGetsNoCandidates(t *testing.T) {
	dns := replay.PodRef{
		ClusterID: "c1", Namespace: "kube-system", Name: "coredns-1",
		IP: "10.0.0.10", Labels: map[string]string{"k8s-app": "kube-dns"},
	}
	client := replay.PodRef{
		ClusterID: "c1", Namespace: "payment", Name: "api-1",
		IP: "10.0.0.11", Labels: map[string]string{"app": "api"},
	}
	res := policygen.Generate(policygen.Input{
		ClusterID: "c1",
		Pods:      []replay.PodRef{dns, client},
		// 一条到 kube-dns 的观测流量：它会被学成一条规则，
		// 而那条规则的主体正是 kube-system/kube-dns。
		Observations: []policygen.Observation{{
			FlowID: "f1",
			Flow: replay.Flow{
				Source:   replay.Endpoint{ClusterID: "c1", IP: client.IP, Pod: &client},
				Dest:     replay.Endpoint{ClusterID: "c1", IP: dns.IP, Pod: &dns},
				Protocol: replay.ProtocolUDP, Port: 53,
			},
			Decision:        replay.Decision{Verdict: replay.VerdictAllow, Confidence: replay.ConfidenceTrusted},
			IdentityTrusted: true,
		}},
	})

	for _, c := range res.Policies {
		if c.Namespace == "kube-system" {
			t.Errorf("kube-system 有流量时仍然生成了候选策略（%s/%s）——"+
				"学习那条路绕过了排除", c.Namespace, c.Workload)
		}
	}
	// 对照：源端所在的业务命名空间照常生成，否则一个"全都不生成"的实现
	// 也能让上面通过。
	var sawPayment bool
	for _, c := range res.Policies {
		if c.Namespace == "payment" {
			sawPayment = true
		}
	}
	if !sawPayment {
		t.Error("业务命名空间也被挡住了")
	}
}
