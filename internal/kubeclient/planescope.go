package kubeclient

import (
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// PlaneScope 是一条平台不解释的策略所覆盖的主体范围。
//
// 只带 namespace 与一组标签相等条件 —— 这是从 CiliumNetworkPolicy 的
// endpointSelector 里**确定算得出来**的部分。它不描述那条策略放行了什么，
// 那是第二套引擎的事（design doc 2026-08-25 §6）。
type PlaneScope struct {
	// Namespace 为空表示集群级（CiliumClusterwideNetworkPolicy）。
	Namespace string
	// MatchLabels 为空表示选中该范围内全部主体，与 endpointSelector: {} 一致。
	MatchLabels map[string]string
}

// ciliumLabelPrefix 是 Cilium 给 Kubernetes 标签加的来源前缀。
//
// Cilium 的标签带来源命名空间：`k8s:app=api`、`reserved:host`、`any:foo`。
// endpointSelector 里写 `app: api` 与写 `k8s:app: api` 是同一件事，而**直接拿
// 后者去匹配 Kubernetes Pod 标签会匹配不上** —— 匹配不上就不降级，于是一条
// 真的被 CNP 管着的连接被判成可信。这是这段解析里唯一一个会静默出错、
// 且错在危险方向的地方。
const ciliumLabelPrefix = "k8s:"

// scopeOf 从一个 CNP/CCNP 对象里抽出覆盖范围。
//
// 第二个返回值为 false 表示**这条策略的覆盖范围解析不出来**，调用方必须据此
// 放弃精确降级、退回整片降级。解析不出来的情形：
//
//   - endpointSelector 带 matchExpressions —— 本轮不解析（Cilium 的表达式同样
//     可能带来源前缀，而一个解释错的表达式会圈出错误的主体集合）
//   - 标签键带 k8s: 之外的来源前缀（reserved:、any:）—— 那些不是 Kubernetes
//     标签，拿它去匹配 Pod 标签没有意义
//
// 两种都往"算不出"倒，不往"没覆盖"倒：后者会让一条真的被管着的连接显示成
// 可信，而这正是这个标记要防的事。
func scopeOf(obj unstructured.Unstructured, namespaced bool) (PlaneScope, bool) {
	sel, found, err := unstructured.NestedMap(obj.Object, "spec", "endpointSelector")
	if err != nil {
		return PlaneScope{}, false
	}
	scope := PlaneScope{}
	if namespaced {
		scope.Namespace = obj.GetNamespace()
	}
	if !found {
		// 没有 endpointSelector：Cilium 语义下这条策略不选中任何 endpoint，
		// 但也可能是它写在 specs[] 数组里（本轮不解析那种形态）。
		// **算不出**，不是"没覆盖"。
		return PlaneScope{}, false
	}

	if _, hasExpr := sel["matchExpressions"]; hasExpr {
		return PlaneScope{}, false
	}
	raw, hasLabels, err := unstructured.NestedStringMap(sel, "matchLabels")
	if err != nil {
		return PlaneScope{}, false
	}
	if !hasLabels || len(raw) == 0 {
		// 空 selector：选中该范围内全部主体。这是一个**确定**的答案。
		return scope, true
	}

	labels := make(map[string]string, len(raw))
	for k, v := range raw {
		key, ok := kubernetesLabelKey(k)
		if !ok {
			return PlaneScope{}, false
		}
		labels[key] = v
	}
	scope.MatchLabels = labels
	return scope, true
}

// kubernetesLabelKey 把 Cilium 的标签键还原成 Kubernetes 标签键。
//
// 第二个返回值为 false 表示这个键不是 Kubernetes 标签（带 reserved: / any:
// 之类的来源前缀），调用方据此放弃精确降级。
func kubernetesLabelKey(key string) (string, bool) {
	if after, cut := strings.CutPrefix(key, ciliumLabelPrefix); cut {
		return after, true
	}
	// 不带前缀即 Kubernetes 标签（Cilium 的默认来源就是 k8s）。
	// 但带**别的**来源前缀的不是 —— 用冒号粗判：Kubernetes 标签键里
	// 不允许出现冒号（前缀用 /，键名只允许字母数字与 -_.）。
	if strings.Contains(key, ":") {
		return "", false
	}
	return key, true
}
