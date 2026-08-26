package collect

import (
	"strconv"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/imkerbos/Distill/internal/snapshot"
)

var (
	anpGVR = schema.GroupVersionResource{
		Group: adminPolicyGroup, Version: adminPolicyVersion, Resource: "adminnetworkpolicies"}
	banpGVR = schema.GroupVersionResource{
		Group: adminPolicyGroup, Version: adminPolicyVersion, Resource: "baselineadminnetworkpolicies"}
)

// adminPolicyListKinds 让 fake 动态客户端知道这两类的 List kind。
func adminPolicyListKinds() map[schema.GroupVersionResource]string {
	return map[schema.GroupVersionResource]string{
		anpGVR:  "AdminNetworkPolicyList",
		banpGVR: "BaselineAdminNetworkPolicyList",
	}
}

// newDynamic 造一个装了给定对象的 fake 动态客户端。
func newDynamic(objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(), adminPolicyListKinds(), objects...)
}

// anpObject 造一条 AdminNetworkPolicy。priority 传负数表示不设这个字段。
func anpObject(name string, priority int64) *unstructured.Unstructured {
	spec := map[string]any{"subject": map[string]any{"namespaces": map[string]any{}}}
	if priority >= 0 {
		spec["priority"] = priority
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": adminPolicyGroup + "/" + adminPolicyVersion,
		"kind":       "AdminNetworkPolicy",
		"metadata":   map[string]any{"name": name, "uid": "uid-" + name},
		"spec":       spec,
	}}
}

func banpObject(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": adminPolicyGroup + "/" + adminPolicyVersion,
		"kind":       "BaselineAdminNetworkPolicy",
		"metadata":   map[string]any{"name": name, "uid": "uid-" + name},
		"spec":       map[string]any{"subject": map[string]any{"namespaces": map[string]any{}}},
	}}
}

func collectAdmin(t *testing.T, dyn dynamic.Interface) (snapshot.Observation, error) {
	t.Helper()
	c := New(testClusterID, nil, dyn, nil)
	var obs snapshot.Observation
	err := c.collectAdminPolicies(t.Context(), &obs)
	return obs, err
}

func TestCollectAdminPoliciesKeepsKindAndPriority(t *testing.T) {
	obs, err := collectAdmin(t, newDynamic(anpObject("tenant-isolation", 20), banpObject("default")))
	if err != nil {
		t.Fatalf("collectAdminPolicies() error = %v", err)
	}
	if len(obs.AdminPolicies) != 2 {
		t.Fatalf("collected %d admin policies, want 2", len(obs.AdminPolicies))
	}

	byName := map[string]snapshot.AdminPolicy{}
	for _, p := range obs.AdminPolicies {
		byName[p.Name] = p
	}

	anp := byName["tenant-isolation"]
	if anp.Kind != snapshot.AdminPolicyAdmin {
		t.Errorf("ANP kind = %q, want %q", anp.Kind, snapshot.AdminPolicyAdmin)
	}
	if !anp.PriorityKnown || anp.Priority != 20 {
		t.Errorf("ANP priority = %d (known=%v), want 20 (known=true)", anp.Priority, anp.PriorityKnown)
	}
	// 原文必须能被当作一份 Kubernetes 清单读回去：List 返回的对象不带
	// TypeMeta，补不回来就再也认不出它是什么。
	for _, want := range []string{"apiVersion: policy.networking.k8s.io/v1alpha1", "kind: AdminNetworkPolicy"} {
		if !strings.Contains(anp.Manifest, want) {
			t.Errorf("ANP manifest is missing %q:\n%s", want, anp.Manifest)
		}
	}

	banp := byName["default"]
	if banp.Kind != snapshot.AdminPolicyBaseline {
		t.Errorf("BANP kind = %q, want %q", banp.Kind, snapshot.AdminPolicyBaseline)
	}
	// BANP 没有优先级，它永远在 NetworkPolicy 之后兜底。
	if banp.PriorityKnown {
		t.Errorf("BANP reports a known priority; it has none")
	}
}

// 优先级 0 是合法的、且是最高的那一个。它必须与"没读到优先级"分得开 ——
// 合并之后，一条读不懂的策略会被排到所有策略之前。
func TestCollectAdminPoliciesTellsPriorityZeroFromUnknown(t *testing.T) {
	obs, err := collectAdmin(t, newDynamic(anpObject("highest", 0), anpObject("no-priority", -1)))
	if err != nil {
		t.Fatalf("collectAdminPolicies() error = %v", err)
	}
	for _, p := range obs.AdminPolicies {
		switch p.Name {
		case "highest":
			if !p.PriorityKnown || p.Priority != 0 {
				t.Errorf("priority 0 came back as %d (known=%v), want 0 (known=true)", p.Priority, p.PriorityKnown)
			}
		case "no-priority":
			if p.PriorityKnown {
				t.Errorf("a policy with no spec.priority reports a known priority of %d", p.Priority)
			}
		}
	}
}

// 超出 API 定义范围的优先级不落库：int64 转 int32 会静默截断，
// 而截断出来的数看上去和一个真的优先级一模一样。
func TestCollectAdminPoliciesRejectsOutOfRangePriority(t *testing.T) {
	obs, err := collectAdmin(t, newDynamic(anpObject("bogus", 1<<32+7)))
	if err != nil {
		t.Fatalf("collectAdminPolicies() error = %v", err)
	}
	if len(obs.AdminPolicies) != 1 {
		t.Fatalf("collected %d admin policies, want 1", len(obs.AdminPolicies))
	}
	if p := obs.AdminPolicies[0]; p.PriorityKnown {
		t.Errorf("an out-of-range priority came back as the usable value %d", p.Priority)
	}
}

// CRD 没装是一个确定的"这里不可能有这类对象"，不是一次采集失败。
// 记成失败会让每一个正常集群的每一次采集都带着一条永远修不掉的失败记录。
func TestCollectAdminPoliciesTreatsAbsentCRDAsEmpty(t *testing.T) {
	dyn := newDynamic()
	dyn.PrependReactor("list", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.NewNotFound(
			schema.GroupResource{Group: adminPolicyGroup, Resource: "adminnetworkpolicies"}, "")
	})

	obs, err := collectAdmin(t, dyn)
	if err != nil {
		t.Fatalf("an absent CRD was reported as a collection failure: %v", err)
	}
	if len(obs.AdminPolicies) != 0 {
		t.Errorf("collected %d admin policies from a cluster with no CRD", len(obs.AdminPolicies))
	}
}

// 无权限是真的没查出结果，必须失败 —— 它与"集群里没有 ANP"在库里长得
// 一模一样，而后者会让下游认为不存在管理面策略。
func TestCollectAdminPoliciesFailsOnForbidden(t *testing.T) {
	dyn := newDynamic()
	dyn.PrependReactor("list", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.NewForbidden(
			schema.GroupResource{Group: adminPolicyGroup, Resource: "adminnetworkpolicies"}, "", nil)
	})

	if _, err := collectAdmin(t, dyn); err == nil {
		t.Fatal("a forbidden list was reported as a successful collection of zero admin policies")
	}
}

// 没有动态客户端时同样必须失败：静默跳过之后，库里的"零条 ANP"与一个
// 真的没有 ANP 的集群完全一样。
func TestCollectAdminPoliciesFailsWithoutDynamicClient(t *testing.T) {
	if _, err := collectAdmin(t, nil); err == nil {
		t.Fatal("a collector with no dynamic client reported a successful collection")
	}
}

// 超过上限整类失败，不落被截断的一半：管理面策略是有序短路求值的，
// 少一条就可能少掉那条命中的 Deny。
func TestCollectAdminPoliciesRefusesTruncation(t *testing.T) {
	objs := make([]runtime.Object, 0, maxAdminPolicies+1)
	for i := range maxAdminPolicies + 1 {
		objs = append(objs, anpObject("anp-"+strconv.Itoa(i), int64(i%1001)))
	}

	obs, err := collectAdmin(t, newDynamic(objs...))
	if err == nil {
		t.Fatalf("collected %d admin policies past the %d cap instead of refusing",
			len(obs.AdminPolicies), maxAdminPolicies)
	}
	if !strings.Contains(err.Error(), "truncated") {
		t.Errorf("error = %v, want it to say the view would be truncated", err)
	}
}
