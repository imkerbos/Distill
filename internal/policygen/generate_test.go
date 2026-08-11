package policygen_test

import (
	"strings"
	"testing"

	"github.com/imkerbos/Distill/internal/fixture"
	"github.com/imkerbos/Distill/internal/policygen"
	"github.com/imkerbos/Distill/internal/replay"
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
		Namespaces: c.Namespaces, Observations: obs,
	}
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
		for j := range pa.Rules {
			ra, rb := pa.Rules[j], pa.Rules[j]
			if ra.Origin != rb.Origin || ra.Evidence != rb.Evidence ||
				ra.Direction != rb.Direction || ra.Enabled != rb.Enabled {
				t.Errorf("policy[%d].rule[%d] differs across runs", i, j)
			}
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

// EnabledPolicies 只吐启用规则，风险规则不得混进生效策略集。
func TestEnabledPoliciesExcludeDisabledRules(t *testing.T) {
	res := policygen.Generate(observe(t, "prod-asia-1"))
	policies := res.EnabledPolicies()
	if len(policies) == 0 {
		t.Fatal("EnabledPolicies() empty")
	}
	for _, p := range policies {
		if len(p.Spec.PolicyTypes) != 2 {
			t.Errorf("%s/%s: policyTypes = %v, want both Ingress and Egress",
				p.Namespace, p.Name, p.Spec.PolicyTypes)
		}
		if p.Spec.PodSelector.MatchLabels["app"] == "" {
			t.Errorf("%s/%s: podSelector has no app label", p.Namespace, p.Name)
		}
	}
}
