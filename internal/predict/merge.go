package predict

import (
	"sort"

	networkingv1 "k8s.io/api/networking/v1"
)

// WithExisting 给出"这批候选策略 apply 之后，集群里会有哪些策略"。
//
// **平台的下发方式是只加不删**（design doc 2026-08-25-existing-policies §3）：
// 写回把候选策略写进仓库，GitOps 把它们 apply 进集群，而集群里原有的策略
// 一条都不会因此消失。因此"合并这个 PR 之后实际会拦断什么"，算的必须是
// 已有 ∪ 候选，而不是候选单独跑。
//
// 只跑候选集回答的是另一个问题——"如果把旧策略也清理掉会怎样"，那是接管
// 路线的终点。两个数字都有用，但**默认要给的是前者**：操作者点下去发生的
// 是前者，而后者会把旧策略额外放行的那部分算成"会被拦断"，于是一份实际
// 无害的写回看起来会打断几十条连接。
//
// **同名对象按候选覆盖已有。** 第二次写回时，集群里已经有平台上一轮写下、
// 并且已被 GitOps 落地的 candidate-* 对象；把新旧两版都塞进策略集，回放会
// 把同一个对象算两遍。additive-allow 下结果碰巧不变，但那是巧合不是保证 ——
// apply 之后集群里只会有一份，而这个函数要描述的正是 apply 之后的状态。
//
// 入参一律不改：两份预测跑在同一批输入上，就地排序会让先跑的那一份改掉
// 后跑那一份的输入。
func WithExisting(
	existing, candidates []networkingv1.NetworkPolicy,
) []networkingv1.NetworkPolicy {
	type key struct{ namespace, name string }

	out := make([]networkingv1.NetworkPolicy, 0, len(existing)+len(candidates))
	idx := make(map[key]int, len(existing)+len(candidates))

	add := func(p networkingv1.NetworkPolicy) {
		k := key{p.Namespace, p.Name}
		if at, dup := idx[k]; dup {
			// 后来者覆盖：候选在已有之后加入，因此同名时留下的是即将被
			// apply 的那一版。
			out[at] = p
			return
		}
		idx[k] = len(out)
		out = append(out, p)
	}
	for _, p := range existing {
		add(p)
	}
	for _, p := range candidates {
		add(p)
	}

	// 确定排序：这份策略集会喂给回放，而同一批输入两次跑出不同结果会让
	// 两份预测之间的差额变得无法解释。
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out
}
