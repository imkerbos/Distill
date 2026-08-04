package replay_test

import (
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"

	"github.com/imkerbos/Distill/internal/replay"
)

// 复现 bug：规则级 ipBlock CIDR 写错时，UNKNOWN/POLICY_MALFORMED 的 Detail
// 是空的，解释器因此说不出"卡在哪一条策略"。这是演示数据集里 40 条 UNKNOWN
// 里 30 条的成因——比 podSelector 写错常见得多，却是唯一不作解释的分支。
func TestEvaluateRuleLevelMalformedIPBlockHasDetail(t *testing.T) {
	bad := npIngress("payment", "bad-cidr", nil, []networkingv1.NetworkPolicyIngressRule{{
		From: []networkingv1.NetworkPolicyPeer{{
			IPBlock: &networkingv1.IPBlock{CIDR: "not-a-cidr"},
		}},
	}})

	gw := pod("gateway", "gw-1", "10.4.0.9", map[string]string{"app": "gateway"})
	api := pod("payment", "api-1", "10.4.0.1", map[string]string{"app": "api"})

	got := replay.NewEvaluator(testCluster, []networkingv1.NetworkPolicy{bad}, namespaces()).
		Evaluate(flowBetween(gw, api, 8080))

	if got.Verdict != replay.VerdictUnknown {
		t.Fatalf("Verdict = %q, want UNKNOWN", got.Verdict)
	}
	if got.UnknownReason != replay.ReasonPolicyMalformed {
		t.Fatalf("UnknownReason = %q, want %q", got.UnknownReason, replay.ReasonPolicyMalformed)
	}
	if got.Reason.Detail == "" {
		t.Fatal("Reason.Detail is empty; a rule-level malformed ipBlock must explain itself just like a malformed pod selector does")
	}
	if !strings.Contains(got.Reason.Detail, "payment/bad-cidr") {
		t.Errorf("Reason.Detail = %q, want it to name the offending policy payment/bad-cidr", got.Reason.Detail)
	}
	if got.Reason.MatchedPolicy != "" {
		t.Errorf("Reason.MatchedPolicy = %q, want empty; nothing matched, this must not look like a found allow rule",
			got.Reason.MatchedPolicy)
	}
}

// Detail 必须与策略选择器错误走同一条"字典序取最小"比较，否则同一份策略集
// 换个切片顺序会吐出两个不同的 Decision——这条流量最终会变成一次策略推荐，
// 顺序不确定的推荐是不可接受的。
func TestEvaluateRuleLevelMalformedIPBlockDetailIsOrderIndependent(t *testing.T) {
	badA := npIngress("payment", "bad-a", nil, []networkingv1.NetworkPolicyIngressRule{{
		From: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "not-a-cidr"}}},
	}})
	badB := npIngress("payment", "bad-b", nil, []networkingv1.NetworkPolicyIngressRule{{
		From: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "also-not-a-cidr"}}},
	}})

	gw := pod("gateway", "gw-1", "10.4.0.9", map[string]string{"app": "gateway"})
	api := pod("payment", "api-1", "10.4.0.1", map[string]string{"app": "api"})

	forward, backward := evaluateBothOrders(t,
		[]networkingv1.NetworkPolicy{badA, badB}, flowBetween(gw, api, 8080))

	if forward != backward {
		t.Errorf("policy slice order changed the decision:\n [badA,badB] = %+v\n [badB,badA] = %+v",
			forward, backward)
	}
	if forward.Reason.Detail == "" {
		t.Fatal("Reason.Detail must survive in both orders")
	}
	// "bad-a" 在字典序上小于 "bad-b"，同一条比较规则不论遍历顺序都必须选中它。
	if !strings.Contains(forward.Reason.Detail, "payment/bad-a") {
		t.Errorf("Reason.Detail = %q, want it to deterministically pick payment/bad-a regardless of slice order",
			forward.Reason.Detail)
	}
}
