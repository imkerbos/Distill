package policygen_test

import (
	"testing"
	"time"

	networkingv1 "k8s.io/api/networking/v1"

	"github.com/imkerbos/Distill/internal/policygen"
	"github.com/imkerbos/Distill/internal/replay"
)

func windowResult() policygen.Result {
	r := ingressRule()
	r.Fingerprint = policygen.FingerprintOf(r)
	return policygen.Result{Policies: []policygen.CandidatePolicy{{
		Cluster: "c1", Namespace: "devops", Workload: "nacos",
		WorkloadLabelKey: "app.kubernetes.io/name",
		Rules:            []policygen.Rule{r},
	}}}
}

// learnedFor 造一条累积规则：同一个 workload，另一个对端。
func learnedFor(ns, wl, peerNS string, seen time.Time) policygen.LearnedRule {
	r := ingressRule()
	r.Ingress.From[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] = peerNS
	return policygen.LearnedRule{
		Namespace: ns, Workload: wl, Fingerprint: policygen.FingerprintOf(r),
		LastSeen: seen, Observations: 37, Rule: r,
	}
}

// **只增不减。** 窗口里学到的规则一条都不许动，累积只能让候选集更宽。
//
// 反过来会造成阻断：合并本该补上窗口没看见的放行，如果它同时删掉了窗口里
// 刚学到的一条，那条链路就在下发后断了——而 dry-run 算的是合并后的策略集，
// 报不出这件事。
func TestMergeOnlyAdds(t *testing.T) {
	base := windowResult()
	before := base.Policies[0].Rules[0].Fingerprint
	seen := time.Date(2026, 8, 29, 3, 0, 0, 0, time.UTC)

	got, unobserved := policygen.MergeLearned(base,
		[]policygen.LearnedRule{learnedFor("devops", "nacos", "g32-merchant", seen)})

	if len(got.Policies) != 1 {
		t.Fatalf("候选策略数变了: %d", len(got.Policies))
	}
	rules := got.Policies[0].Rules
	if len(rules) != 2 {
		t.Fatalf("规则数 = %d, want 2（窗口 1 条 + 累积 1 条）", len(rules))
	}
	if rules[0].Fingerprint != before {
		t.Errorf("窗口里那条被动了: %s → %s", before, rules[0].Fingerprint)
	}
	if len(unobserved) != 1 || unobserved[0].Workload != "nacos" {
		t.Fatalf("未观测清单不对: %+v", unobserved)
	}
	if !unobserved[0].LastSeen.Equal(seen) {
		t.Errorf("LastSeen 没带上 —— 界面就说不出这条规则有多久没见过了")
	}
}

// 窗口里已经有的规则不重复添加：同一条规则出现两遍，人工确认只对其中一条
// 生效，而界面上那两行长得一模一样。
func TestMergeDoesNotDuplicate(t *testing.T) {
	base := windowResult()
	same := base.Policies[0].Rules[0]
	got, unobserved := policygen.MergeLearned(base, []policygen.LearnedRule{{
		Namespace: "devops", Workload: "nacos", Fingerprint: same.Fingerprint,
		LastSeen: time.Now().UTC(), Observations: 9, Rule: same,
	}})
	if n := len(got.Policies[0].Rules); n != 1 {
		t.Errorf("规则数 = %d, want 1 —— 同一条规则被并进去两遍", n)
	}
	if len(unobserved) != 0 {
		t.Errorf("窗口里出现过的规则不该进未观测清单: %+v", unobserved)
	}
}

// **不在当前花名册里的 workload 不生成策略。**
//
// 这里拿不到它的 WorkloadLabelKey，凭空补一个会产出一条 selector 选不中任何
// Pod 的策略——在 NetworkPolicy 语义下那等于一条 default-deny，方向完全反了。
func TestMergeSkipsWorkloadsWithNoCandidatePolicy(t *testing.T) {
	got, unobserved := policygen.MergeLearned(windowResult(),
		[]policygen.LearnedRule{learnedFor("gone-ns", "retired", "g32-base", time.Now().UTC())})
	if len(got.Policies) != 1 {
		t.Errorf("给一个不在花名册里的 workload 造了策略: %+v", got.Policies)
	}
	if len(unobserved) != 0 {
		t.Errorf("没并进去的规则不该进未观测清单: %+v", unobserved)
	}
}

// 调用方那一份不许被穿透修改：Result 里 Policies 与 Rules 都是切片，
// 直接 append 会写回调用方的底层数组。
func TestMergeDoesNotMutateItsInput(t *testing.T) {
	base := windowResult()
	// **容量刻意留富余。** len == cap 时 append 必然重新分配，穿透写不回去，
	// 于是"忘了深拷贝"这个缺陷在测试里根本不出现。留出容量才让这条用例
	// 真的能失败——一个抓不到缺陷的守卫，其存在本身是误导。
	spacious := make([]policygen.Rule, 1, 4)
	spacious[0] = base.Policies[0].Rules[0]
	base.Policies[0].Rules = spacious
	sentinel := base.Policies[0].Rules[:1]

	policygen.MergeLearned(base,
		[]policygen.LearnedRule{learnedFor("devops", "nacos", "g32-merchant", time.Now().UTC())})

	if n := len(base.Policies[0].Rules); n != 1 {
		t.Errorf("入参被改了: 规则数 = %d, want 1", n)
	}
	// 底层数组也不许被写：append 到共享数组上时长度不变、内容却被覆盖了。
	if got := sentinel[:cap(sentinel)][1:2]; got[0].Fingerprint != "" {
		t.Errorf("入参的底层数组被写进了一条规则: %s", got[0].Fingerprint)
	}
}

// 风险端口在合并时**重算**，不从库里取。
//
// 存下来的那一份会在风险清单更新之后过期，而过期的方向是"这个端口不再算
// 风险"——一条本该扣住等人确认的规则会自动启用。
func TestMergeRecomputesRiskAndEnabled(t *testing.T) {
	r := ingressRule()
	r.Ingress.Ports[0].Port.IntVal = 6379 // Redis，在风险清单里
	r.Evidence = policygen.EvidenceTrustedAllow
	fp := policygen.FingerprintOf(r)

	got, _ := policygen.MergeLearned(windowResult(), []policygen.LearnedRule{{
		Namespace: "devops", Workload: "nacos", Fingerprint: fp,
		LastSeen: time.Now().UTC(), Observations: 5, Rule: r,
	}})
	var merged *policygen.Rule
	for i := range got.Policies[0].Rules {
		if got.Policies[0].Rules[i].Fingerprint == fp {
			merged = &got.Policies[0].Rules[i]
		}
	}
	if merged == nil {
		t.Fatal("规则没并进去")
	}
	if merged.Risk == nil {
		t.Fatal("6379 没被标成风险端口 —— 它会绕过人工确认自动启用")
	}
	if merged.Enabled {
		t.Error("风险端口的规则默认就启用了 —— 判据不因证据变多而放宽")
	}
	if merged.FlowCount != 5 {
		t.Errorf("FlowCount = %d, want 5（累计观测数，不是某个旧窗口的计数）", merged.FlowCount)
	}
}

// 展示串按规则体重新渲染，不沿用存下来的那一份。
func TestMergeRendersDisplayStrings(t *testing.T) {
	l := learnedFor("devops", "nacos", "g32-merchant", time.Now().UTC())
	got, _ := policygen.MergeLearned(windowResult(), []policygen.LearnedRule{l})
	for _, r := range got.Policies[0].Rules {
		if r.Fingerprint != l.Fingerprint {
			continue // 窗口里那条的展示串由 Generate 渲染，不归这里管
		}
		if len(r.Peers) == 0 || len(r.Ports) == 0 {
			t.Errorf("并进来的规则展示串是空的，界面上这一行会什么都不显示: %+v", r)
		}
		if r.Peers[0] == "" {
			t.Errorf("对端渲染成了空串: %+v", r.Peers)
		}
	}
}

// 没有累积规则时原样返回，且未观测清单为空而不是 nil 之外的东西。
func TestMergeWithNothingLearnedIsANoop(t *testing.T) {
	base := windowResult()
	got, unobserved := policygen.MergeLearned(base, nil)
	if len(got.Policies[0].Rules) != 1 || len(unobserved) != 0 {
		t.Errorf("空输入下结果变了: rules=%d unobserved=%d",
			len(got.Policies[0].Rules), len(unobserved))
	}
}

var _ = networkingv1.NetworkPolicyIngressRule{}
var _ = replay.DirectionIngress
