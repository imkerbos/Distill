package collectstore

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	npav1 "sigs.k8s.io/network-policy-api/apis/v1alpha1"

	"github.com/imkerbos/Distill/internal/flow"
	"github.com/imkerbos/Distill/internal/policygen"
	"github.com/imkerbos/Distill/internal/replay"
)

// dry-run 必须拿到与判定那一屏同一份求值选项。
//
// 这条用例补的是一个**假绿**：predict 那边已经有一条"两条引擎必须同源"的
// 守卫，但它测的是 predict 自己的行为 —— 把这里的 EvalOptions 换成 nil，
// 那条守卫照样绿，因为没有任何东西断言 collectstore 真的传了。
//
// 传丢的后果是 dry-run 看不见 AdminNetworkPolicy：一条被 ANP Deny 拦着的
// 连接，/flows 说 DENY，dry-run 的基线说 ALLOW，WOULD_BREAK 于是算的是一次
// 不存在的中断，而那个数是写回门禁的判据。
func TestPredictWithCarriesTheEvaluatorOptions(t *testing.T) {
	nsLabel := func(v string) *metav1.LabelSelector {
		return &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": v}}
	}
	anp := npav1.AdminNetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "deny-gateway"},
		Spec: npav1.AdminNetworkPolicySpec{
			Priority: 10,
			Subject:  npav1.AdminNetworkPolicySubject{Namespaces: nsLabel("payment")},
			Ingress: []npav1.AdminNetworkPolicyIngressRule{{
				Name:   "deny-gw",
				Action: npav1.AdminNetworkPolicyRuleActionDeny,
				From:   []npav1.AdminNetworkPolicyIngressPeer{{Namespaces: nsLabel("gateway")}},
			}},
		},
	}
	namespaces := []replay.NamespaceRef{
		{ClusterID: "c1", Name: "gateway", Labels: map[string]string{"kubernetes.io/metadata.name": "gateway"}},
		{ClusterID: "c1", Name: "payment", Labels: map[string]string{"kubernetes.io/metadata.name": "payment"}},
	}
	src := replay.PodRef{ClusterID: "c1", Namespace: "gateway", Name: "gw-1", IP: "10.0.0.1"}
	dst := replay.PodRef{ClusterID: "c1", Namespace: "payment", Name: "api-1", IP: "10.0.0.2"}
	f := replay.Flow{
		Source:   replay.Endpoint{ClusterID: "c1", IP: src.IP, Pod: &src},
		Dest:     replay.Endpoint{ClusterID: "c1", IP: dst.IP, Pod: &dst},
		Protocol: replay.ProtocolTCP, Port: 8080,
	}
	opts := []replay.Option{replay.WithAdminPolicies([]npav1.AdminNetworkPolicy{anp}, nil)}

	live := replay.NewEvaluator("c1", nil, namespaces, opts...).Evaluate(f)
	if live.Verdict != replay.VerdictDeny {
		t.Fatalf("前提不成立：判定路径给出 %q，这条用例要的是 DENY", live.Verdict)
	}

	cs := candidateSet{
		traffic: traffic{
			described:  described{clusterID: "c1", completeness: flow.CompletenessComplete},
			namespaces: namespaces,
			evalOpts:   opts,
		},
		observations: []policygen.Observation{{FlowID: "f1", Flow: f, Decision: live}},
	}
	rep := cs.predictWith(nil)

	// 候选集为空 = 没有任何 NetworkPolicy，而 ANP 的 Deny 照旧成立：
	// dry-run 必须认为这条连接现在就是被拦着的，这次覆盖什么都没改变。
	if n := rep.Counts["WOULD_BREAK"]; n != 0 {
		t.Errorf("WOULD_BREAK = %d, want 0 —— dry-run 把一条已经被 ANP 拦住的连接"+
			"算成了「会被这次覆盖打断」", n)
	}
	if n := rep.Counts["UNCHANGED"]; n != 1 {
		t.Errorf("UNCHANGED = %d, want 1 —— 求值选项没有传到 dry-run", n)
	}
}

// 求值器与它用的那份选项必须出自同一次构造。
//
// 上一条用例直接构造 traffic，因此绕过了「trafficOf 到底有没有把选项存下来」
// 这一环 —— 实测把那一行改成 nil，上一条照样绿。newTraffic 把两者收成一个
// 落点，这条用例守住那个落点：eval 按选项求值，evalOpts 就是那份选项。
func TestNewTrafficKeepsTheOptionsItBuiltTheEvaluatorWith(t *testing.T) {
	nsLabel := func(v string) *metav1.LabelSelector {
		return &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": v}}
	}
	anp := npav1.AdminNetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "deny-gateway"},
		Spec: npav1.AdminNetworkPolicySpec{
			Priority: 10,
			Subject:  npav1.AdminNetworkPolicySubject{Namespaces: nsLabel("payment")},
			Ingress: []npav1.AdminNetworkPolicyIngressRule{{
				Name:   "deny-gw",
				Action: npav1.AdminNetworkPolicyRuleActionDeny,
				From:   []npav1.AdminNetworkPolicyIngressPeer{{Namespaces: nsLabel("gateway")}},
			}},
		},
	}
	namespaces := []replay.NamespaceRef{
		{ClusterID: "c1", Name: "gateway", Labels: map[string]string{"kubernetes.io/metadata.name": "gateway"}},
		{ClusterID: "c1", Name: "payment", Labels: map[string]string{"kubernetes.io/metadata.name": "payment"}},
	}
	src := replay.PodRef{ClusterID: "c1", Namespace: "gateway", Name: "gw-1", IP: "10.0.0.1"}
	dst := replay.PodRef{ClusterID: "c1", Namespace: "payment", Name: "api-1", IP: "10.0.0.2"}
	f := replay.Flow{
		Source:   replay.Endpoint{ClusterID: "c1", IP: src.IP, Pod: &src},
		Dest:     replay.Endpoint{ClusterID: "c1", IP: dst.IP, Pod: &dst},
		Protocol: replay.ProtocolTCP, Port: 8080,
	}
	opts := []replay.Option{replay.WithAdminPolicies([]npav1.AdminNetworkPolicy{anp}, nil)}

	tr := newTraffic(described{clusterID: "c1"}, nil, namespaces, opts)

	if got := tr.eval.Evaluate(f).Verdict; got != replay.VerdictDeny {
		t.Errorf("eval 没有按传入的选项求值：Verdict = %q, want DENY", got)
	}
	if len(tr.evalOpts) != len(opts) {
		t.Fatalf("evalOpts 有 %d 项，构造时给了 %d 项 —— dry-run 拿到的模型与判定那一屏不同",
			len(tr.evalOpts), len(opts))
	}
	// 光比长度不够：选项是函数，比不了相等。改为按它们的效果比 —— 用
	// evalOpts 重建一个求值器，对同一条流量必须给出同一个判定。
	rebuilt := replay.NewEvaluator("c1", nil, namespaces, tr.evalOpts...).Evaluate(f)
	if rebuilt.Verdict != tr.eval.Evaluate(f).Verdict {
		t.Errorf("按 evalOpts 重建的求值器给出 %q，而 eval 给出 %q —— 两者已经分叉",
			rebuilt.Verdict, tr.eval.Evaluate(f).Verdict)
	}
}
