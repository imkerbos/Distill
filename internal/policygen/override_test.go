package policygen_test

import (
	"reflect"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/imkerbos/Distill/internal/baseline"
	"github.com/imkerbos/Distill/internal/policygen"
	"github.com/imkerbos/Distill/internal/replay"
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
//
// 改的是规则体，不是 Peers/Ports 展示串：指纹取自规则体（见
// FingerprintOf 的注释）。改展示串来断言指纹变化，测的就不是"内容变了"
// 而是渲染函数，而渲染是有损的 —— 两条选中范围不同的规则可以渲染成
// 同一个串（这正是 T6 的 I3）。
//
// 两半都不做条件跳过：fixture 拿不出一条同时带端口与 selector 对端的
// 出向规则，本身就是该报的失败，不是该静默略过的情况。
func TestFingerprintChangesWithContent(t *testing.T) {
	res := policygen.Generate(observe(t, "prod-asia-1"))

	var base policygen.Rule
search:
	for _, p := range res.Policies {
		for _, r := range p.Rules {
			if r.Egress != nil && len(r.Egress.Ports) > 0 && len(r.Egress.To) > 0 &&
				r.Egress.To[0].PodSelector != nil && len(r.Egress.To[0].PodSelector.MatchLabels) > 0 {
				base = r
				break search
			}
		}
	}
	if base.Fingerprint == "" {
		t.Fatal("fixture produced no egress rule carrying both a port and a selector peer")
	}

	portChanged := base
	portBody := base.Egress.DeepCopy()
	newPort := intstr.FromInt32(9999)
	portBody.Ports[0].Port = &newPort
	portChanged.Egress = portBody
	if policygen.FingerprintOf(portChanged) == base.Fingerprint {
		t.Error("fingerprint unchanged after a port change")
	}

	peerChanged := base
	peerBody := base.Egress.DeepCopy()
	for k := range peerBody.To[0].PodSelector.MatchLabels {
		peerBody.To[0].PodSelector.MatchLabels[k] = "somewhere-else"
	}
	peerChanged.Egress = peerBody
	if policygen.FingerprintOf(peerChanged) == base.Fingerprint {
		t.Error("fingerprint unchanged after a peer change")
	}

	keyChanged := base
	keyBody := base.Egress.DeepCopy()
	for k, v := range keyBody.To[0].PodSelector.MatchLabels {
		delete(keyBody.To[0].PodSelector.MatchLabels, k)
		keyBody.To[0].PodSelector.MatchLabels["k8s-app-renamed"] = v
	}
	keyChanged.Egress = keyBody
	if policygen.FingerprintOf(keyChanged) == base.Fingerprint {
		t.Error("fingerprint unchanged after the peer's label key changed with the value kept")
	}
}

// 两个只差标签键的对端在界面上渲染成同一个串（describeSelector 命中
// workloadLabelKeys 时只显示取值），但它们选中的是不同的 Pod。指纹
// 必须把它们分开：否则两条规则共用一个 rule_override 主键，一次人工
// 确认只对其中一条生效，而另一条在界面上长得一模一样、点了没反应。
func TestFingerprintDistinguishesPeersThatRenderIdentically(t *testing.T) {
	client := replay.PodRef{
		ClusterID: "c1", Namespace: "shop", Name: "client-1", IP: "10.0.0.1",
		Labels: map[string]string{"app": "client"},
	}
	apiApp := replay.PodRef{
		ClusterID: "c1", Namespace: "payment", Name: "api-app-1", IP: "10.0.1.1",
		Labels: map[string]string{"app": "api"},
	}
	apiK8s := replay.PodRef{
		ClusterID: "c1", Namespace: "payment", Name: "api-k8s-1", IP: "10.0.1.2",
		Labels: map[string]string{"k8s-app": "api"},
	}
	flowTo := func(dst replay.PodRef) replay.Flow {
		return replay.Flow{
			Source:   replay.Endpoint{IP: client.IP, ClusterID: client.ClusterID, Pod: &client},
			Dest:     replay.Endpoint{IP: dst.IP, ClusterID: dst.ClusterID, Pod: &dst},
			Protocol: replay.ProtocolTCP, Port: 3306,
			Timestamp: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		}
	}
	allow := replay.Decision{Verdict: replay.VerdictAllow, Confidence: replay.ConfidenceTrusted}
	res := policygen.Generate(policygen.Input{
		ClusterID: "c1",
		Pods:      []replay.PodRef{client, apiApp, apiK8s},
		Observations: []policygen.Observation{
			{FlowID: "f-app", Flow: flowTo(apiApp), Decision: allow, IdentityTrusted: true},
			{FlowID: "f-k8s", Flow: flowTo(apiK8s), Decision: allow, IdentityTrusted: true},
		},
	})

	var rules []policygen.Rule
	for _, p := range res.Policies {
		if p.Namespace != "shop" || p.Workload != "client" {
			continue
		}
		for _, r := range p.Rules {
			if r.Origin == policygen.OriginLearned && len(r.Peers) == 1 && r.Peers[0] == "payment/api" {
				rules = append(rules, r)
			}
		}
	}
	if len(rules) != 2 {
		t.Fatalf("egress rules rendering as payment/api = %d, want 2 (one per peer label key)", len(rules))
	}
	// 先证明碰撞的前提确实成立：两条规则的展示视图逐字段相同。少了这
	// 一句，下面那条断言在"渲染碰巧不同"时也会通过，就测不到它要测的东西。
	if rules[0].Peers[0] != rules[1].Peers[0] || rules[0].Ports[0] != rules[1].Ports[0] {
		t.Fatalf("the two rules no longer render identically (%v/%v vs %v/%v); "+
			"this test only means something while they do",
			rules[0].Peers, rules[0].Ports, rules[1].Peers, rules[1].Ports)
	}
	if rules[0].Fingerprint == rules[1].Fingerprint {
		t.Errorf("peers {app: api} and {k8s-app: api} share fingerprint %s; "+
			"they select different Pods and must be separate rule_override rows",
			rules[0].Fingerprint)
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

// 人工覆盖改的是"哪几条规则算启用"，改不了 Service selector 当初点没点名
// 单个 Pod 这件事实——Apply 必须原样带过 ExposureWidenings，否则覆盖之后
// 那一份预览会显示成这条暴露从未放宽过。
func TestApplyCarriesExposureWideningsThrough(t *testing.T) {
	base := generateWith(t, threeZookeeperPods(), zk0LBService())
	if len(base.ExposureWidenings) != 1 {
		t.Fatalf("生成阶段先要有一条放宽，报了 %d 条", len(base.ExposureWidenings))
	}

	out, _ := policygen.Apply(base, nil)
	if len(out.ExposureWidenings) != 1 {
		t.Fatalf("Apply 之后 ExposureWidenings 丢了：报了 %d 条，want 1", len(out.ExposureWidenings))
	}
	if out.ExposureWidenings[0] != base.ExposureWidenings[0] {
		t.Errorf("Apply 之后内容变了：%+v，want %+v",
			out.ExposureWidenings[0], base.ExposureWidenings[0])
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
func cloneMissing(in []policygen.MissingBaseline) []policygen.MissingBaseline {
	if in == nil {
		return nil
	}
	out := make([]policygen.MissingBaseline, len(in))
	for i, mb := range in {
		out[i] = policygen.MissingBaseline{Namespace: mb.Namespace, Kinds: cloneKinds(mb.Kinds)}
	}
	return out
}

func deepCopyResult(r policygen.Result) policygen.Result {
	out := policygen.Result{
		Policies:          make([]policygen.CandidatePolicy, len(r.Policies)),
		Ungeneratable:     cloneUngeneratable(r.Ungeneratable),
		ExcludedWorkloads: cloneExcludedWorkloads(r.ExcludedWorkloads),
		// nil 与"非 nil 但长度为 0"在 DeepEqual 下不相等，而 Generate 恒给
		// 出一个空切片（那是"算过、没有"）—— 照抄原件的形状，不改写。
		UnattachedImports:   append([]policygen.UnattachedImport{}, r.UnattachedImports...),
		UnattachedBaselines: append([]policygen.UnattachedBaselineRule{}, r.UnattachedBaselines...),
		ExcludedNamespaces:  append([]policygen.ExcludedNamespace{}, r.ExcludedNamespaces...),
		ExposureWidenings:   append([]policygen.ExposureWidening{}, r.ExposureWidenings...),
	}
	out.MissingBaselines = cloneMissing(r.MissingBaselines)
	out.NotApplicableBaselines = cloneMissing(r.NotApplicableBaselines)
	for i, p := range r.Policies {
		rules := make([]policygen.Rule, len(p.Rules))
		for j, rule := range p.Rules {
			rules[j] = deepCopyRule(rule)
		}
		out.Policies[i] = policygen.CandidatePolicy{
			Cluster: p.Cluster, Namespace: p.Namespace, Granularity: p.Granularity,
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
