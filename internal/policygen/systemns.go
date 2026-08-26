package policygen

import "sort"

// systemNamespaces 是 Kubernetes 内置的系统命名空间。
//
// **默认不为它们生成候选策略。** 候选集本质上是给每个 workload 装上
// default-deny、再把观测到的连接一条条放回去；而观测窗口证明不了完整时，
// 学出来的规则默认不启用 —— 于是 kube-dns 会拿到一份"只放行 Baseline"的
// default-deny ingress，全集群的 DNS 解析随之中断。
//
// 这不是假设。真集群上实测：kube-system/kube-dns 的候选里，各 namespace 到
// UDP/53 的规则全部 enabled=false，而 dry-run 报出 14 条 DNS 会被拦断
// （2026-08-26）。dry-run 确实报了出来 —— 但 DNS 断与一条业务连接断在计数里
// 是平等的，屏幕上没有任何东西说"这 14 条会让整个集群失去 DNS"。
//
// **只列内置的三个，不把 argocd、istio-system 之类加进来。** 那些是用户装的，
// 哪些算基础设施只有集群管理员知道；硬编码一份清单等于替他做一个他没做过的
// 判断，而漏掉的那个会被静默地下发 default-deny。要排除它们走的是登记那条路，
// 不是猜。
//
// 排除只影响**生成**，不影响**判定**：这些 namespace 的 Pod 照常参与流量归属
// 与回放，否则对账与 dry-run 会跟着一起错。
var systemNamespaces = map[string]bool{
	"kube-system":     true,
	"kube-public":     true,
	"kube-node-lease": true,
}

// IsSystemNamespace 报告某个命名空间是否为 Kubernetes 内置系统命名空间。
func IsSystemNamespace(ns string) bool { return systemNamespaces[ns] }

// NamespaceExclusionReason 是命名空间未进候选集的原因。封闭枚举。
type NamespaceExclusionReason string

const (
	// NamespaceExclusionSystem 表示这是 Kubernetes 内置系统命名空间。
	NamespaceExclusionSystem NamespaceExclusionReason = "SYSTEM_NAMESPACE"
)

// ExcludedNamespace 是一个整体未进候选集的命名空间。
//
// **必须报出来，不能静默消失。** 一个悄悄不见的 namespace，在界面上与
// "这个 namespace 没有 workload" 长得一样，而操作者据此以为平台看过了、
// 覆盖是完整的 —— 与 ExcludedWorkloads 同一条纪律。
type ExcludedNamespace struct {
	Namespace string                   `json:"namespace"`
	Reason    NamespaceExclusionReason `json:"reason"`
}

// namespaceGate 决定某个命名空间要不要生成候选策略。
//
// managed 是操作者在集群登记里明示"这个系统命名空间由平台管"的那些。
// **默认不碰不等于永远不能碰**：没有这条出口，这道保护就成了一堵没有门的墙，
// 而真正需要管控 kube-system 的集群会被迫绕过整个平台。
func namespaceGate(managed []string) func(string) (ExcludedNamespace, bool) {
	allow := make(map[string]bool, len(managed))
	for _, ns := range managed {
		allow[ns] = true
	}
	return func(ns string) (ExcludedNamespace, bool) {
		if !IsSystemNamespace(ns) || allow[ns] {
			return ExcludedNamespace{}, false
		}
		return ExcludedNamespace{Namespace: ns, Reason: NamespaceExclusionSystem}, true
	}
}

// sortedExcludedNamespaces 把排除集排成确定顺序。
//
// 与 Policies 同一条理由：这份清单会进界面与响应体，而一个随 map 遍历顺序
// 变化的清单会让同一批输入每次读起来都不一样。
func sortedExcludedNamespaces(in map[string]ExcludedNamespace) []ExcludedNamespace {
	out := make([]ExcludedNamespace, 0, len(in))
	for _, ex := range in {
		out = append(out, ex)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Namespace < out[j].Namespace })
	return out
}
