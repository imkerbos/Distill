package kubeclient

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func cnp(namespace string, selector map[string]any) unstructured.Unstructured {
	obj := map[string]any{
		"apiVersion": "cilium.io/v2",
		"kind":       "CiliumNetworkPolicy",
		"metadata":   map[string]any{"name": "p", "namespace": namespace},
	}
	if selector != nil {
		obj["spec"] = map[string]any{"endpointSelector": selector}
	}
	return unstructured.Unstructured{Object: obj}
}

// **Cilium 的 k8s: 前缀必须剥掉。**
//
// endpointSelector 里写 `app: api` 与写 `k8s:app: api` 是同一件事，而直接拿
// 后者去匹配 Kubernetes Pod 标签会匹配不上 —— 匹配不上就不降级，于是一条真的
// 被 CNP 管着的连接被判成可信。这是这段解析里唯一一个会静默出错、且错在
// 危险方向的地方。
func TestCiliumLabelPrefixIsStripped(t *testing.T) {
	got, ok := scopeOf(cnp("payment", map[string]any{
		"matchLabels": map[string]any{"k8s:app": "api"},
	}), true)
	if !ok {
		t.Fatal("带 k8s: 前缀的 selector 被判成解析不出来")
	}
	if got.MatchLabels["app"] != "api" {
		t.Errorf("MatchLabels = %v，前缀没有剥掉：拿它去匹配 Pod 标签会匹配不上，"+
			"于是该降级的连接被判成可信", got.MatchLabels)
	}
	if got.Namespace != "payment" {
		t.Errorf("Namespace = %q, want payment", got.Namespace)
	}
}

// 不带前缀的键原样保留 —— Cilium 的默认来源就是 k8s。
func TestPlainLabelKeysAreKeptAsIs(t *testing.T) {
	got, ok := scopeOf(cnp("shop", map[string]any{
		"matchLabels": map[string]any{"app.kubernetes.io/name": "web"},
	}), true)
	if !ok || got.MatchLabels["app.kubernetes.io/name"] != "web" {
		t.Errorf("scopeOf() = %v, %v", got, ok)
	}
}

// **别的来源前缀解析不出来 → 放弃精确降级。**
//
// reserved:host、any:foo 不是 Kubernetes 标签，拿它去匹配 Pod 标签没有意义。
// 往"算不出"倒，不往"没覆盖"倒。
func TestForeignLabelSourcesAreUnparseable(t *testing.T) {
	for _, key := range []string{"reserved:host", "any:role", "fqdn:example.com"} {
		if _, ok := scopeOf(cnp("payment", map[string]any{
			"matchLabels": map[string]any{key: "x"},
		}), true); ok {
			t.Errorf("%q 被当成了可解析的 Kubernetes 标签", key)
		}
	}
}

// matchExpressions 本轮不解析：Cilium 的表达式同样可能带来源前缀，
// 而一个解释错的表达式会圈出错误的主体集合。
func TestMatchExpressionsAreUnparseable(t *testing.T) {
	if _, ok := scopeOf(cnp("payment", map[string]any{
		"matchExpressions": []any{map[string]any{
			"key": "app", "operator": "In", "values": []any{"api"},
		}},
	}), true); ok {
		t.Error("带 matchExpressions 的 selector 被当成可解析")
	}
}

// 空 selector 是一个**确定**的答案：选中该范围内全部主体。
func TestEmptySelectorIsAConfidentSelectAll(t *testing.T) {
	got, ok := scopeOf(cnp("payment", map[string]any{}), true)
	if !ok {
		t.Fatal("空 endpointSelector 被判成解析不出来 —— 它是 select-all，是确定的")
	}
	if len(got.MatchLabels) != 0 {
		t.Errorf("MatchLabels = %v, want 空（选中全部）", got.MatchLabels)
	}
	if got.Namespace != "payment" {
		t.Errorf("Namespace = %q", got.Namespace)
	}
}

// 压根没有 endpointSelector：**算不出**，不是"没覆盖"。
//
// 它可能写在 specs[] 数组里，而本轮不解析那种形态。
func TestMissingSelectorIsUnparseableNotEmpty(t *testing.T) {
	if _, ok := scopeOf(cnp("payment", nil), true); ok {
		t.Error("没有 endpointSelector 的对象被当成「没覆盖任何主体」——" +
			"它可能把 selector 写在 specs[] 里，那时精确降级会漏掉它")
	}
}

// 集群级对象不带 namespace：它跨全部 namespace 生效。
func TestClusterWideScopeCarriesNoNamespace(t *testing.T) {
	got, ok := scopeOf(cnp("", map[string]any{
		"matchLabels": map[string]any{"tier": "edge"},
	}), false)
	if !ok {
		t.Fatal("集群级对象解析失败")
	}
	if got.Namespace != "" {
		t.Errorf("Namespace = %q, want 空（集群级跨全部 namespace）", got.Namespace)
	}
}
