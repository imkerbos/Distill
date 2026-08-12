package policygen_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

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
	Peers       []string
	Ports       []string
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
			Peers: r.Peers, Ports: r.Ports,
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
		// 不假定固定的 app 键：真实集群里 workload 归属键因 Pod 而异
		// （coredns 用 k8s-app 等），生成的 podSelector 恒为单键
		// matchLabels，直接取这唯一的值。
		var workload string
		for _, v := range p.Spec.PodSelector.MatchLabels {
			workload = v
		}
		if workload == "" {
			t.Errorf("%s/%s: podSelector has no workload label", p.Namespace, p.Name)
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

// 每条规则都必须带对端与端口的展示视图。
//
// 原始 networkingv1 字段带 json:"-" 不出 API，界面只能看到这两个字段。
// 它们缺失时，页面上的一条规则就只剩「LEARNED · EGRESS」——读的人分不出
// payment:8080 与 0.0.0.0/0:443，而这是两件性质完全不同的事。
func TestEveryRuleCarriesPeersAndPorts(t *testing.T) {
	res := policygen.Generate(observe(t, "prod-asia-1"))
	if len(res.Policies) == 0 {
		t.Fatal("no candidate policies generated")
	}
	for _, p := range res.Policies {
		for _, r := range p.Rules {
			if len(r.Peers) == 0 {
				t.Errorf("%s/%s: %s rule has no rendered peer", p.Namespace, p.Workload, r.Direction)
			}
			if len(r.Ports) == 0 {
				t.Errorf("%s/%s: %s rule has no rendered port", p.Namespace, p.Workload, r.Direction)
			}
			for _, port := range r.Ports {
				if !strings.Contains(port, "/") {
					t.Errorf("%s/%s: port %q is not rendered as PROTOCOL/PORT",
						p.Namespace, p.Workload, port)
				}
			}
		}
	}
}

// 真实集群不会都用 app 标签：coredns / kube-proxy 用 k8s-app。一条从
// k8s-app 归属出来的候选策略，podSelector 必须真的用 k8s-app 构造，
// 否则生成的是一条集群里没有任何 Pod 会命中的幽灵 selector
// （{app: kube-dns} 而不是 {k8s-app: kube-dns}）。
func TestK8sAppLabelledPodProducesMatchingPodSelector(t *testing.T) {
	coredns := replay.PodRef{
		ClusterID: "c1", Namespace: "kube-system", Name: "coredns-1", IP: "10.0.0.2",
		Labels: map[string]string{"k8s-app": "kube-dns"},
	}
	client := replay.PodRef{
		ClusterID: "c1", Namespace: "default", Name: "client-1", IP: "10.0.0.1",
		Labels: map[string]string{"app": "client"},
	}
	flow := replay.Flow{
		Source:    replay.Endpoint{IP: client.IP, ClusterID: client.ClusterID, Pod: &client},
		Dest:      replay.Endpoint{IP: coredns.IP, ClusterID: coredns.ClusterID, Pod: &coredns},
		Protocol:  replay.ProtocolUDP,
		Port:      53,
		Timestamp: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
	}
	res := policygen.Generate(policygen.Input{
		ClusterID: "c1",
		Pods:      []replay.PodRef{coredns, client},
		Observations: []policygen.Observation{{
			FlowID: "f1", Flow: flow,
			Decision: replay.Decision{Verdict: replay.VerdictAllow, Confidence: replay.ConfidenceTrusted},
		}},
	})

	var found *policygen.CandidatePolicy
	for i := range res.Policies {
		if res.Policies[i].Namespace == "kube-system" && res.Policies[i].Workload == "kube-dns" {
			found = &res.Policies[i]
		}
	}
	if found == nil {
		t.Fatal("no candidate policy generated for the k8s-app-labelled coredns Pod")
	}
	if found.WorkloadLabelKey != "k8s-app" {
		t.Errorf("WorkloadLabelKey = %q, want k8s-app", found.WorkloadLabelKey)
	}

	var np *networkingv1.NetworkPolicy
	for _, p := range res.EnabledPolicies() {
		if p.Namespace == "kube-system" && p.Spec.PodSelector.MatchLabels["k8s-app"] == "kube-dns" {
			pCopy := p
			np = &pCopy
		}
	}
	if np == nil {
		t.Fatal("EnabledPolicies() has no policy with podSelector {k8s-app: kube-dns}")
	}
	if _, wrongKey := np.Spec.PodSelector.MatchLabels["app"]; wrongKey {
		t.Errorf("podSelector = %v, must not use the app key for a k8s-app-labelled workload",
			np.Spec.PodSelector.MatchLabels)
	}
}

// hostNetwork 与无 workload 标签的 Pod 从未进入候选策略花名册，因此
// 永远不会作为主体出现在任何一条流量判定里——Ungeneratable 报不出这个
// 缺口。ExcludedWorkloads 必须点名这两类 Pod，各自带上正确的原因。
func TestFixtureExcludedWorkloadsNameHostNetworkAndUnlabelledPods(t *testing.T) {
	res := policygen.Generate(observe(t, "prod-asia-1"))

	var gotHostNetwork, gotNoLabel *policygen.ExcludedWorkload
	for i := range res.ExcludedWorkloads {
		w := &res.ExcludedWorkloads[i]
		if w.Namespace == "kube-system" && w.Pod == "kube-proxy-1" {
			gotHostNetwork = w
		}
		if w.Namespace == "legacy" && w.Pod == "legacy-unlabelled" {
			gotNoLabel = w
		}
	}
	if gotHostNetwork == nil {
		t.Fatal("kube-system/kube-proxy-1 not present in ExcludedWorkloads")
	}
	if gotHostNetwork.Reason != policygen.ExclusionHostNetwork {
		t.Errorf("kube-proxy-1 reason = %q, want %q", gotHostNetwork.Reason, policygen.ExclusionHostNetwork)
	}
	if gotNoLabel == nil {
		t.Fatal("legacy/legacy-unlabelled not present in ExcludedWorkloads")
	}
	if gotNoLabel.Reason != policygen.ExclusionNoWorkloadLabel {
		t.Errorf("legacy-unlabelled reason = %q, want %q", gotNoLabel.Reason, policygen.ExclusionNoWorkloadLabel)
	}
}

// Helm chart 常见同时打 app.kubernetes.io/name 与 app 两个标签，且取值
// 不同（迁移期、chart 内部命名与对外服务名不一致等）。workloadLabelKeys
// 顺序即优先级：app.kubernetes.io/name 必须赢，而且赢的是这个键本身，
// 不是"随便挑一个非空的"——podSelector 的键和值都要来自赢家，输家的取值
// 一个字符都不能泄进去。
func TestPodWithMultipleResolvableLabelsUsesHighestPriorityKey(t *testing.T) {
	web := replay.PodRef{
		ClusterID: "c1", Namespace: "shop", Name: "web-1", IP: "10.0.0.2",
		Labels: map[string]string{
			"app.kubernetes.io/name": "web-canonical",
			"app":                    "web-legacy",
		},
	}
	client := replay.PodRef{
		ClusterID: "c1", Namespace: "shop", Name: "client-1", IP: "10.0.0.1",
		Labels: map[string]string{"app": "client"},
	}
	flow := replay.Flow{
		Source:    replay.Endpoint{IP: client.IP, ClusterID: client.ClusterID, Pod: &client},
		Dest:      replay.Endpoint{IP: web.IP, ClusterID: web.ClusterID, Pod: &web},
		Protocol:  replay.ProtocolTCP,
		Port:      8080,
		Timestamp: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
	}
	res := policygen.Generate(policygen.Input{
		ClusterID: "c1",
		Pods:      []replay.PodRef{web, client},
		Observations: []policygen.Observation{{
			FlowID: "f1", Flow: flow,
			Decision: replay.Decision{Verdict: replay.VerdictAllow, Confidence: replay.ConfidenceTrusted},
		}},
	})

	var found *policygen.CandidatePolicy
	for i := range res.Policies {
		if res.Policies[i].Namespace == "shop" && res.Policies[i].Cluster == "c1" &&
			res.Policies[i].WorkloadLabelKey == "app.kubernetes.io/name" {
			found = &res.Policies[i]
		}
	}
	if found == nil {
		t.Fatal("no candidate policy resolved via app.kubernetes.io/name")
	}
	if found.Workload != "web-canonical" {
		t.Errorf("Workload = %q, want web-canonical (the app.kubernetes.io/name value, not the app value)",
			found.Workload)
	}

	var np *networkingv1.NetworkPolicy
	for _, p := range res.EnabledPolicies() {
		if p.Namespace == "shop" && p.Spec.PodSelector.MatchLabels["app.kubernetes.io/name"] == "web-canonical" {
			pCopy := p
			np = &pCopy
		}
	}
	if np == nil {
		t.Fatal("EnabledPolicies() has no policy with podSelector {app.kubernetes.io/name: web-canonical}")
	}
	if v, wrongKey := np.Spec.PodSelector.MatchLabels["app"]; wrongKey {
		t.Errorf("podSelector = %v, must not carry the losing app=%q label at all",
			np.Spec.PodSelector.MatchLabels, v)
	}
}

// hostNetwork 判断先于标签判断（peerOf 的既有注释已经这么写），理由是
// hostNetwork 是更根本的事实——policy 根本够不着这个 Pod，而缺标签是
// 运维能修的。但这个优先级只活在代码里：谁都能在下次改动时不小心把
// 判断顺序换过来，换完之后所有测试大概率照样绿——因为大多数 hostNetwork
// Pod 恰好也没有 workload 标签，两个分支报的都是"排除"，只是原因不同，
// 谁都不会注意到原因错了。这条用例专门构造两者都成立的 Pod，把顺序
// 钉死成一个可验证的事实：必须恰好一条排除记录，原因是 hostNetwork，
// 不是 NO_WORKLOAD_LABEL。
func TestHostNetworkTakesPrecedenceOverMissingLabel(t *testing.T) {
	privileged := replay.PodRef{
		ClusterID: "c1", Namespace: "kube-system", Name: "cni-agent-1", IP: "10.0.0.9",
		HostNetwork: true, Labels: map[string]string{"env": "prod"}, // 无任何可识别 workload 标签
	}
	res := policygen.Generate(policygen.Input{
		ClusterID: "c1",
		Pods:      []replay.PodRef{privileged},
	})

	var matches []policygen.ExcludedWorkload
	for _, w := range res.ExcludedWorkloads {
		if w.Namespace == "kube-system" && w.Pod == "cni-agent-1" {
			matches = append(matches, w)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("ExcludedWorkloads entries for cni-agent-1 = %d, want exactly 1: %+v", len(matches), matches)
	}
	if matches[0].Reason != policygen.ExclusionHostNetwork {
		t.Errorf("reason = %q, want %q (hostNetwork must win over the missing-label case)",
			matches[0].Reason, policygen.ExclusionHostNetwork)
	}
}

// 手工构造的 Pod 证明了机制成立，但没有证明真实数据集受影响。这里用
// prod-asia-1 的真实快照钉住那个曾经手工用 curl 验证过的事实：
// kube-system/kube-dns（k8s-app: kube-dns 的 Pod）现在真的进了候选集，
// 且用的是 k8s-app 键。少了这条，"kube-dns 现在有候选策略了"这句话
// 只在人手工跑一次 docker compose 时成立，下一次改动就可能悄悄回退
// 而没有任何测试报错。
func TestFixtureKubeDNSGetsCandidateWithK8sAppKey(t *testing.T) {
	res := policygen.Generate(observe(t, "prod-asia-1"))

	var found *policygen.CandidatePolicy
	for i := range res.Policies {
		if res.Policies[i].Namespace == "kube-system" && res.Policies[i].Workload == "kube-dns" {
			found = &res.Policies[i]
		}
	}
	if found == nil {
		t.Fatal("prod-asia-1: no candidate policy for kube-system/kube-dns; " +
			"the k8s-app-labelled kube-dns Pods never entered the roster")
	}
	if found.WorkloadLabelKey != "k8s-app" {
		t.Errorf("kube-system/kube-dns WorkloadLabelKey = %q, want k8s-app", found.WorkloadLabelKey)
	}
}

// 展示视图必须与它渲染的规则体一致：selector 对端渲染成 namespace/workload，
// ipBlock 对端渲染成 CIDR 原文。渲染错了比不渲染更糟——它看起来是事实。
func TestPeersAndPortsMatchTheRuleBody(t *testing.T) {
	res := policygen.Generate(observe(t, "prod-asia-1"))
	checked := map[string]bool{}
	for _, p := range res.Policies {
		for _, r := range p.Rules {
			var peers []networkingv1.NetworkPolicyPeer
			switch {
			case r.Ingress != nil:
				peers = r.Ingress.From
			case r.Egress != nil:
				peers = r.Egress.To
			}
			if len(peers) != len(r.Peers) {
				t.Fatalf("%s/%s: %d rendered peers for %d actual peers",
					p.Namespace, p.Workload, len(r.Peers), len(peers))
			}
			for i, peer := range peers {
				switch {
				case peer.IPBlock != nil:
					checked["cidr"] = true
					if !strings.HasPrefix(r.Peers[i], peer.IPBlock.CIDR) {
						t.Errorf("%s/%s: peer %q does not carry CIDR %q",
							p.Namespace, p.Workload, r.Peers[i], peer.IPBlock.CIDR)
					}
				case peer.PodSelector != nil:
					checked["selector"] = true
					if !strings.Contains(r.Peers[i], "/") {
						t.Errorf("%s/%s: selector peer %q is not rendered as namespace/workload",
							p.Namespace, p.Workload, r.Peers[i])
					}
				}
			}
		}
	}
	// 两种对端形态都要真的走到，否则这条断言只覆盖了碰巧存在的那一种。
	for _, form := range []string{"cidr", "selector"} {
		if !checked[form] {
			t.Errorf("no %s peer exercised; the fixture no longer covers this shape", form)
		}
	}
}
