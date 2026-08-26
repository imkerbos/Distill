package predict_test

import (
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/imkerbos/Distill/internal/predict"
)

func policy(ns, name string) networkingv1.NetworkPolicy {
	return networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
	}
}

// 已有与候选并成一份：平台只加不删，apply 之后两者都在。
func TestWithExistingKeepsBothSides(t *testing.T) {
	got := predict.WithExisting(
		[]networkingv1.NetworkPolicy{policy("payment", "legacy-allow")},
		[]networkingv1.NetworkPolicy{policy("payment", "candidate-api-ingress")},
	)
	if len(got) != 2 {
		t.Fatalf("合并出 %d 条, want 2 —— 平台只加不删，已有策略不会因写回消失", len(got))
	}
}

// **同名对象只留一份，且留的是候选那一版。**
//
// 第二次写回时集群里已经有平台上一轮写下、并已被 GitOps 落地的 candidate-*
// 对象。两版都塞进去，回放会把同一个对象算两遍 —— additive-allow 下结果碰巧
// 不变，但那是巧合；apply 之后集群里只会有一份。
func TestWithExistingDedupesByObjectIdentity(t *testing.T) {
	old := policy("payment", "candidate-api-ingress")
	old.Spec.PolicyTypes = []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}
	fresh := policy("payment", "candidate-api-ingress")
	fresh.Spec.PolicyTypes = []networkingv1.PolicyType{
		networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress,
	}

	got := predict.WithExisting(
		[]networkingv1.NetworkPolicy{old},
		[]networkingv1.NetworkPolicy{fresh},
	)
	if len(got) != 1 {
		t.Fatalf("同名对象留了 %d 份, want 1：apply 之后集群里只会有一份", len(got))
	}
	if len(got[0].Spec.PolicyTypes) != 2 {
		t.Error("留下的是集群里那一版旧的，而将要 apply 的是候选那一版")
	}
}

// 同名但不同 namespace 是两个对象，不能折叠。
func TestWithExistingKeepsSameNameInDifferentNamespaces(t *testing.T) {
	got := predict.WithExisting(
		[]networkingv1.NetworkPolicy{policy("payment", "deny-all")},
		[]networkingv1.NetworkPolicy{policy("shop", "deny-all")},
	)
	if len(got) != 2 {
		t.Fatalf("折叠成了 %d 条：namespace 不同就是两个对象", len(got))
	}
}

// 输出确定排序：两份预测的差额只有在输入稳定时才解释得了。
func TestWithExistingIsDeterministic(t *testing.T) {
	existing := []networkingv1.NetworkPolicy{
		policy("shop", "b"), policy("payment", "z"), policy("payment", "a"),
	}
	first := predict.WithExisting(existing, nil)
	for range 5 {
		got := predict.WithExisting(existing, nil)
		for i := range got {
			if got[i].Name != first[i].Name || got[i].Namespace != first[i].Namespace {
				t.Fatalf("同一批输入两次给出不同次序：%v vs %v", got, first)
			}
		}
	}
	if first[0].Namespace != "payment" || first[0].Name != "a" {
		t.Errorf("没有按 (namespace, name) 排序：%v", first)
	}
}

// **不得就地改动入参。**
//
// 两份预测跑在同一批输入上，就地排序会让先跑的那一份改掉后跑那一份的输入。
func TestWithExistingDoesNotMutateInput(t *testing.T) {
	existing := []networkingv1.NetworkPolicy{policy("shop", "b"), policy("payment", "a")}
	candidates := []networkingv1.NetworkPolicy{policy("batch", "c")}
	predict.WithExisting(existing, candidates)

	if existing[0].Namespace != "shop" || existing[1].Namespace != "payment" {
		t.Errorf("入参被就地排序了：%v", existing)
	}
	if len(candidates) != 1 {
		t.Errorf("候选切片被改了：%v", candidates)
	}
}

// 任一侧为空都成立：新集群没有已有策略，只看 Baseline 的集群没有候选。
func TestWithExistingHandlesEmptySides(t *testing.T) {
	if got := predict.WithExisting(nil, nil); len(got) != 0 {
		t.Errorf("两侧都空却给出 %d 条", len(got))
	}
	if got := predict.WithExisting(nil, []networkingv1.NetworkPolicy{policy("a", "b")}); len(got) != 1 {
		t.Errorf("没有已有策略时丢掉了候选：%d", len(got))
	}
}
