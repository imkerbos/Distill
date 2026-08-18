package policygen_test

import (
	"reflect"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"

	"github.com/imkerbos/Distill/internal/policygen"
)

func nsOf(t *testing.T, r policygen.Result, namespace string) policygen.CandidatePolicy {
	t.Helper()
	for _, p := range r.Policies {
		if p.Namespace == namespace {
			return p
		}
	}
	t.Fatalf("no policy for namespace %q", namespace)
	return policygen.CandidatePolicy{}
}

func wideningOf(t *testing.T, list []policygen.Widening, namespace string) policygen.Widening {
	t.Helper()
	for _, w := range list {
		if w.Namespace == namespace {
			return w
		}
	}
	t.Fatalf("no widening report for namespace %q", namespace)
	return policygen.Widening{}
}

// 折叠后每个 namespace 恰好一份策略，主体是整个 namespace。
//
// 303 份平铺在一屏里没人 review 得完，而一份 review 不完的推荐等于没有推荐。
func TestCollapsingGivesOnePolicyPerNamespace(t *testing.T) {
	base := policygen.Generate(observe(t, "prod-asia-1"))
	collapsed, _ := base.AtNamespaceGranularity()

	seen := map[string]int{}
	for _, p := range collapsed.Policies {
		seen[p.Namespace]++
		if p.Granularity != policygen.GranularityNamespace {
			t.Errorf("%s: Granularity = %q, want NAMESPACE", p.Namespace, p.Granularity)
		}
		if p.Workload != "" || p.WorkloadLabelKey != "" {
			t.Errorf("%s: still carries workload %q/%q; the subject is the whole namespace",
				p.Namespace, p.WorkloadLabelKey, p.Workload)
		}
	}
	for ns, n := range seen {
		if n != 1 {
			t.Errorf("namespace %s got %d policies, want 1", ns, n)
		}
	}
	if len(collapsed.Policies) >= len(base.Policies) {
		t.Errorf("collapsed to %d policies from %d — collapsing bought nothing",
			len(collapsed.Policies), len(base.Policies))
	}
}

// 折叠**只动主体，不动对端**。一条规则折叠前后逐字节相同。
//
// 对端粗化（只写 namespaceSelector）会把「放行 kube-system」变成「放行它
// 里面每一个 Pod」，那是另一个数量级的放宽，不该顺手混进来。
func TestCollapsingLeavesThePeersUntouched(t *testing.T) {
	base := policygen.Generate(observe(t, "prod-asia-1"))
	collapsed, _ := base.AtNamespaceGranularity()

	// 原始规则按指纹建索引，折叠后逐条比对端。
	want := map[string]policygen.Rule{}
	for _, p := range base.Policies {
		for _, r := range p.Rules {
			want[r.Fingerprint] = r
		}
	}
	checked := 0
	for _, p := range collapsed.Policies {
		for _, got := range p.Rules {
			orig, ok := want[got.Fingerprint]
			if !ok {
				t.Errorf("%s: rule %s appeared out of nowhere", p.Namespace, got.Fingerprint[:8])
				continue
			}
			if !reflect.DeepEqual(got.Ingress, orig.Ingress) {
				t.Errorf("%s/%s: ingress changed while collapsing", p.Namespace, got.Fingerprint[:8])
			}
			if !reflect.DeepEqual(got.Egress, orig.Egress) {
				t.Errorf("%s/%s: egress changed while collapsing", p.Namespace, got.Fingerprint[:8])
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("compared no rules at all; this case proved nothing")
	}
}

// 规则按指纹去重。一个 N 个 workload 共享同一条 DNS egress 的 namespace
// 折叠后只留 1 条 —— 不去重会灌出 N 条一模一样的规则（同
// ScrapeTargetSnapshots 那次 506 条只有 20 个指纹）。
func TestCollapsedRulesAreDedupedByFingerprint(t *testing.T) {
	base := policygen.Generate(observe(t, "prod-asia-1"))
	collapsed, _ := base.AtNamespaceGranularity()

	for _, p := range collapsed.Policies {
		seen := map[string]bool{}
		for _, r := range p.Rules {
			if seen[r.Fingerprint] {
				t.Errorf("%s: fingerprint %s appears more than once", p.Namespace, r.Fingerprint[:8])
			}
			seen[r.Fingerprint] = true
		}
	}
	// 对照：kube-system 在 fixture 里有两个 workload，两者都带 DNS 与
	// control plane 的 Baseline。折叠后这两类各只剩一条。
	ks := nsOf(t, collapsed, "kube-system")
	if len(ks.Rules) == 0 {
		t.Fatal("kube-system collapsed to zero rules")
	}
	var base2 int
	for _, p := range base.Policies {
		if p.Namespace == "kube-system" {
			base2 += len(p.Rules)
		}
	}
	if len(ks.Rules) >= base2 {
		t.Errorf("kube-system kept %d of %d rules; the shared baselines were not deduped",
			len(ks.Rules), base2)
	}
}

// **粗化只会放宽，而放宽必须报出来。**
//
// 一条原本只属于一个 workload 的放行，折叠之后该 namespace 里每个 Pod 都
// 拿到了。这是严格更宽的策略，不得无声发生。
//
// 手工构造而不是取 fixture：fixture 里唯一的多 workload namespace
// （kube-system）两个 workload 的 Baseline 完全相同，折叠天然无损 ——
// 拿它做断言，一个恒报 0 的实现照样通过。要证明这个数算得对，就必须有一条
// 规则只属于其中一个 workload（design doc §10）。
func TestCollapsingReportsHowMuchWiderItGot(t *testing.T) {
	shared := policygen.Rule{Fingerprint: "fp-shared", Enabled: true}
	only := policygen.Rule{Fingerprint: "fp-only-a", Enabled: true}
	base := policygen.Result{Policies: []policygen.CandidatePolicy{
		{
			Cluster: "c", Namespace: "app", Granularity: policygen.GranularityWorkload,
			Workload: "a", WorkloadLabelKey: "app",
			Rules: []policygen.Rule{shared, only},
		},
		{
			Cluster: "c", Namespace: "app", Granularity: policygen.GranularityWorkload,
			Workload: "b", WorkloadLabelKey: "app",
			Rules: []policygen.Rule{shared},
		},
	}}

	collapsed, widening := base.AtNamespaceGranularity()
	app := wideningOf(t, widening, "app")
	if app.Workloads != 2 {
		t.Errorf("Workloads = %d, want 2", app.Workloads)
	}
	if app.Rules != 2 {
		t.Errorf("Rules = %d, want 2 after dedup", app.Rules)
	}
	// fp-shared 两个 workload 都有 → 多 0 份；fp-only-a 只有 a 有 →
	// 折叠后 b 也拿到了 → 多 1 份。
	if app.ExtraGrants != 1 {
		t.Errorf("ExtraGrants = %d, want 1: exactly one workload gained a rule it did not have",
			app.ExtraGrants)
	}
	if got := len(collapsed.Policies); got != 1 {
		t.Errorf("collapsed to %d policies, want 1", got)
	}
}

// 对照组：每个 workload 都持有的规则，折叠没有放宽任何东西 → ExtraGrants 0。
//
// 少了这条，一个"恒报一个正数"的实现照样通过上一条 —— 而那会让操作者
// 分不出哪几个 namespace 真的值得回到 workload 粒度去看。
func TestALosslessCollapseReportsZeroExtraGrants(t *testing.T) {
	base := policygen.Generate(observe(t, "prod-asia-1"))
	_, widening := base.AtNamespaceGranularity()

	var lossless int
	for _, w := range widening {
		if w.ExtraGrants == 0 {
			lossless++
		}
	}
	if lossless == 0 {
		t.Error("no namespace reports a lossless collapse; in the fixture the single-workload " +
			"namespaces cannot possibly widen, so a zero must be reachable")
	}
}

// 单个 workload 的 namespace 折叠必然无损：没有别的 Pod 能多拿到东西。
func TestASingleWorkloadNamespaceCannotWiden(t *testing.T) {
	base := policygen.Generate(observe(t, "prod-asia-1"))
	_, widening := base.AtNamespaceGranularity()

	for _, w := range widening {
		if w.Workloads == 1 && w.ExtraGrants != 0 {
			t.Errorf("%s has one workload but reports ExtraGrants = %d; there is no second pod "+
				"to grant anything to", w.Namespace, w.ExtraGrants)
		}
	}
}

// 折叠取的是**入参里的启用集合**，因此人工决定自动被尊重：
// A 的某条规则被禁用、B 的没有 → 折叠后该规则仍在（并集）。
//
// 这条让本轮不必新造任何覆盖语义。namespace 粒度上不存在「只为这个
// namespace 禁用」这个动作 —— 它的爆炸半径与 workload 粒度的同名动作
// 完全不同，合用一个键会让两者互相污染。
func TestCollapsingTakesTheUnionOfWhatSurvivedTheOverrides(t *testing.T) {
	base := policygen.Generate(observe(t, "prod-asia-1"))

	// 找一个 namespace 里两个 workload 共有的规则，把其中一个禁掉。
	shared := ""
	var target string
	counts := map[string][]string{}
	for _, p := range base.Policies {
		if p.Namespace != "kube-system" {
			continue
		}
		for _, r := range p.Rules {
			counts[r.Fingerprint] = append(counts[r.Fingerprint], p.Workload)
		}
	}
	for fp, wls := range counts {
		if len(wls) >= 2 {
			shared, target = fp, wls[0]
			break
		}
	}
	if shared == "" {
		t.Skip("fixture has no rule shared by two workloads in kube-system")
	}

	// 就地关掉 target 上的那一条，模拟一次人工禁用之后的 Result。
	muted := base
	muted.Policies = make([]policygen.CandidatePolicy, len(base.Policies))
	for i, p := range base.Policies {
		rules := make([]policygen.Rule, len(p.Rules))
		copy(rules, p.Rules)
		if p.Namespace == "kube-system" && p.Workload == target {
			for j := range rules {
				if rules[j].Fingerprint == shared {
					rules[j].Enabled = false
				}
			}
		}
		muted.Policies[i] = p
		muted.Policies[i].Rules = rules
	}

	collapsed, _ := muted.AtNamespaceGranularity()
	ks := nsOf(t, collapsed, "kube-system")
	var found bool
	for _, r := range ks.Rules {
		if r.Fingerprint == shared {
			found = true
			if !r.Enabled {
				t.Error("the shared rule collapsed to disabled although another workload still " +
					"has it enabled; the collapse must be a union, not an intersection")
			}
		}
	}
	if !found {
		t.Error("the shared rule vanished after one workload had it disabled; other pods in the " +
			"namespace still need it")
	}
}

// 渲染：namespace 粒度的 podSelector 为空、名字是该 ns 内的常量。
func TestNamespaceGranularityRendersAnEmptyPodSelector(t *testing.T) {
	base := policygen.Generate(observe(t, "prod-asia-1"))
	collapsed, _ := base.AtNamespaceGranularity()

	policies := collapsed.EnabledPolicies()
	if len(policies) == 0 {
		t.Fatal("collapsed result rendered no policies")
	}
	names := map[string]bool{}
	for _, np := range policies {
		if len(np.Spec.PodSelector.MatchLabels) != 0 || len(np.Spec.PodSelector.MatchExpressions) != 0 {
			t.Errorf("%s/%s: podSelector is not empty, so it does not select the namespace",
				np.Namespace, np.Name)
		}
		key := np.Namespace + "/" + np.Name
		if names[key] {
			t.Errorf("%s collides with another policy in the same namespace", key)
		}
		names[key] = true
		if want := []networkingv1.PolicyType{
			networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress,
		}; !reflect.DeepEqual(np.Spec.PolicyTypes, want) {
			t.Errorf("%s: policyTypes = %v, want both", key, np.Spec.PolicyTypes)
		}
	}
}

// **本轮不得改动 workload 粒度的产物。** 缺省那一套逐字段不变。
func TestWorkloadGranularityIsUnchanged(t *testing.T) {
	base := policygen.Generate(observe(t, "prod-asia-1"))
	for _, p := range base.Policies {
		if p.Granularity != policygen.GranularityWorkload {
			t.Errorf("%s/%s: Granularity = %q, want WORKLOAD as the default",
				p.Namespace, p.Workload, p.Granularity)
		}
		if p.Workload == "" {
			t.Errorf("%s: workload granularity lost its subject", p.Namespace)
		}
	}
	for _, np := range base.EnabledPolicies() {
		if len(np.Spec.PodSelector.MatchLabels) == 0 {
			t.Errorf("%s/%s: workload granularity rendered an empty podSelector, which would "+
				"select the whole namespace", np.Namespace, np.Name)
		}
	}
}
