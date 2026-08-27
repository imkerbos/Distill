package predict_test

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	npav1 "sigs.k8s.io/network-policy-api/apis/v1alpha1"

	"github.com/imkerbos/Distill/internal/policygen"
	"github.com/imkerbos/Distill/internal/predict"
	"github.com/imkerbos/Distill/internal/replay"
)

// dry-run 与 /flows 那一屏必须用同一个模型解释同一个集群。
//
// 这条用例来自一个真实缺口：判定那一侧接了 AdminNetworkPolicy 与精确降级
// 范围，dry-run 那一侧只拼了一个 ForeignPlane 布尔，于是同一个集群被两套
// 模型解释 —— 一条被 ANP Deny 拦着的连接，/flows 说 DENY，dry-run 的基线
// 说 ALLOW，WOULD_BREAK 因此算的是一次不存在的中断，而那个数是写回门禁
// 的判据。代码里那句「对账器与读栈同源」要的正是这件事。
//
// 断言的形式是：拿同一份选项分别喂给两条路，基线判定必须一致。少传一项
// 的调用方会在这里变红。
func TestDryRunUsesTheSameModelAsTheVerdictPath(t *testing.T) {
	src := pod("gateway", "gateway-1", "gateway")
	dst := pod("payment", "payment-1", "api")
	f := flow(src, dst, 8080)

	// 一条 ANP：Deny 从 gateway 命名空间到 payment 的入向。
	anp := npav1.AdminNetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "deny-gateway"},
		Spec: npav1.AdminNetworkPolicySpec{
			Priority: 10,
			Subject: npav1.AdminNetworkPolicySubject{
				Namespaces: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "payment"}},
			},
			Ingress: []npav1.AdminNetworkPolicyIngressRule{{
				Name:   "deny-gw",
				Action: npav1.AdminNetworkPolicyRuleActionDeny,
				From: []npav1.AdminNetworkPolicyIngressPeer{{
					Namespaces: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "gateway"}},
				}},
			}},
		},
	}
	opts := []replay.Option{replay.WithAdminPolicies([]npav1.AdminNetworkPolicy{anp}, nil)}

	// 判定那一侧：直接用求值器，这是 /flows 走的路。
	live := replay.NewEvaluator("c1", nil, nss(), opts...).Evaluate(f)
	if live.Verdict != replay.VerdictDeny {
		t.Fatalf("前提不成立：判定路径给出 %q，这条用例要的是 DENY", live.Verdict)
	}

	// dry-run 那一侧：候选集为空，于是它回放的就是基线，两者必须一致。
	rep := predict.Run(predict.Input{
		ClusterID:   "c1",
		Namespaces:  nss(),
		EvalOptions: opts,
		Observations: []policygen.Observation{{
			FlowID: "f1", Flow: f, Decision: live,
		}},
		Label: func(replay.Endpoint) string { return "" },
	})

	// 候选集为空 = 没有任何策略，而 ANP 的 Deny 照旧成立：dry-run 必须
	// 认为这条连接现在就是被拦着的，因此这次覆盖什么都没改变。
	if n := rep.Counts[predict.ChangeWouldBreak]; n != 0 {
		t.Errorf("WOULD_BREAK = %d, want 0 —— dry-run 把一条已经被 ANP 拦住的连接"+
			"算成了「会被这次覆盖打断」，那是一次不存在的中断，而这个数是写回门禁的判据", n)
	}
	if n := rep.Counts[predict.ChangeUnchanged]; n != 1 {
		t.Errorf("UNCHANGED = %d, want 1 —— 两条路对同一条流量必须给出同一个判定", n)
	}
}
