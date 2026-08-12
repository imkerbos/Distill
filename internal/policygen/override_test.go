package policygen_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/baseline"
	"github.com/imkerbos/Distill/internal/policygen"
)

func TestOverrideDecisionEnumIsClosed(t *testing.T) {
	all := policygen.AllOverrideDecisions()
	if len(all) != 2 {
		t.Fatalf("AllOverrideDecisions() = %d entries, want 2", len(all))
	}
	for _, d := range all {
		if !d.Valid() {
			t.Errorf("registered decision %q reported invalid", d)
		}
	}
	if policygen.OverrideDecision("SKIP").Valid() {
		t.Error("unregistered decision reported valid")
	}
	if policygen.DecisionEnable != "ENABLE" || policygen.DecisionDisable != "DISABLE" {
		t.Errorf("literals drifted: %q / %q", policygen.DecisionEnable, policygen.DecisionDisable)
	}
}

// 指纹只覆盖规则内容。FlowCount 每天都在变，把它算进去会让
// 每一次重新生成都作废掉全部人工确认 —— 这个机制在真实环境里
// 第一天就不可用。
func TestFingerprintIgnoresFlowCount(t *testing.T) {
	in := observe(t, "prod-asia-1")
	res := policygen.Generate(in)
	if len(res.Policies) == 0 || len(res.Policies[0].Rules) == 0 {
		t.Fatal("no rules generated")
	}
	r := res.Policies[0].Rules[0]
	if r.Fingerprint == "" {
		t.Fatal("Fingerprint is empty")
	}
	bumped := r
	bumped.FlowCount = r.FlowCount + 1000
	if got := policygen.FingerprintOf(bumped); got != r.Fingerprint {
		t.Errorf("fingerprint changed with FlowCount: %s vs %s", got, r.Fingerprint)
	}
}

// 内容变了指纹必须变，否则「确认的是 MySQL，重新生成后变成 SSH，
// 覆盖仍在」这种情况无法被发现。
func TestFingerprintChangesWithContent(t *testing.T) {
	in := observe(t, "prod-asia-1")
	r := policygen.Generate(in).Policies[0].Rules[0]

	changed := r
	changed.Ports = append([]string{}, r.Ports...)
	if len(changed.Ports) == 0 {
		t.Skip("rule has no ports to mutate")
	}
	changed.Ports[0] = "TCP/9999"
	if policygen.FingerprintOf(changed) == r.Fingerprint {
		t.Error("fingerprint unchanged after a port change")
	}

	changed2 := r
	changed2.Peers = append([]string{}, r.Peers...)
	if len(changed2.Peers) > 0 {
		changed2.Peers[0] = "somewhere/else"
		if policygen.FingerprintOf(changed2) == r.Fingerprint {
			t.Error("fingerprint unchanged after a peer change")
		}
	}
}

// Apply 不得修改入参：默认推荐与人工版本必须能并列展示，
// 改写原对象会让「平台推荐了什么」这个问题失去答案。
func TestApplyDoesNotMutateInput(t *testing.T) {
	base := policygen.Generate(observe(t, "prod-asia-1"))
	before := deepCopyResult(base)

	var target policygen.Rule
	var ns, wl string
	for _, p := range base.Policies {
		for _, r := range p.Rules {
			if r.Origin == policygen.OriginLearned && !r.Enabled {
				target, ns, wl = r, p.Namespace, p.Workload
			}
		}
	}
	if target.Fingerprint == "" {
		t.Fatal("fixture produced no disabled learned rule")
	}

	_, _ = policygen.Apply(base, []policygen.Override{{
		Namespace: ns, Workload: wl, Fingerprint: target.Fingerprint,
		Decision: policygen.DecisionEnable, Reason: "对账任务",
		DecidedBy: "admin", DecidedAt: time.Now().UTC(),
	}})

	if !reflect.DeepEqual(base, before) {
		t.Error("Apply mutated its input")
	}
}

func TestApplyEnablesAndDisables(t *testing.T) {
	base := policygen.Generate(observe(t, "prod-asia-1"))

	var disabled, enabled policygen.Rule
	var dns, dwl, ens, ewl string
	for _, p := range base.Policies {
		for _, r := range p.Rules {
			if r.Origin != policygen.OriginLearned {
				continue
			}
			if !r.Enabled && disabled.Fingerprint == "" {
				disabled, dns, dwl = r, p.Namespace, p.Workload
			}
			if r.Enabled && enabled.Fingerprint == "" {
				enabled, ens, ewl = r, p.Namespace, p.Workload
			}
		}
	}
	if disabled.Fingerprint == "" || enabled.Fingerprint == "" {
		t.Fatal("fixture lacks both a disabled and an enabled learned rule")
	}

	out, stale := policygen.Apply(base, []policygen.Override{
		{Namespace: dns, Workload: dwl, Fingerprint: disabled.Fingerprint,
			Decision: policygen.DecisionEnable, Reason: "r", DecidedBy: "admin"},
		{Namespace: ens, Workload: ewl, Fingerprint: enabled.Fingerprint,
			Decision: policygen.DecisionDisable, Reason: "r", DecidedBy: "admin"},
	})
	if len(stale) != 0 {
		t.Fatalf("stale = %+v, want none", stale)
	}
	if !ruleEnabled(out, dns, dwl, disabled.Fingerprint) {
		t.Error("ENABLE did not take effect")
	}
	if ruleEnabled(out, ens, ewl, enabled.Fingerprint) {
		t.Error("DISABLE did not take effect")
	}
}

// 指纹对不上的覆盖必须进失效清单，不得静默丢弃。
func TestApplyReportsStaleOverride(t *testing.T) {
	base := policygen.Generate(observe(t, "prod-asia-1"))
	ns, wl := base.Policies[0].Namespace, base.Policies[0].Workload

	_, stale := policygen.Apply(base, []policygen.Override{{
		Namespace: ns, Workload: wl,
		Fingerprint: "0000000000000000000000000000000000000000000000000000000000000000",
		Decision:    policygen.DecisionEnable, Reason: "r", DecidedBy: "admin",
	}})
	if len(stale) != 1 {
		t.Fatalf("stale = %d entries, want 1", len(stale))
	}
	if len(stale[0].CurrentRules) == 0 {
		t.Error("CurrentRules empty; the operator cannot see what that workload holds now")
	}
}

// workload 整个消失时 CurrentRules 为空切片 —— 那本身就是答案，
// 说明这条确认失效是因为 workload 没了，不是因为规则内容变了。
func TestStaleOverrideOnVanishedWorkloadHasNoCurrentRules(t *testing.T) {
	empty := policygen.Result{Policies: []policygen.CandidatePolicy{}}

	_, stale := policygen.Apply(empty, []policygen.Override{{
		Namespace: "gone", Workload: "worker",
		Fingerprint: "0000000000000000000000000000000000000000000000000000000000000000",
		Decision:    policygen.DecisionEnable, Reason: "r", DecidedBy: "admin",
	}})
	if len(stale) != 1 {
		t.Fatalf("stale = %d entries, want 1", len(stale))
	}
	if stale[0].CurrentRules == nil {
		t.Error("CurrentRules is nil; it must be an empty slice so the UI renders 「该 workload 已不存在」 rather than a missing field")
	}
	if len(stale[0].CurrentRules) != 0 {
		t.Errorf("CurrentRules = %v, want empty for a vanished workload", stale[0].CurrentRules)
	}
}

// Baseline 规则不接受人工禁用：关掉 DNS 出向或健康检查入向的后果
// 分别是解析失败与入口中断，而这两类流量恰恰是学习环节发现不了、
// 只能靠 Baseline 补上的。
func TestApplyRejectsDisablingBaseline(t *testing.T) {
	base := policygen.Generate(observe(t, "prod-asia-1"))

	var bl policygen.Rule
	var ns, wl string
	for _, p := range base.Policies {
		for _, r := range p.Rules {
			if r.Origin == policygen.OriginBaseline && bl.Fingerprint == "" {
				bl, ns, wl = r, p.Namespace, p.Workload
			}
		}
	}
	if bl.Fingerprint == "" {
		t.Fatal("fixture produced no baseline rule")
	}

	out, stale := policygen.Apply(base, []policygen.Override{{
		Namespace: ns, Workload: wl, Fingerprint: bl.Fingerprint,
		Decision: policygen.DecisionDisable, Reason: "r", DecidedBy: "admin",
	}})
	if !ruleEnabled(out, ns, wl, bl.Fingerprint) {
		t.Error("a baseline rule was disabled by an override")
	}
	if len(stale) != 1 {
		t.Errorf("stale = %d, want 1 — a rejected override must be reported, not dropped", len(stale))
	}
}

// ruleEnabled 查一条规则当前是否启用。
func ruleEnabled(res policygen.Result, ns, wl, fp string) bool {
	for _, p := range res.Policies {
		if p.Namespace != ns || p.Workload != wl {
			continue
		}
		for _, r := range p.Rules {
			if r.Fingerprint == fp {
				return r.Enabled
			}
		}
	}
	return false
}

// deepCopyResult 对 Result 做一次深拷贝，供「未被修改」断言使用。
//
// 不走 JSON 往返：Rule.Ingress / Rule.Egress 标了 json:"-"，序列化会把
// 这两个字段丢光，拷出来的副本就天生跟原对象不相等 —— 那样这条断言
// 测的就不是「Apply 有没有改」，而是「json:"-" 的字段是否存在」。
func deepCopyResult(r policygen.Result) policygen.Result {
	out := policygen.Result{
		Policies:          make([]policygen.CandidatePolicy, len(r.Policies)),
		Ungeneratable:     cloneUngeneratable(r.Ungeneratable),
		ExcludedWorkloads: cloneExcludedWorkloads(r.ExcludedWorkloads),
	}
	if r.MissingBaselines != nil {
		out.MissingBaselines = make([]policygen.MissingBaseline, len(r.MissingBaselines))
		for i, mb := range r.MissingBaselines {
			out.MissingBaselines[i] = policygen.MissingBaseline{
				Namespace: mb.Namespace, Kinds: cloneKinds(mb.Kinds),
			}
		}
	}
	for i, p := range r.Policies {
		rules := make([]policygen.Rule, len(p.Rules))
		for j, rule := range p.Rules {
			rules[j] = deepCopyRule(rule)
		}
		out.Policies[i] = policygen.CandidatePolicy{
			Cluster: p.Cluster, Namespace: p.Namespace,
			Workload: p.Workload, WorkloadLabelKey: p.WorkloadLabelKey, Rules: rules,
		}
	}
	return out
}

// deepCopyRule 深拷贝单条规则，含 json:"-" 的 Ingress/Egress。
//
// nil 与「非 nil 但长度为 0」在这里必须分得清：LEARNED 规则的
// Derivations 天生是 nil，copy 时若一律换成空切片字面量，会让这份
// 拷贝和原件在 reflect.DeepEqual 下永远不相等——那样报的就不是
// 「Apply 改了」，而是这份深拷贝自己引入的假阳性。
func deepCopyRule(r policygen.Rule) policygen.Rule {
	out := r
	out.Derivations = cloneDerivations(r.Derivations)
	out.Peers = cloneStrings(r.Peers)
	out.Ports = cloneStrings(r.Ports)
	if r.Baseline != nil {
		b := *r.Baseline
		out.Baseline = &b
	}
	if r.Risk != nil {
		rp := *r.Risk
		out.Risk = &rp
	}
	if r.Ingress != nil {
		out.Ingress = r.Ingress.DeepCopy()
	}
	if r.Egress != nil {
		out.Egress = r.Egress.DeepCopy()
	}
	return out
}

// cloneStrings 拷贝一个字符串切片，nil 与「非 nil 但空」保持原样。
func cloneStrings(s []string) []string {
	if s == nil {
		return nil
	}
	out := make([]string, len(s))
	copy(out, s)
	return out
}

// cloneDerivations 拷贝一个推导依据切片，nil 与「非 nil 但空」保持原样。
func cloneDerivations(d []baseline.Derivation) []baseline.Derivation {
	if d == nil {
		return nil
	}
	out := make([]baseline.Derivation, len(d))
	copy(out, d)
	return out
}

// cloneKinds 拷贝一个 Baseline 类型切片，nil 与「非 nil 但空」保持原样。
func cloneKinds(k []baseline.Kind) []baseline.Kind {
	if k == nil {
		return nil
	}
	out := make([]baseline.Kind, len(k))
	copy(out, k)
	return out
}

// cloneUngeneratable 拷贝一个不可生成条目切片，nil 与「非 nil 但空」保持原样。
func cloneUngeneratable(items []policygen.UngeneratableItem) []policygen.UngeneratableItem {
	if items == nil {
		return nil
	}
	out := make([]policygen.UngeneratableItem, len(items))
	copy(out, items)
	return out
}

// cloneExcludedWorkloads 拷贝一个排除清单切片，nil 与「非 nil 但空」保持原样。
//
// 元素内的 Labels 是 map，copy(out, items) 只拷贝切片头，Labels 字段
// 本身仍与源共享同一份底层 map。这里够用：Apply 从不修改
// ExcludedWorkloads，TestApplyDoesNotMutateInput 只需要判定字段有没有
// 被整个换掉，不需要 Labels 的内容也独立一份。
func cloneExcludedWorkloads(items []policygen.ExcludedWorkload) []policygen.ExcludedWorkload {
	if items == nil {
		return nil
	}
	out := make([]policygen.ExcludedWorkload, len(items))
	copy(out, items)
	return out
}
