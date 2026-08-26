package replay

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ForeignScope 是一条**平台不解释的策略**所覆盖的主体范围
// （design doc 2026-08-25 §2）。
//
// 本包不知道 CiliumNetworkPolicy 或 AdminNetworkPolicy 长什么样，也不该知道：
// 它们的语义是第二套引擎的事（实体选择器、L7 规则、优先级与 Pass 动作）。
// 这里只需要一件事实 —— **哪些主体被那样的策略选中**。selector 匹配是可以
// 直接算出来的事实，不是猜测；而"那条策略具体放行了什么"仍然算不出来，
// 因此落在范围内的判定照旧降级。
//
// 这样降级就从"整个集群"收窄到"真的被覆盖的那些主体"。在这之前，集群里只要
// 存在一条 CNP，每一条判定都会被标成 DEGRADED —— 而降级面越大，操作者越会
// 习惯性忽略它，那个标记的全部意义恰恰是让他在真该停手的地方停手。
type ForeignScope struct {
	// Namespace 为空表示集群级（CiliumClusterwideNetworkPolicy 那一类），
	// 跨全部 namespace 生效。
	Namespace string
	// Selector 是主体选择器。
	//
	// **空 selector 选中该范围内全部主体**，与 endpointSelector: {} 和
	// NetworkPolicySpec.PodSelector 的语义一致 —— 它是值类型，"空"是
	// "全选"，不是"未设置"。
	Selector metav1.LabelSelector
}

// WithForeignPlaneScopes 声明这个集群里、平台不解释的策略覆盖了哪些主体。
//
// 与 WithForeignPlane 并存而不是取代它：那一个说的是"这个集群有没有第二
// 平面平台没查过/查不动"，那时任何精确到主体的说法都是编出来的，只能整片
// 降级。这一个说的是"查过了，覆盖范围是这些"。
//
// **列表为空不等于没有第二平面**：调用方必须先用三态确认过是 NONE 才可以
// 不传 scope，否则要走 WithForeignPlane(true)。本函数不替调用方做这个判断 ——
// 一个把"没查到"读成"没有"的默认值，正是这条链路上最危险的那种错误。
func WithForeignPlaneScopes(scopes []ForeignScope) Option {
	return func(e *Evaluator) { e.foreignScopes = scopes }
}

// coveredByForeignPolicy 报告这条连接的任一端是否落在第二平面的覆盖范围内。
//
// **身份解不出的端点一律算作覆盖**（在存在 scope 的前提下）：解不出就不知道
// 它有没有被那些策略选中，而"不知道有没有东西在覆盖我的结论"承担的正是这个
// 标记要表达的风险。
//
// 两端都查：判定是双向的，对端被一个平台不解释的策略管着，这条连接的结论
// 一样不可信。
func (e *Evaluator) coveredByForeignPolicy(f Flow) bool {
	if len(e.foreignScopes) == 0 {
		return false
	}
	for _, endpoint := range []Endpoint{f.Source, f.Dest} {
		// 集群外的地址不受本集群策略管辖，跳过 —— 它没有身份不是"解不出"，
		// 是本来就不该有。
		if endpoint.ClusterID != "" && endpoint.ClusterID != e.clusterID {
			continue
		}
		if endpoint.Pod == nil {
			// 集群内、却解不出身份：不知道有没有被覆盖，按覆盖处理。
			// 外部地址（ClusterID 为空且不属于本集群）在上一支已经被排除。
			if endpoint.ClusterID == e.clusterID {
				return true
			}
			continue
		}
		if e.scopesCover(*endpoint.Pod) {
			return true
		}
	}
	return false
}

// scopesCover 报告某个 Pod 是否落在任一 scope 内。
func (e *Evaluator) scopesCover(pod PodRef) bool {
	for _, sc := range e.foreignScopes {
		if sc.Namespace != "" && sc.Namespace != pod.Namespace {
			continue
		}
		// 取地址后传给 selectorMatches：那个函数对 nil 返回 false（用于区分
		// "未设置"），而这里的 Selector 是值类型，空即全选 —— 传地址正好落到
		// LabelSelectorAsSelector 的空选择器语义上。
		sel := sc.Selector
		ok, err := selectorMatches(&sel, pod.Labels)
		if err != nil {
			// 选择器本身写坏了：算不出它选中谁。**按覆盖处理**，与解不出
			// 身份同一方向 —— 判不出来就不放行。
			return true
		}
		if ok {
			return true
		}
	}
	return false
}
