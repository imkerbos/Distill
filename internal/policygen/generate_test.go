package policygen_test

import (
	"encoding/json"
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"

	"github.com/imkerbos/Distill/internal/baseline"
	"github.com/imkerbos/Distill/internal/fixture"
	"github.com/imkerbos/Distill/internal/policygen"
	"github.com/imkerbos/Distill/internal/replay"
	"github.com/imkerbos/Distill/internal/risk"
)

// observe 用真实 fixture 数据与真实求值引擎构造观测集，
// 避免手写 Decision 与引擎的实际输出漂移。
func observe(t *testing.T, clusterID string) policygen.Input {
	t.Helper()
	f := fixture.Load()
	c, ok := f.Cluster(clusterID)
	if !ok {
		t.Fatalf("cluster %q missing", clusterID)
	}
	var nss []replay.NamespaceRef
	for _, cl := range f.Clusters {
		nss = append(nss, cl.Namespaces...)
	}
	ev := replay.NewEvaluator(clusterID, c.Policies, nss,
		replay.WithCCNPPresent(c.CCNPPresent))
	var obs []policygen.Observation
	for _, fl := range f.Flows {
		obs = append(obs, policygen.Observation{
			FlowID: fl.ID, Flow: fl.Flow, Decision: ev.Evaluate(fl.Flow),
		})
	}
	return policygen.Input{
		ClusterID: clusterID, Assets: c.Assets,
		Namespaces: c.Namespaces, Pods: c.Pods, Observations: obs,
	}
}

// ruleContent 是一条规则参与决定性比较的全部字段。
//
// policygen.Rule 的 Ingress/Egress 字段带 json:"-"：直接 json.Marshal 一个
// Rule 会静默漏掉正是本测试要盯的那部分——对端、端口、协议——那两个字段
// 正是排序键漏掉时会重排的地方。分开列一份不带这个 tag 的字段集合，
// 逐字段搬过来，才能让序列化真的覆盖到会变的内容。
type ruleContent struct {
	Origin      policygen.RuleOrigin
	Evidence    policygen.EvidenceClass
	Baseline    *baseline.Kind
	Derivations []baseline.Derivation
	Risk        *risk.Port
	Enabled     bool
	Direction   replay.Direction
	FlowCount   int
	Ingress     *networkingv1.NetworkPolicyIngressRule
	Egress      *networkingv1.NetworkPolicyEgressRule
}

// policyContent 序列化一条候选策略的完整内容，供两次 Generate 的输出
// 逐字节比较。比较序列化结果而不是挑几个字段比对，是因为"同一份输入
// 两次生成必须逐字节相同"这句话里的"字节"不是修辞——少比对一个会变的
// 字段，这条断言就退化成只测它比对的那几个字段恰好没坏。
func policyContent(t *testing.T, p policygen.CandidatePolicy) string {
	t.Helper()
	rules := make([]ruleContent, len(p.Rules))
	for i, r := range p.Rules {
		rules[i] = ruleContent{
			Origin: r.Origin, Evidence: r.Evidence, Baseline: r.Baseline,
			Derivations: r.Derivations, Risk: r.Risk, Enabled: r.Enabled,
			Direction: r.Direction, FlowCount: r.FlowCount,
			Ingress: r.Ingress, Egress: r.Egress,
		}
	}
	b, err := json.Marshal(struct {
		Cluster, Namespace, Workload string
		Rules                        []ruleContent
	}{p.Cluster, p.Namespace, p.Workload, rules})
	if err != nil {
		t.Fatalf("marshal policy content: %v", err)
	}
	return string(b)
}

// 同一份输入两次生成必须逐字节相同。否则产物 diff 全是噪声，review 失效。
func TestGenerateIsDeterministic(t *testing.T) {
	in := observe(t, "prod-asia-1")
	a, b := policygen.Generate(in), policygen.Generate(in)
	if len(a.Policies) != len(b.Policies) {
		t.Fatalf("policy counts differ: %d vs %d", len(a.Policies), len(b.Policies))
	}
	for i := range a.Policies {
		pa, pb := a.Policies[i], b.Policies[i]
		if pa.Cluster != pb.Cluster || pa.Namespace != pb.Namespace || pa.Workload != pb.Workload {
			t.Fatalf("policy[%d] identity differs: %+v vs %+v", i, pa, pb)
		}
		if len(pa.Rules) != len(pb.Rules) {
			t.Fatalf("policy[%d] rule counts differ: %d vs %d", i, len(pa.Rules), len(pb.Rules))
		}
		ca, cb := policyContent(t, pa), policyContent(t, pb)
		if ca != cb {
			t.Errorf("policy[%d] (%s/%s) content differs across runs:\n  run A: %s\n  run B: %s",
				i, pa.Namespace, pa.Workload, ca, cb)
		}
	}
}

// 每条候选策略都必须带上 Baseline，且 Baseline 一条不落。
// spec §7.1：候选策略生成时必须自动包含 Baseline，且不得被学习结果覆盖。
func TestEveryPolicyCarriesBaselineRules(t *testing.T) {
	res := policygen.Generate(observe(t, "prod-asia-1"))
	if len(res.Policies) == 0 {
		t.Fatal("no candidate policies generated")
	}
	for _, p := range res.Policies {
		baselines := 0
		for _, r := range p.Rules {
			if r.Origin == policygen.OriginBaseline {
				baselines++
				if len(r.Derivations) == 0 {
					t.Errorf("%s/%s: baseline rule without derivation", p.Namespace, p.Workload)
				}
				if !r.Enabled {
					t.Errorf("%s/%s: baseline rule disabled; baselines are mandatory",
						p.Namespace, p.Workload)
				}
			}
		}
		if baselines == 0 {
			t.Errorf("%s/%s: no baseline rules injected", p.Namespace, p.Workload)
		}
	}
}

// 风险规则必须生成、必须可见、必须默认不启用。
func TestRiskyRulesAreGeneratedButDisabled(t *testing.T) {
	res := policygen.Generate(observe(t, "prod-asia-1"))
	risky, riskyEnabled := 0, 0
	for _, p := range res.Policies {
		for _, r := range p.Rules {
			if r.Risk == nil {
				continue
			}
			risky++
			if r.Enabled {
				riskyEnabled++
			}
		}
	}
	if risky == 0 {
		t.Fatal("no risky rules generated; the fixture contains MySQL and SSH traffic")
	}
	if riskyEnabled != 0 {
		t.Errorf("%d risky rules enabled, want 0 — a risky rule must never enter the default set", riskyEnabled)
	}
}

// TRUSTED_DENY / INTERNET_EGRESS / CROSS_CLUSTER 一律不默认启用。
func TestOnlyTrustedAllowRulesAreEnabled(t *testing.T) {
	res := policygen.Generate(observe(t, "prod-asia-1"))
	for _, p := range res.Policies {
		for _, r := range p.Rules {
			if r.Origin != policygen.OriginLearned || !r.Enabled {
				continue
			}
			if r.Evidence != policygen.EvidenceTrustedAllow {
				t.Errorf("%s/%s: enabled rule with evidence %q, want TRUSTED_ALLOW only",
					p.Namespace, p.Workload, r.Evidence)
			}
			if r.Risk != nil {
				t.Errorf("%s/%s: enabled rule hits risk port %d",
					p.Namespace, p.Workload, r.Risk.Port)
			}
		}
	}
}

// 四类不可生成原因在 fixture 上都要真实出现过，否则那些分支从未被验证。
func TestUngeneratableCoversAllFourReasons(t *testing.T) {
	seen := map[policygen.UngeneratableReason]bool{}
	for _, cluster := range []string{"prod-asia-1", "prod-eu-1"} {
		for _, item := range policygen.Generate(observe(t, cluster)).Ungeneratable {
			if !item.Reason.Valid() {
				t.Errorf("unregistered reason %q", item.Reason)
			}
			seen[item.Reason] = true
		}
	}
	for _, want := range policygen.AllUngeneratableReasons() {
		if !seen[want] {
			t.Errorf("reason %q never occurs in the fixture; that branch is unverified", want)
		}
	}
}

// legacy-unlabelled 必须被点名，不能只报一个总数。
// 只说"有 3 条流量表达不了"，没人知道该去给哪个 Pod 补标签。
func TestLabelLessPodIsNamedInDetail(t *testing.T) {
	found := false
	for _, item := range policygen.Generate(observe(t, "prod-asia-1")).Ungeneratable {
		if item.Reason == policygen.ReasonNoWorkloadLabel &&
			strings.Contains(item.Detail, "legacy-unlabelled") {
			found = true
		}
	}
	if !found {
		t.Error("legacy-unlabelled not named in any NO_WORKLOAD_LABEL detail")
	}
}

// ingressSig 与 egressSig 把一条规则的对端、端口、协议序列化成一个匹配键。
//
// EnabledPolicies() 直接把 rule.Ingress / rule.Egress 的值原样拷贝进输出，
// 不做任何改写，所以序列化结果逐字节相等就能确认"这是同一条规则"，
// 不需要另外手写一套字段比对。
func ingressSig(r *networkingv1.NetworkPolicyIngressRule) string {
	b, err := json.Marshal(r)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func egressSig(r *networkingv1.NetworkPolicyEgressRule) string {
	b, err := json.Marshal(r)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// EnabledPolicies 只吐启用规则，风险规则不得混进生效策略集。
//
// 这条断言是承重的：Task 7 的 dry-run 直接回放 EnabledPolicies() 吐出的
// 内容，一条禁用规则漏进去，dry-run 回答的就不再是"按默认推荐上线会
// 怎样"，而是"把所有已知敞口都放开会怎样"——方向是危险的那一头。因此
// 不能只查 PolicyTypes 和 podSelector 这类元信息，必须逐条核对
// Spec.Ingress / Spec.Egress 里的每条规则都能在源策略里找到对应的
// Enabled: true 规则，且没有一条 Enabled: false 的规则混进来；还要正向
// 核对数量——只查"没有不该出现的"，在函数什么都不吐的时候一样会通过。
func TestEnabledPoliciesExcludeDisabledRules(t *testing.T) {
	res := policygen.Generate(observe(t, "prod-asia-1"))
	policies := res.EnabledPolicies()
	if len(policies) == 0 {
		t.Fatal("EnabledPolicies() empty")
	}

	bySubject := map[[2]string]policygen.CandidatePolicy{}
	for _, p := range res.Policies {
		bySubject[[2]string{p.Namespace, p.Workload}] = p
	}

	sawDisabledRule := false
	for _, p := range policies {
		if len(p.Spec.PolicyTypes) != 2 {
			t.Errorf("%s/%s: policyTypes = %v, want both Ingress and Egress",
				p.Namespace, p.Name, p.Spec.PolicyTypes)
		}
		workload := p.Spec.PodSelector.MatchLabels["app"]
		if workload == "" {
			t.Errorf("%s/%s: podSelector has no app label", p.Namespace, p.Name)
			continue
		}

		src, ok := bySubject[[2]string{p.Namespace, workload}]
		if !ok {
			t.Fatalf("%s/%s: emitted policy has no matching CandidatePolicy", p.Namespace, workload)
		}

		enabledIngress, disabledIngress := map[string]bool{}, map[string]bool{}
		enabledEgress, disabledEgress := map[string]bool{}, map[string]bool{}
		wantIngress, wantEgress := 0, 0
		for _, srcRule := range src.Rules {
			if !srcRule.Enabled {
				sawDisabledRule = true
			}
			switch {
			case srcRule.Ingress != nil:
				sig := ingressSig(srcRule.Ingress)
				if srcRule.Enabled {
					enabledIngress[sig] = true
					wantIngress++
				} else {
					disabledIngress[sig] = true
				}
			case srcRule.Egress != nil:
				sig := egressSig(srcRule.Egress)
				if srcRule.Enabled {
					enabledEgress[sig] = true
					wantEgress++
				} else {
					disabledEgress[sig] = true
				}
			}
		}

		for _, emitted := range p.Spec.Ingress {
			sig := ingressSig(&emitted)
			if disabledIngress[sig] {
				t.Errorf("%s/%s: disabled ingress rule leaked into EnabledPolicies(): %s",
					p.Namespace, workload, sig)
			}
			if !enabledIngress[sig] {
				t.Errorf("%s/%s: emitted ingress rule has no matching enabled source rule: %s",
					p.Namespace, workload, sig)
			}
		}
		for _, emitted := range p.Spec.Egress {
			sig := egressSig(&emitted)
			if disabledEgress[sig] {
				t.Errorf("%s/%s: disabled egress rule leaked into EnabledPolicies(): %s",
					p.Namespace, workload, sig)
			}
			if !enabledEgress[sig] {
				t.Errorf("%s/%s: emitted egress rule has no matching enabled source rule: %s",
					p.Namespace, workload, sig)
			}
		}

		if len(p.Spec.Ingress) != wantIngress {
			t.Errorf("%s/%s: emitted %d ingress rules, want %d (count of enabled ingress rules)",
				p.Namespace, workload, len(p.Spec.Ingress), wantIngress)
		}
		if len(p.Spec.Egress) != wantEgress {
			t.Errorf("%s/%s: emitted %d egress rules, want %d (count of enabled egress rules)",
				p.Namespace, workload, len(p.Spec.Egress), wantEgress)
		}
	}

	if !sawDisabledRule {
		t.Fatal("no disabled rule found in any candidate policy; this test proves nothing without one")
	}
}

// checkout（mesh 内，全流量 DEGRADED）与 legacy（全部入向被写坏的
// ipBlock 策略挡成 UNKNOWN）两个 workload 没有任何可学习的流量，但
// 候选策略的生成单位是 Pod 名册而非流量：零学习规则也必须拿到强制
// Baseline，否则"平台学不出它的流量"这个信号会被一个从未生成的策略
// 悄悄吞掉。
func TestWorkloadsWithNoLearnableTrafficStillGetBaselines(t *testing.T) {
	res := policygen.Generate(observe(t, "prod-asia-1"))
	for _, want := range []struct{ ns, workload string }{
		{"checkout", "checkout"},
		{"legacy", "legacy"},
	} {
		var found *policygen.CandidatePolicy
		for i := range res.Policies {
			if res.Policies[i].Namespace == want.ns && res.Policies[i].Workload == want.workload {
				found = &res.Policies[i]
				break
			}
		}
		if found == nil {
			t.Fatalf("%s/%s: no candidate policy generated despite having pods", want.ns, want.workload)
		}
		baselines := 0
		for _, r := range found.Rules {
			if r.Origin == policygen.OriginBaseline {
				baselines++
			}
			if r.Origin == policygen.OriginLearned {
				t.Errorf("%s/%s: unexpected learned rule; this workload has no classifiable traffic",
					want.ns, want.workload)
			}
		}
		if baselines == 0 {
			t.Errorf("%s/%s: no baseline rules injected", want.ns, want.workload)
		}
	}
}

// hostNetwork Pod（如 kube-proxy）选不中，没有 app 标签的 Pod 无法用
// podSelector 表达；名册生成必须把两者都排除在外，否则会生成一条谁都
// 匹配不到、或者选中了不该选对象的幽灵策略。
func TestUnmanagedAndLabelLessWorkloadsGetNoPolicy(t *testing.T) {
	res := policygen.Generate(observe(t, "prod-asia-1"))
	for _, p := range res.Policies {
		if p.Namespace == "kube-system" && p.Workload == "kube-proxy" {
			t.Errorf("hostNetwork workload kube-proxy got a candidate policy; NetworkPolicy can't select it")
		}
		if p.Workload == "" {
			t.Errorf("empty workload got a candidate policy: %+v", p)
		}
	}
}
