package policygen_test

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	networkingv1 "k8s.io/api/networking/v1"

	"github.com/imkerbos/Distill/internal/baseline"
	"github.com/imkerbos/Distill/internal/fixture"
	"github.com/imkerbos/Distill/internal/policygen"
	"github.com/imkerbos/Distill/internal/replay"
	"github.com/imkerbos/Distill/internal/risk"
	"github.com/imkerbos/Distill/internal/snapshot"
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
		replay.WithForeignPlane(c.CCNPPresent))
	var obs []policygen.Observation
	for _, fl := range f.Flows {
		d := ev.Evaluate(fl.Flow)
		obs = append(obs, policygen.Observation{
			FlowID: fl.ID, Flow: fl.Flow, Decision: d,
			// 这条路径上没有窗口完整度可传导，因此求值引擎自己的可信度
			// 就是身份可信度：mesh / CCNP 降级的那些仍然一条都学不到。
			IdentityTrusted: d.Confidence == replay.ConfidenceTrusted,
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
	// WorkloadLabelKey 一并比对：它直接决定生成的 podSelector 的键，
	// 是"会变的字段"里最要紧的一个——归属键换了，策略选中的对象就换了，
	// 而 Namespace/Workload/Rules 全都可以一字不差。
	b, err := json.Marshal(struct {
		Cluster, Namespace, Workload, WorkloadLabelKey string
		Rules                                          []ruleContent
	}{p.Cluster, p.Namespace, p.Workload, p.WorkloadLabelKey, rules})
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

// 学习来的风险规则必须生成、必须可见、必须默认不启用——平台猜出来的放行
// 命中风险清单时，"这条值得看一眼"翻译成"先别自动生效"。
//
// **只看 OriginLearned。** 这条不变量是为学习来的规则写的，I9 之后
// Baseline 侧会故意反过来：风险且启用（design review NI3，2026-08-28）。
// 早先这里遍历全部 Origin，只是因为 prod-asia-1 这份 fixture 恰好没有
// 任何一个 EXPOSED_INGRESS Service 暴露风险端口——加一个 6379/3306 就会
// 让它变红，是一条巧合绿而非真的守住了这条不变量。
func TestRiskyRulesAreGeneratedButDisabled(t *testing.T) {
	res := policygen.Generate(observe(t, "prod-asia-1"))
	risky, riskyEnabled := 0, 0
	for _, p := range res.Policies {
		for _, r := range p.Rules {
			if r.Origin != policygen.OriginLearned || r.Risk == nil {
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

// Baseline 侧反过来：风险且启用，标注仍然要在。spec §3.6 是权威——一条
// Baseline 暴露描述的是集群已经在公开的东西，禁用它等于切断现有流量；
// 标注的作用只是让它在界面上跟一条普通放行区分开，不是把它挡下来。
//
// 不用 prod-asia-1 fixture：它没有暴露风险端口的 Service，一个断言"找到
// 就检查、没找到就跳过"的用例在这份 fixture 上恒为跳过，等于没测——
// 用手工构造的最小场景直接钉住这个形态。
func TestBaselineRiskyRulesAreAnnotatedAndStayEnabled(t *testing.T) {
	pods := []replay.PodRef{
		{ClusterID: "c1", Namespace: "istio-system", Name: "inner-1", IP: "10.4.0.5",
			Labels: map[string]string{"app": "istio-ingressgateway-inner"}},
	}
	assets := snapshot.Assets{
		ClusterID: "c1",
		Services: []snapshot.Service{{
			ClusterID: "c1", Namespace: "istio-system", Name: "istio-ingressgateway-inner",
			Type:     "LoadBalancer",
			Selector: map[string]string{"app": "istio-ingressgateway-inner"},
			Ports: []snapshot.ServicePort{
				{Name: "redis", Port: 6379, TargetPort: 6379, Protocol: "TCP"},
			},
			LoadBalancerIngressIPs: []string{"10.170.48.55"},
		}},
		Registry: snapshot.ClusterRegistry{ClusterID: "c1", NodeCIDR: "10.170.48.0/24"},
	}
	res := policygen.Generate(policygen.Input{ClusterID: "c1", Pods: pods, Assets: assets})

	var found bool
	for _, p := range res.Policies {
		for _, r := range p.Rules {
			if r.Origin != policygen.OriginBaseline || r.Risk == nil {
				continue
			}
			found = true
			if !r.Enabled {
				t.Errorf("%s/%s: baseline 风险规则被禁用了，它描述的是现状，"+
					"禁用会切断真实流量", p.Namespace, p.Workload)
			}
		}
	}
	if !found {
		t.Fatal("没有生成任何带 Risk 标注的 baseline 规则，这条用例没跑到要测的形态")
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

// fixtureUnreachableReasons 是 fixture 流量构造不出、由专门用例覆盖的原因。
//
// 列出来而不是从下面的断言里删掉：新增一个原因时，作者必须显式回答
// "它由谁覆盖"，否则那条断言会失败。值是覆盖它的用例名，好让读的人
// 直接去看，而不是相信这句话。
var fixtureUnreachableReasons = map[policygen.UngeneratableReason]string{
	// LABEL_KEY_CONFLICT 要求同一个 namespace 里两个 Pod 带同一个 workload
	// 取值却挂在不同标签键上，且输家有流量。往 fixture 加这样一对 Pod 会
	// 改动 spec §6 钉死的验收数字（81/0/123/44），因此单独构造。
	policygen.ReasonLabelKeyConflict: "TestSameWorkloadUnderTwoLabelKeysYieldsOneCandidatePolicy",
}

// 每一类不可生成原因都必须被验证过：fixture 上真实出现过，或由
// fixtureUnreachableReasons 点名的专门用例覆盖。两者都不满足，
// 那个分支就是从未被执行过的代码。
func TestUngeneratableCoversEveryReason(t *testing.T) {
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
		covered, listed := fixtureUnreachableReasons[want]
		switch {
		case seen[want] && listed:
			t.Errorf("reason %q now occurs in the fixture; drop it from fixtureUnreachableReasons"+
				" so %s is not the only thing claiming to cover it", want, covered)
		case !seen[want] && !listed:
			t.Errorf("reason %q never occurs in the fixture and no test claims it;"+
				" that branch is unverified", want)
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

	// 一个主体现在是两个对象（design doc 2026-08-24 §3.6）。先按主体把两半
	// 合回去再比：这条用例守的是「禁用规则不会漏进生效策略集」，那条性质与
	// 规则装在一个对象还是两个无关，而逐个对象去比只会把它变成一条在比
	// 形状的用例。
	merged := map[[2]string]networkingv1.NetworkPolicy{}
	for _, p := range policies {
		if len(p.Spec.PolicyTypes) != 1 {
			t.Errorf("%s/%s: policyTypes = %v，拆分后一个对象只该管一个方向",
				p.Namespace, p.Name, p.Spec.PolicyTypes)
			continue
		}
		key := [2]string{p.Namespace, strings.TrimSuffix(
			strings.TrimSuffix(p.Name, policygen.IngressSuffix), policygen.EgressSuffix)}
		m, ok := merged[key]
		if !ok {
			m = networkingv1.NetworkPolicy{
				ObjectMeta: p.ObjectMeta,
				Spec:       networkingv1.NetworkPolicySpec{PodSelector: p.Spec.PodSelector},
			}
		}
		m.Spec.Ingress = append(m.Spec.Ingress, p.Spec.Ingress...)
		m.Spec.Egress = append(m.Spec.Egress, p.Spec.Egress...)
		merged[key] = m
	}

	sawDisabledRule := false
	for _, p := range merged {
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
			Decision:        replay.Decision{Verdict: replay.VerdictAllow, Confidence: replay.ConfidenceTrusted},
			IdentityTrusted: true,
		}},
		// 显式纳入 kube-system：这条用例守的是"k8s-app 这个标签键要产出
		// 正确的 podSelector"，而 coredns 只是它最典型的例子。系统命名空间
		// 默认整片不生成（systemNamespaces），不纳入的话这里没有候选可查 ——
		// 那道保护与这条纪律无关，不该让它把用例的前提抽掉。
		ManagedSystemNamespaces: []string{"kube-system"},
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

	var gotNoLabel *policygen.ExcludedWorkload
	for i := range res.ExcludedWorkloads {
		w := &res.ExcludedWorkloads[i]
		if w.Namespace == "legacy" && w.Pod == "legacy-unlabelled" {
			gotNoLabel = w
		}
	}

	// **hostNetwork 那一半单独构造，不再依赖 fixture。**
	//
	// fixture 里唯一的 hostNetwork Pod 在 kube-system，而系统命名空间现在
	// 整片不生成候选策略（systemNamespaces），那个 Pod 因此走不到 hostNetwork
	// 这一支。它的排除原因变成了"整片不管"，由 ExcludedNamespaces 报 ——
	// 两件事分开说，逐个重复报会让"这个 Pod 有标签问题"淹没在"这一片不管"里。
	//
	// 那条纪律本身没变，因此用一个最小构造继续守住它：一个业务命名空间里的
	// hostNetwork Pod 必须被点名，否则它会以"0 不可生成"的面貌被悄悄吞掉。
	host := policygen.Generate(policygen.Input{
		ClusterID: "c1",
		Pods: []replay.PodRef{{
			ClusterID: "c1", Namespace: "payment", Name: "node-agent-1",
			Labels: map[string]string{"app": "node-agent"}, HostNetwork: true,
		}},
	})
	var gotHostNetwork *policygen.ExcludedWorkload
	for i := range host.ExcludedWorkloads {
		if host.ExcludedWorkloads[i].Pod == "node-agent-1" {
			gotHostNetwork = &host.ExcludedWorkloads[i]
		}
	}
	if gotHostNetwork == nil {
		t.Fatal("业务命名空间里的 hostNetwork Pod 没有出现在 ExcludedWorkloads")
	}
	if gotHostNetwork.Reason != policygen.ExclusionHostNetwork {
		t.Errorf("node-agent-1 reason = %q, want %q",
			gotHostNetwork.Reason, policygen.ExclusionHostNetwork)
	}

	// 新行为：kube-system 整片进排除清单，不再逐个 Pod 报。
	var sawSystemNS bool
	for _, ns := range res.ExcludedNamespaces {
		if ns.Namespace == "kube-system" {
			sawSystemNS = true
		}
	}
	if !sawSystemNS {
		t.Error("kube-system 没有出现在 ExcludedNamespaces —— " +
			"一个悄悄不见的命名空间与「它没有 workload」长得一样")
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
			Decision:        replay.Decision{Verdict: replay.VerdictAllow, Confidence: replay.ConfidenceTrusted},
			IdentityTrusted: true,
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

// 同一个 namespace 里两个 Pod 带同一个 workload 取值、却挂在不同的标签
// 键上——Helm chart 改标签的滚动更新期间就是这个形态。(namespace, workload)
// 是覆盖机制的定位键：Apply 按它找策略、EnsureRuleExists 按它校验、
// EnabledPolicies 按它命名、排序比较器按它定序。它一旦不唯一，这四处
// 会各自给出不同的答案，而其中三处不报错。
//
// 因此这里钉死：赢家只有一个，输家进 ExcludedWorkloads 并点名，输家的
// 流量进 Ungeneratable 并点名。输家确实没有候选策略——赢家的 podSelector
// 用赢家的键构造，选不中它——报出这个缺口，好过发一条同名、却谁都选不中
// 的第二份策略。
func TestSameWorkloadUnderTwoLabelKeysYieldsOneCandidatePolicy(t *testing.T) {
	canonical := replay.PodRef{
		ClusterID: "c1", Namespace: "shop", Name: "web-new-1", IP: "10.0.0.2",
		Labels: map[string]string{"app.kubernetes.io/name": "web"},
	}
	legacy := replay.PodRef{
		ClusterID: "c1", Namespace: "shop", Name: "web-legacy-1", IP: "10.0.0.3",
		Labels: map[string]string{"app": "web"},
	}
	client := replay.PodRef{
		ClusterID: "c1", Namespace: "shop", Name: "client-1", IP: "10.0.0.1",
		Labels: map[string]string{"app": "client"},
	}
	flowTo := func(dst replay.PodRef) replay.Flow {
		return replay.Flow{
			Source:   replay.Endpoint{IP: client.IP, ClusterID: client.ClusterID, Pod: &client},
			Dest:     replay.Endpoint{IP: dst.IP, ClusterID: dst.ClusterID, Pod: &dst},
			Protocol: replay.ProtocolTCP, Port: 8080,
			Timestamp: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		}
	}
	allow := replay.Decision{Verdict: replay.VerdictAllow, Confidence: replay.ConfidenceTrusted}
	res := policygen.Generate(policygen.Input{
		ClusterID: "c1",
		Pods:      []replay.PodRef{canonical, legacy, client},
		Observations: []policygen.Observation{
			{FlowID: "f-new", Flow: flowTo(canonical), Decision: allow, IdentityTrusted: true},
			{FlowID: "f-legacy", Flow: flowTo(legacy), Decision: allow, IdentityTrusted: true},
		},
	})

	var forWeb []policygen.CandidatePolicy
	for _, p := range res.Policies {
		if p.Namespace == "shop" && p.Workload == "web" {
			forWeb = append(forWeb, p)
		}
	}
	if len(forWeb) != 1 {
		t.Fatalf("candidate policies for shop/web = %d, want exactly 1: %+v", len(forWeb), forWeb)
	}
	if forWeb[0].WorkloadLabelKey != "app.kubernetes.io/name" {
		t.Errorf("WorkloadLabelKey = %q, want app.kubernetes.io/name (the higher-priority key)",
			forWeb[0].WorkloadLabelKey)
	}

	// 同名 NetworkPolicy 对 predict.Run 是加法、无害，但轮 3 要把它写回
	// Git：两个同名对象在一个 namespace 里，后写的那个会覆盖前一个。
	seen := map[string]bool{}
	for _, np := range res.EnabledPolicies() {
		id := np.Namespace + "/" + np.Name
		if seen[id] {
			t.Errorf("EnabledPolicies() emits two NetworkPolicies named %s", id)
		}
		seen[id] = true
	}

	var excluded []policygen.ExcludedWorkload
	for _, w := range res.ExcludedWorkloads {
		if w.Pod == "web-legacy-1" {
			excluded = append(excluded, w)
		}
	}
	if len(excluded) != 1 {
		t.Fatalf("ExcludedWorkloads entries for web-legacy-1 = %d, want exactly 1: %+v",
			len(excluded), excluded)
	}
	if excluded[0].Reason != policygen.ExclusionLabelKeyConflict {
		t.Errorf("reason = %q, want %q", excluded[0].Reason, policygen.ExclusionLabelKeyConflict)
	}
	if excluded[0].Labels["app"] != "web" {
		t.Errorf("Labels = %v, want the losing Pod's own labels for triage", excluded[0].Labels)
	}

	// 输家的那条流量必须留下痕迹：它的目的侧不再产出任何规则，只报
	// "候选策略 N 条" 会让这条连接凭空消失，而人正是照着这份清单判断
	// 上线会不会断。
	var gaps []policygen.UngeneratableItem
	for _, it := range res.Ungeneratable {
		if it.Reason == policygen.ReasonLabelKeyConflict {
			gaps = append(gaps, it)
		}
	}
	if len(gaps) != 1 {
		t.Fatalf("LABEL_KEY_CONFLICT gaps = %d, want exactly 1 (the f-legacy ingress side): %+v",
			len(gaps), gaps)
	}
	if gaps[0].FlowID != "f-legacy" {
		t.Errorf("gap flowID = %q, want f-legacy", gaps[0].FlowID)
	}
	if !strings.Contains(gaps[0].Detail, "web-legacy-1") {
		t.Errorf("detail = %q, must name the Pod; a count alone says nothing about what to fix",
			gaps[0].Detail)
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
	// 刻意放在业务命名空间：系统命名空间整片不生成候选策略
	// （systemNamespaces），那时这个 Pod 走不到排除判定这一支，
	// 而这条用例要钉的是**两个排除原因谁优先**，与那道保护无关。
	privileged := replay.PodRef{
		ClusterID: "c1", Namespace: "payment", Name: "cni-agent-1", IP: "10.0.0.9",
		HostNetwork: true, Labels: map[string]string{"env": "prod"}, // 无任何可识别 workload 标签
	}
	res := policygen.Generate(policygen.Input{
		ClusterID: "c1",
		Pods:      []replay.PodRef{privileged},
	})

	var matches []policygen.ExcludedWorkload
	for _, w := range res.ExcludedWorkloads {
		if w.Namespace == "payment" && w.Pod == "cni-agent-1" {
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
	// **显式纳入 kube-system**：系统命名空间默认整片不生成候选策略，
	// 而这条用例守的是"k8s-app 这个标签键要被认出来"——那条纪律与那道
	// 保护无关，kube-dns 只是它最典型的例子。纳入之后照常生成，正好也
	// 验证了那条出口是通的。
	in := observe(t, "prod-asia-1")
	in.ManagedSystemNamespaces = []string{"kube-system"}
	res := policygen.Generate(in)

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

// 每份候选策略拆成 ingress 与 egress 两个对象（design doc 2026-08-24 §3.6）。
//
// 这两个方向的风险性质不同：egress 收错的症状是隐蔽的超时，ingress 收错是
// 立刻连不上。拆开之后评审人不打开文件就知道这一条改的是谁的哪个方向。
func TestEnabledPoliciesSplitByDirection(t *testing.T) {
	policies := policygen.Generate(observe(t, "prod-asia-1")).EnabledPolicies()
	if len(policies) == 0 {
		t.Fatal("EnabledPolicies() empty")
	}

	subjects := map[[2]string][]string{}
	for _, p := range policies {
		if len(p.Spec.PolicyTypes) != 1 {
			t.Errorf("%s/%s: policyTypes = %v，一个对象只该管一个方向",
				p.Namespace, p.Name, p.Spec.PolicyTypes)
			continue
		}
		switch p.Spec.PolicyTypes[0] {
		case networkingv1.PolicyTypeIngress:
			if !strings.HasSuffix(p.Name, "-ingress") {
				t.Errorf("%s/%s: 入站对象的名字没有 -ingress 后缀", p.Namespace, p.Name)
			}
			if len(p.Spec.Egress) != 0 {
				t.Errorf("%s/%s: 入站对象里带着出站规则", p.Namespace, p.Name)
			}
		case networkingv1.PolicyTypeEgress:
			if !strings.HasSuffix(p.Name, "-egress") {
				t.Errorf("%s/%s: 出站对象的名字没有 -egress 后缀", p.Namespace, p.Name)
			}
			if len(p.Spec.Ingress) != 0 {
				t.Errorf("%s/%s: 出站对象里带着入站规则", p.Namespace, p.Name)
			}
		}
		key := [2]string{p.Namespace, strings.TrimSuffix(
			strings.TrimSuffix(p.Name, "-ingress"), "-egress")}
		subjects[key] = append(subjects[key], string(p.Spec.PolicyTypes[0]))
	}

	// **两半永远成对出现，包括空的那一半。**
	//
	// 一个 policyTypes:[Ingress] 且没有 ingress 规则的对象，含义是「拒绝全部
	// 入站」，不是「无操作」。少生成它，这个 workload 的入站就从默认拒绝变成
	// 全部放行 —— 方向朝不安全，且不报错（design doc §3.6）。
	for subject, dirs := range subjects {
		sort.Strings(dirs)
		if !reflect.DeepEqual(dirs, []string{"Egress", "Ingress"}) {
			t.Errorf("%s/%s 只生成了 %v —— 缺的那一半意味着那个方向从默认拒绝变成全部放行",
				subject[0], subject[1], dirs)
		}
	}
}

// 拆分后的两个对象合起来，与拆分前那一个逐条等价。
//
// 等价性是这次改动的全部前提：拆的是表达形式，不是策略本身。任何一条规则
// 在拆分中丢失或换了方向，都是一次静默的语义变更。
func TestSplitPoliciesCarryEveryRule(t *testing.T) {
	res := policygen.Generate(observe(t, "prod-asia-1"))
	policies := res.EnabledPolicies()

	gotIngress, gotEgress := 0, 0
	for _, p := range policies {
		gotIngress += len(p.Spec.Ingress)
		gotEgress += len(p.Spec.Egress)
	}

	wantIngress, wantEgress := 0, 0
	for _, c := range res.Policies {
		for _, r := range c.Rules {
			if !r.Enabled {
				continue
			}
			if r.Ingress != nil {
				wantIngress++
			}
			if r.Egress != nil {
				wantEgress++
			}
		}
	}
	if gotIngress != wantIngress || gotEgress != wantEgress {
		t.Errorf("拆分后规则条数 ingress=%d egress=%d，拆分前 ingress=%d egress=%d",
			gotIngress, gotEgress, wantIngress, wantEgress)
	}
}

// --- Fix round 1 (design review 2026-08-28) ---

// C1: EXPOSED_INGRESS 只挂给 Service selector 实际选中的那个 workload，
// 不广播给整个 namespace。之前的实现把 Baseline 无条件追加给 namespace
// 里的每个 workload——那条规则对既有五类（DNS、control plane 等）是对的，
// 因为它们本来就是 namespace 级的基础设施事实，但对 EXPOSED_INGRESS 是错的：
// shop/worker 没有任何暴露对象，却因为同 namespace 里的 shop/edge 有一个
// LoadBalancer Service，也拿到了一条 EXPOSED_INGRESS peers=[0.0.0.0/0]
// enabled=true（design review C1，2026-08-28，复现于真实生成结果）。
func TestExposedIngressBaselineOnlyAttachesToTheExposedWorkload(t *testing.T) {
	pods := []replay.PodRef{
		{ClusterID: "c1", Namespace: "shop", Name: "worker-1", IP: "10.4.0.1",
			Labels: map[string]string{"app": "worker"}},
		{ClusterID: "c1", Namespace: "shop", Name: "edge-1", IP: "10.4.0.2",
			Labels: map[string]string{"app": "edge"}},
	}
	assets := snapshot.Assets{
		ClusterID: "c1",
		Services: []snapshot.Service{{
			ClusterID: "c1", Namespace: "shop", Name: "edge-lb", Type: "LoadBalancer",
			Selector: map[string]string{"app": "edge"},
			Ports: []snapshot.ServicePort{
				{Name: "https", Port: 443, TargetPort: 443, Protocol: "TCP"},
			},
			LoadBalancerIngressIPs: []string{"34.150.1.177"},
		}},
		// 网段登记填全：登记不全时入口地址的归属判不出来，EXPOSED_INGRESS
		// 一条规则都推不出，这条用例的前提就不成立了。
		Registry: snapshot.ClusterRegistry{
			ClusterID: "c1", PodCIDR: "10.4.0.0/16", NodeCIDR: "10.170.48.0/24",
		},
	}
	res := policygen.Generate(policygen.Input{ClusterID: "c1", Pods: pods, Assets: assets})

	kindsFor := func(workload string) []baseline.Kind {
		var kinds []baseline.Kind
		for _, p := range res.Policies {
			if p.Namespace != "shop" || p.Workload != workload {
				continue
			}
			for _, r := range p.Rules {
				if r.Baseline != nil {
					kinds = append(kinds, *r.Baseline)
				}
			}
		}
		return kinds
	}

	for _, k := range kindsFor("worker") {
		if k == baseline.KindExposedIngress {
			t.Fatalf("shop/worker 没有暴露对象，却拿到了 EXPOSED_INGRESS: %v", kindsFor("worker"))
		}
	}
	var edgeHasIt bool
	for _, k := range kindsFor("edge") {
		if k == baseline.KindExposedIngress {
			edgeHasIt = true
		}
	}
	if !edgeHasIt {
		t.Error("shop/edge 是真正被 LoadBalancer 暴露的 workload，却没拿到 EXPOSED_INGRESS")
	}
}

// I9（spec §3.6）：EXPOSED_INGRESS 命中风险端口清单时，候选规则要带上
// Risk 标注,且保持 Enabled——它描述的是集群已经在暴露的东西，不生成等于
// 切断现有流量；标注只是让它在界面上跟普通放行区分开。
// istio-ingressgateway-inner 暴露 6379（Redis）正是这个真实形态。
func TestExposedIngressRiskyPortIsAnnotatedButStillEnabled(t *testing.T) {
	pods := []replay.PodRef{
		{ClusterID: "c1", Namespace: "istio-system", Name: "inner-1", IP: "10.4.0.5",
			Labels: map[string]string{"app": "istio-ingressgateway-inner"}},
	}
	assets := snapshot.Assets{
		ClusterID: "c1",
		Services: []snapshot.Service{{
			ClusterID: "c1", Namespace: "istio-system", Name: "istio-ingressgateway-inner",
			Type:     "LoadBalancer",
			Selector: map[string]string{"app": "istio-ingressgateway-inner"},
			Ports: []snapshot.ServicePort{
				{Name: "redis", Port: 6379, TargetPort: 6379, Protocol: "TCP"},
			},
			LoadBalancerIngressIPs: []string{"10.170.48.55"},
		}},
		Registry: snapshot.ClusterRegistry{ClusterID: "c1", NodeCIDR: "10.170.48.0/24"},
	}
	res := policygen.Generate(policygen.Input{ClusterID: "c1", Pods: pods, Assets: assets})

	var found *policygen.Rule
	for _, p := range res.Policies {
		if p.Namespace != "istio-system" || p.Workload != "istio-ingressgateway-inner" {
			continue
		}
		for i, r := range p.Rules {
			if r.Baseline != nil && *r.Baseline == baseline.KindExposedIngress {
				found = &p.Rules[i]
			}
		}
	}
	if found == nil {
		t.Fatal("istio-system/istio-ingressgateway-inner 没有生成 EXPOSED_INGRESS 规则")
	}
	if found.Risk == nil || found.Risk.Category != risk.Database {
		t.Errorf("Risk = %+v，want 命中 Redis(6379)/DATABASE", found.Risk)
	}
	if !found.Enabled {
		t.Error("风险端口的暴露规则被禁用了——它描述的是现状，禁用会切断真实流量")
	}
}

// NC1: 没有 selector 的暴露型 Service（手工维护 Endpoints 的合法形态）不能
// 广播——它没有 workload 可挂，广播的后果是把 EXPOSED_INGRESS
// peers=[0.0.0.0/0] 发给这个 namespace 里完全无关的 workload，原样复现了
// 修复前 C1 的症状。它必须变成一条看得见的缺口，指名具体是哪个 Service。
func TestExposedIngressWithoutSelectorDoesNotBroadcastAndIsReported(t *testing.T) {
	pods := []replay.PodRef{
		{ClusterID: "c1", Namespace: "shop", Name: "worker-1", IP: "10.4.0.1",
			Labels: map[string]string{"app": "worker"}},
	}
	assets := snapshot.Assets{
		ClusterID: "c1",
		Services: []snapshot.Service{{
			ClusterID: "c1", Namespace: "shop", Name: "external-backend", Type: "LoadBalancer",
			// 没有 Selector：手工维护 Endpoints 的外部后端就是这个形态。
			Ports: []snapshot.ServicePort{
				{Name: "https", Port: 443, TargetPort: 443, Protocol: "TCP"},
			},
			LoadBalancerIngressIPs: []string{"34.150.1.177"},
		}},
		// 网段登记填全：登记不全时入口地址的归属判不出来，EXPOSED_INGRESS
		// 一条规则都推不出，这条用例的前提就不成立了。
		Registry: snapshot.ClusterRegistry{
			ClusterID: "c1", PodCIDR: "10.4.0.0/16", NodeCIDR: "10.170.48.0/24",
		},
	}
	res := policygen.Generate(policygen.Input{ClusterID: "c1", Pods: pods, Assets: assets})

	for _, p := range res.Policies {
		for _, r := range p.Rules {
			if r.Baseline != nil && *r.Baseline == baseline.KindExposedIngress {
				t.Errorf("%s/%s 拿到了没有 selector 的 Service 广播出来的 EXPOSED_INGRESS: %+v",
					p.Namespace, p.Workload, r)
			}
		}
	}

	var found bool
	for _, u := range res.UnattachedBaselines {
		if u.Kind == baseline.KindExposedIngress && u.Namespace == "shop" &&
			u.Name == "external-backend" && u.Reason == policygen.UnattachedBaselineNoSelector {
			found = true
		}
	}
	if !found {
		t.Errorf("shop/external-backend 没有 selector，却没有出现在 UnattachedBaselines 里: %+v",
			res.UnattachedBaselines)
	}
}

// NC2：Helm 常见的两标签形态——Pod 同时带 app.kubernetes.io/name 与 app 两个
// 不同取值，Service selector 只用 app。selector 解出的 workload 值与 Pod
// 真正的赢家标签键对不上，规则挂不到任何 workload 上。这不是罕见边界，是
// 复现于真实 fixture 形态（aggregate.go workloadLabelKeys 的注释原文）。
// 沉默的后果是候选集里什么迹象都没有——必须报出来。
func TestExposedIngressSelectorMismatchWithWinningKeyIsReported(t *testing.T) {
	pods := []replay.PodRef{
		{ClusterID: "c1", Namespace: "istio-system", Name: "gw-1", IP: "10.4.0.9",
			Labels: map[string]string{
				"app.kubernetes.io/name": "istio-ingressgateway", // 优先级更高，是赢家键
				"app":                    "gateway",
			}},
	}
	assets := snapshot.Assets{
		ClusterID: "c1",
		Services: []snapshot.Service{{
			ClusterID: "c1", Namespace: "istio-system", Name: "istio-ingressgateway",
			Type:     "LoadBalancer",
			Selector: map[string]string{"app": "gateway"}, // 只用 app，与赢家键不同
			Ports: []snapshot.ServicePort{
				{Name: "https", Port: 443, TargetPort: 443, Protocol: "TCP"},
			},
			LoadBalancerIngressIPs: []string{"34.150.1.177"},
		}},
		// 网段登记填全：登记不全时入口地址的归属判不出来，EXPOSED_INGRESS
		// 一条规则都推不出，这条用例的前提就不成立了。
		Registry: snapshot.ClusterRegistry{
			ClusterID: "c1", PodCIDR: "10.4.0.0/16", NodeCIDR: "10.170.48.0/24",
		},
	}
	res := policygen.Generate(policygen.Input{ClusterID: "c1", Pods: pods, Assets: assets})

	for _, p := range res.Policies {
		for _, r := range p.Rules {
			if r.Baseline != nil && *r.Baseline == baseline.KindExposedIngress {
				t.Errorf("%s/%s 不该拿到这条规则——selector 与赢家标签键不一致: %+v",
					p.Namespace, p.Workload, r)
			}
		}
	}

	var found bool
	for _, u := range res.UnattachedBaselines {
		if u.Kind == baseline.KindExposedIngress && u.Namespace == "istio-system" &&
			u.Name == "istio-ingressgateway" && u.Reason == policygen.UnattachedBaselineNoSuchWorkload {
			found = true
		}
	}
	if !found {
		t.Errorf("istio-system/istio-ingressgateway 的规则挂不上任何 workload，"+
			"却没有出现在 UnattachedBaselines 里: %+v", res.UnattachedBaselines)
	}
}

// UnattachedBaselines 恒为空切片而不是 nil：与 UnattachedImports 同一条
// 纪律，序列化成 null 会被读成"这一栏没算过"。
func TestNoUnattachedBaselinesIsAnEmptySliceNotNil(t *testing.T) {
	res := policygen.Generate(observe(t, "prod-asia-1"))
	if res.UnattachedBaselines == nil {
		t.Error("UnattachedBaselines 是 nil —— 它会序列化成 null，而空清单要读作" +
			"「五类都检查过、都挂上了」，不是「没算过」")
	}
}

// NC2 的另一个触发路径：Service selector 用的标签键根本不在
// workloadLabelKeys 里（比如只用 statefulset.kubernetes.io/pod-name 之类
// 的自定义键），resolveWorkloadLabel 直接解不出 workload 取值。同样不能
// 沉默——必须报出来，不能因为"这条路径比 Helm 两标签冲突少见"就不管。
func TestExposedIngressSelectorWithUnrecognizedLabelKeyIsReported(t *testing.T) {
	pods := []replay.PodRef{
		{ClusterID: "c1", Namespace: "shop", Name: "custom-1", IP: "10.4.0.9",
			Labels: map[string]string{"app": "custom-workload"}},
	}
	assets := snapshot.Assets{
		ClusterID: "c1",
		Services: []snapshot.Service{{
			ClusterID: "c1", Namespace: "shop", Name: "custom-lb", Type: "LoadBalancer",
			// 只用一个不在 workloadLabelKeys 里的键：resolveWorkloadLabel
			// 对这个 selector 直接返回 ok=false。
			Selector: map[string]string{"custom-selector-key": "custom-workload"},
			Ports: []snapshot.ServicePort{
				{Name: "https", Port: 443, TargetPort: 443, Protocol: "TCP"},
			},
			LoadBalancerIngressIPs: []string{"34.150.1.177"},
		}},
		// 网段登记填全：登记不全时入口地址的归属判不出来，EXPOSED_INGRESS
		// 一条规则都推不出，这条用例的前提就不成立了。
		Registry: snapshot.ClusterRegistry{
			ClusterID: "c1", PodCIDR: "10.4.0.0/16", NodeCIDR: "10.170.48.0/24",
		},
	}
	res := policygen.Generate(policygen.Input{ClusterID: "c1", Pods: pods, Assets: assets})

	var found bool
	for _, u := range res.UnattachedBaselines {
		if u.Kind == baseline.KindExposedIngress && u.Namespace == "shop" &&
			u.Name == "custom-lb" && u.Reason == policygen.UnattachedBaselineNoSuchWorkload {
			found = true
		}
	}
	if !found {
		t.Errorf("shop/custom-lb 的 selector 解不出 workload，却没有出现在 UnattachedBaselines 里: %+v",
			res.UnattachedBaselines)
	}
}

// NI2 补测：第三条判不出主体的路径——winKey 能查到，但 (namespace, workload,
// winKey) 不在花名册里。此前认为这条路径结构上很难触发（每一个在 winners
// 里留下主张的 workload，通常也会有一个 Pod 进了 workloads），但
// resolveWinningKeys 同时采信名册（fromRoster=true）与**观测流量里出现过的
// Pod**（fromRoster=false，aggregate.go:120-129）——一个只在流量里出现过、
// 从未进入 Input.Pods 的 Pod 照样能给 winners 留下一条主张。mesh/CCNP
// 降级的流量（IdentityTrusted=false）与已经删除/滚动过的 Pod 都会走到
// 这条路径，不是罕见边界。
func TestExposedIngressWinningKeyFromObservedOnlyPodIsReported(t *testing.T) {
	pods := []replay.PodRef{
		{ClusterID: "c1", Namespace: "shop", Name: "worker-1", IP: "10.4.0.1",
			Labels: map[string]string{"app": "worker"}},
	}
	// gw-1 只出现在观测流量里，从未进 Input.Pods——它不在花名册中。
	gwPod := replay.PodRef{
		ClusterID: "c1", Namespace: "shop", Name: "gw-1", IP: "10.4.0.9",
		Labels: map[string]string{"app": "gateway"},
	}
	obs := []policygen.Observation{{
		FlowID: "flow-degraded-1",
		Flow: replay.Flow{
			Source:   replay.Endpoint{ClusterID: "c1", IP: gwPod.IP, Pod: &gwPod},
			Dest:     replay.Endpoint{IP: "8.8.8.8"},
			Protocol: replay.ProtocolTCP, Port: 443,
		},
		// 身份不可信（mesh/CCNP 干扰）：整条流量判 Ungeneratable，但
		// resolveWinningKeys 仍然会看到 gw-1 的标签，留下一条 winners 主张——
		// 它不检查这条流量最终生不生成规则。
		IdentityTrusted: false,
	}}
	assets := snapshot.Assets{
		ClusterID: "c1",
		Services: []snapshot.Service{{
			ClusterID: "c1", Namespace: "shop", Name: "gw-lb", Type: "LoadBalancer",
			Selector: map[string]string{"app": "gateway"},
			Ports: []snapshot.ServicePort{
				{Name: "https", Port: 443, TargetPort: 443, Protocol: "TCP"},
			},
			LoadBalancerIngressIPs: []string{"34.150.1.177"},
		}},
		// 网段登记填全：登记不全时入口地址的归属判不出来，EXPOSED_INGRESS
		// 一条规则都推不出，这条用例的前提就不成立了。
		Registry: snapshot.ClusterRegistry{
			ClusterID: "c1", PodCIDR: "10.4.0.0/16", NodeCIDR: "10.170.48.0/24",
		},
	}
	res := policygen.Generate(policygen.Input{
		ClusterID: "c1", Pods: pods, Observations: obs, Assets: assets,
	})

	if len(res.Ungeneratable) == 0 {
		t.Fatal("身份不可信的流量没有进 Ungeneratable，这条用例的前提没建立起来")
	}
	for _, p := range res.Policies {
		for _, r := range p.Rules {
			if r.Baseline != nil && *r.Baseline == baseline.KindExposedIngress {
				t.Errorf("%s/%s 不该拿到这条规则——gw-1 从未进入花名册: %+v",
					p.Namespace, p.Workload, r)
			}
		}
	}

	var found bool
	for _, u := range res.UnattachedBaselines {
		if u.Kind == baseline.KindExposedIngress && u.Namespace == "shop" &&
			u.Name == "gw-lb" && u.Reason == policygen.UnattachedBaselineNoSuchWorkload {
			found = true
		}
	}
	if !found {
		t.Errorf("shop/gw-lb 的赢家键来自一个从未进花名册的观测 Pod，"+
			"却没有出现在 UnattachedBaselines 里: %+v", res.UnattachedBaselines)
	}
}
