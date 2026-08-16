package collect

import (
	"context"
	"errors"
	"strings"
	"testing"

	authv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// reviewKey 把一次 SelfSubjectAccessReview 的问询压成一个可比较的字符串。
func reviewKey(spec authv1.SelfSubjectAccessReviewSpec) string {
	r := spec.ResourceAttributes
	if r == nil {
		return "<no resource attributes>"
	}
	return r.Verb + " " + r.Resource + "." + r.Group
}

// answerReviews 让 fake 按 allow 回答每一次自检问询，并记录问了什么。
//
// fake clientset 不做任何鉴权，SSAR 打到它身上永远返回一个零值 ——
// 也就是"未授权"。这里必须自己接管应答，否则"守卫发现写权限"这条
// 分支在 fake 上根本不可达。
func answerReviews(cs *fake.Clientset, allow func(string) bool) *[]string {
	var asked []string
	cs.PrependReactor("create", "selfsubjectaccessreviews",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			review, ok := action.(k8stesting.CreateAction).GetObject().(*authv1.SelfSubjectAccessReview)
			if !ok {
				return true, nil, errors.New("unexpected object submitted for review")
			}
			key := reviewKey(review.Spec)
			asked = append(asked, key)
			out := review.DeepCopy()
			out.Status.Allowed = allow(key)
			return true, out, nil
		})
	return &asked
}

func TestAssertReadOnlyAcceptsACredentialWithNoWriteAccess(t *testing.T) {
	cs := fake.NewClientset()
	asked := answerReviews(cs, func(string) bool { return false })

	if err := AssertReadOnly(context.Background(), cs); err != nil {
		t.Fatalf("AssertReadOnly = %v, want nil", err)
	}
	if len(*asked) != len(policyResources)*len(writeVerbs) {
		t.Errorf("asked %d questions, want %d (every write verb on every policy resource)",
			len(*asked), len(policyResources)*len(writeVerbs))
	}
}

// 三类策略资源乘五个写动词，一个都不能少。少问一格就是一个能悄悄
// 通过自检的凭据，而 Cilium 的两类同样能阻断生产流量。
func TestAssertReadOnlyAsksAboutEveryPolicyResourceAndVerb(t *testing.T) {
	cs := fake.NewClientset()
	asked := answerReviews(cs, func(string) bool { return false })

	if err := AssertReadOnly(context.Background(), cs); err != nil {
		t.Fatalf("AssertReadOnly = %v", err)
	}

	got := map[string]bool{}
	for _, k := range *asked {
		got[k] = true
	}
	for _, res := range []string{
		"networkpolicies.networking.k8s.io",
		"ciliumnetworkpolicies.cilium.io",
		"ciliumclusterwidenetworkpolicies.cilium.io",
	} {
		for _, verb := range []string{"create", "update", "patch", "delete", "deletecollection"} {
			if !got[verb+" "+res] {
				t.Errorf("never asked whether it may %s %s", verb, res)
			}
		}
	}
}

func TestAssertReadOnlyRefusesWhenAnyWriteVerbIsGranted(t *testing.T) {
	cases := []string{
		"create networkpolicies.networking.k8s.io",
		"delete networkpolicies.networking.k8s.io",
		"deletecollection networkpolicies.networking.k8s.io",
		"patch ciliumnetworkpolicies.cilium.io",
		"update ciliumclusterwidenetworkpolicies.cilium.io",
	}
	for _, granted := range cases {
		t.Run(granted, func(t *testing.T) {
			cs := fake.NewClientset()
			answerReviews(cs, func(k string) bool { return k == granted })

			err := AssertReadOnly(context.Background(), cs)
			if err == nil {
				t.Fatalf("AssertReadOnly = nil, want a refusal for a credential granted %q", granted)
			}
			if !strings.Contains(err.Error(), granted) {
				t.Errorf("error %q does not name the granted permission %q", err, granted)
			}
		})
	}
}

func TestAssertReadOnlyNamesEveryGrantedPermission(t *testing.T) {
	cs := fake.NewClientset()
	answerReviews(cs, func(k string) bool {
		return k == "create networkpolicies.networking.k8s.io" ||
			k == "delete ciliumclusterwidenetworkpolicies.cilium.io"
	})

	err := AssertReadOnly(context.Background(), cs)
	if err == nil {
		t.Fatal("AssertReadOnly = nil, want a refusal")
	}
	for _, want := range []string{
		"create networkpolicies.networking.k8s.io",
		"delete ciliumclusterwidenetworkpolicies.cilium.io",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q omits %q; the operator cannot fix what is not named", err, want)
		}
	}
}

// 连"我有没有写权限"都问不出来的时候，假定没有是这条守卫最没用的
// 失败方向。审查本身失败必须拒绝启动。
func TestAssertReadOnlyRefusesWhenTheReviewItselfFails(t *testing.T) {
	cs := fake.NewClientset()
	cs.PrependReactor("create", "selfsubjectaccessreviews",
		func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("apiserver unreachable")
		})

	if err := AssertReadOnly(context.Background(), cs); err == nil {
		t.Fatal("AssertReadOnly = nil when the review could not be answered, want a refusal")
	}
}

// 守卫必须在第一次问询失败时就停下，而不是把剩下十四次也问一遍：
// 一个连不上的 apiserver 不会在第二个动词上变得可达。
func TestAssertReadOnlyStopsAtTheFirstUnanswerableReview(t *testing.T) {
	cs := fake.NewClientset()
	calls := 0
	cs.PrependReactor("create", "selfsubjectaccessreviews",
		func(k8stesting.Action) (bool, runtime.Object, error) {
			calls++
			return true, nil, errors.New("apiserver unreachable")
		})

	if err := AssertReadOnly(context.Background(), cs); err == nil {
		t.Fatal("AssertReadOnly = nil, want a refusal")
	}
	if calls != 1 {
		t.Errorf("issued %d reviews after the first failure, want 1", calls)
	}
}

// AssertReadOnly 的价值全在"它在采集之前跑过"。本包里 Collect 不调用它，
// 调用点属于尚未写出的 cmd/distill-collector（见 docs/TODO.md）。
// 这条测试钉住能钉的那一半：守卫自身只发出 SSAR，不碰任何被采集的资源，
// 因此它可以安全地放在采集之前。真实 apiserver 是否会拒绝一个越权凭据，
// fake clientset 证明不了 —— 那只能在 kind 上证伪。
func TestAssertReadOnlyTouchesNothingButAccessReviews(t *testing.T) {
	cs := fake.NewClientset()
	answerReviews(cs, func(string) bool { return false })

	if err := AssertReadOnly(context.Background(), cs); err != nil {
		t.Fatalf("AssertReadOnly = %v", err)
	}
	for _, a := range cs.Actions() {
		if a.GetResource().Resource != "selfsubjectaccessreviews" {
			t.Errorf("AssertReadOnly issued %s on %q; the guard must only ask, never read or write cluster state",
				a.GetVerb(), a.GetResource().Resource)
		}
	}
}
