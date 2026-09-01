package httpapi

import (
	"sort"
	"strings"

	networkingv1 "k8s.io/api/networking/v1"

	"github.com/imkerbos/Distill/internal/baseline"
	"github.com/imkerbos/Distill/internal/policygen"
	"github.com/imkerbos/Distill/internal/store"
)

// enforcingBlockers 报告这次写回为什么不该发生，一切齐备时返回空串。
//
// **推送就是进入 Enforcing。** 生成器给每条候选策略固定
// policyTypes: Ingress+Egress，规则为空即 default-deny（policygen/generate.go
// 的注释）—— 文件一旦被合并、被 Argo 应用，被选中的工作负载就只剩规则里
// 列出的那些放行。V4 spec §7.3 的 G3 校验项落在这里：必备 Baseline 在任何
// (cluster, namespace) 进入 Enforcing 前必须齐备
// （design doc 2026-08-18-enforcing-gate §1）。
//
// 三栏都挡，理由不同：
//
//   - MissingBaselines —— 依据看过了，推不出这一类。那一类的流量真的没有
//     被放行，下发之后它会被打断。
//
//   - NotAssessedBaselines —— 依据这次没采回来，无从判断。**没做过的检查
//     不是通过了的检查**：放它过去等于让一次采集失败变成一次放行，而失败
//     方向必须朝关（安全规范 §49），与 RepoVerifyResult 为 NOT_VERIFIED
//     时不写是同一条。
//
//   - UnattachedBaselines 里 **NO_SUCH_WORKLOAD 的那些** —— 推出来了，
//     却挂不到任何 workload 上，而那是一次操作者改得动的标签错配。
//     **这一栏必须单独挡，因为上面第一栏是 kind 粒度的**：一个 namespace 里
//     有两个暴露型 Service，一个挂上了、一个挂不上，EXPOSED_INGRESS 就算
//     "齐备"，于是这个 namespace 通过门禁被推送 —— 而那个挂不上的 Service
//     背后的 workload 拿到的是 policyTypes:[Ingress] 加零条放行，集群的一个
//     真实入口在合并之后无声中断。那正是这一轮存在的理由（spec §6.2）。
//     注意这里的不自洽：**两个 Service 都挂不上**时 kind 缺失、门禁会挡；
//     只挂不上一个反而放行 —— 越严重的形态反而更容易过。
//
//     UnattachedImports 不在这一栏里：spec §6.2 明确把两者分开，导入是
//     操作者自己补的东西，而暴露描述的是集群**已经在对外发布**的事实，
//     更迫切。要不要一并挡是另一个决定，不在这次一起做掉。
//
// 不适用的那几类不在任何一栏里 —— 无论是推导出来的（namespace 里没有
// 推导对象）还是人工声明的（NoNodeAgentsReason，带审计）。**改变对现实的
// 记录可以放行，跳过检查不行**，因此本函数没有任何绕过参数。
func enforcingBlockers(pv store.PolicyPreview) string {
	var parts []string

	if missing := missingInPushedNamespaces(pv); len(missing) > 0 {
		parts = append(parts, "这些命名空间的必备 Baseline 尚未齐备："+strings.Join(missing, "；")+
			"。若某一类在本集群确实不需要，请在集群登记里写下理由")
	}
	// 挂不上的暴露单独说，**处置也单独说**：这一栏补不了登记，要去看这个
	// Service 的 selector 与它真正想暴露的 workload 之间标签对不对得上。
	if unattached := unattachedInPushedNamespaces(pv); len(unattached) > 0 {
		parts = append(parts,
			"这些对外暴露的 Service 推出了放行范围，却挂不到任何 workload 上，"+
				"下发之后它们的入口会被 default-deny 断掉："+strings.Join(unattached, "；")+
				"。去把 Service 的 spec.selector 与那个 workload 的标签对齐"+
				"（集群登记里没有能豁免这一条的地方）")
	}
	// 未评估单独说，不与缺失混成一句：处置不同 —— 缺失去补登记，
	// 未评估去查这次采集为什么没拿回依据（多半是 RBAC 或超时）。
	if len(pv.NotAssessedBaselines) > 0 {
		parts = append(parts,
			"这几类的依据本次采集没有拿回来，因而无从判断缺不缺："+
				joinKinds(pv.NotAssessedBaselines)+
				"。没做过的检查不算通过")
	}
	if len(parts) == 0 {
		return ""
	}
	// 处置跟着各自那一栏走，不放在这里统一说一句。此前这里写的是"若某一类
	// 在本集群确实不需要，请在集群登记里写下理由"，而那条出路只对 kind 粒度
	// 的缺失成立（NoNodeAgentsReason）—— 一个被挂不上的 Service 挡住的操作者
	// 照着做，会在集群登记里找一个不存在的字段。
	return "写回会让这些策略在集群里生效，而每条策略都是 default-deny：" +
		strings.Join(parts, "。") +
		"。处理完重新出一次计划 —— 平台不提供跳过这道检查的开关。"
}

// missingInPushedNamespaces 挑出**这次真的会被写进文件**的那些 namespace
// 里的缺失，按 namespace 排序。
//
// 判的不是整个集群：没有策略落进去的 namespace 不会获得 default-deny，
// 也就不会被打断。把它的缺失算进来等于让一个与本次推送无关的缺口永久挡住
// 所有推送，而一道永远在挡的门会被整体绕开（design doc §2）。
func missingInPushedNamespaces(pv store.PolicyPreview) []string {
	pushed := pushedNamespaces(pv.Overridden.Enabled)
	var out []string
	for _, m := range pv.MissingBaselines {
		if !pushed[m.Namespace] || len(m.Kinds) == 0 {
			continue
		}
		out = append(out, m.Namespace+" 缺 "+joinKinds(m.Kinds))
	}
	sort.Strings(out)
	return out
}

// unattachedInPushedNamespaces 挑出**这次真的会被写进文件**的那些 namespace
// 里、**操作者修得好**的那些挂不上的暴露，按 Service 排序。
//
// 按推送范围裁剪，与 missingInPushedNamespaces 同一条理由：没有策略落进去的
// namespace 不会获得 default-deny，也就不会被打断；把它算进来等于让一个与
// 本次推送无关的缺口永久挡住所有推送，而一道永远在挡的门会被整体绕开。
//
// **两种成因只挡得住一种**（spec §6.2 的封闭枚举）：
//
//   - NO_SUCH_WORKLOAD 挡。Service selector 与 workload 的赢家标签键对不上
//     （Helm 同时打 app 与 app.kubernetes.io/name 是最常见的触发方式），
//     这是集群里一处真实的标签错配，改 Service 或改 Pod 标签就能解除，
//     解除之后这条暴露会正常挂上 —— 门禁挡的是一件挡得有意义的事。
//
//   - NO_SELECTOR **不挡**。没有 spec.selector 的 LoadBalancer / NodePort
//     是手工维护 Endpoints 的外部后端，spec §6.2 与 derive_exposed.go 都写着
//     它"合法且常见"：**没有 workload 可挂，也没有任何一处改动能让它挂上**。
//     挡住它等于给这个 namespace 装一把没有钥匙的锁 —— 唯一的解法是把这个
//     namespace 里的策略全部禁用直到它掉出推送范围，而那正是本函数上一段
//     注释所说、按推送范围裁剪要防的那件事：一道永远在挡的门会被整体绕开，
//     连同它本该挡住的 NO_SUCH_WORKLOAD 一起（design review RI1，2026-08-28）。
//
// 不挡不等于不说：这一条照旧完整地留在 pv.UnattachedBaselines 里，经预览
// 接口进"没有挂上的对外暴露"那一节（unattachedBaselineView.ts 为两种成因
// 各写了一句处置）。spec §6.2 要的是这次暴露不能悄无声息，不是它必须拦住
// 写回 —— 而一条既拦不住又劝不动的拒绝，两样都做不到。
func unattachedInPushedNamespaces(pv store.PolicyPreview) []string {
	pushed := pushedNamespaces(pv.Overridden.Enabled)
	var out []string
	for _, u := range pv.UnattachedBaselines {
		if !pushed[u.Namespace] || !blocksWriteback(u.Reason) {
			continue
		}
		out = append(out, u.Namespace+"/"+u.Name+"（"+string(u.Kind)+"，"+
			string(u.Reason)+"）")
	}
	sort.Strings(out)
	return out
}

// blocksWriteback 判断一种"挂不上"的成因该不该挡住写回。
//
// 用显式 switch 而非"不等于 NO_SELECTOR"：UnattachedBaselineReason 是封闭
// 枚举，新增一个取值时 default 会把它按"不挡"放过去，而放过去的方向是
// 静默的。写成 switch，新增取值的人至少要走到这一行前面来。
func blocksWriteback(r policygen.UnattachedBaselineReason) bool {
	switch r {
	case policygen.UnattachedBaselineNoSuchWorkload:
		return true
	case policygen.UnattachedBaselineNoSelector:
		return false
	default:
		// 认不出的成因按挡处理：一个平台不认识的原因，最不该做的事是
		// 假定它无害（CLAUDE.md §3，判不出就朝关的那一侧失败）。
		return true
	}
}

// pushedNamespaces 是这份文件里出现的全部 namespace。
func pushedNamespaces(enabled []networkingv1.NetworkPolicy) map[string]bool {
	out := make(map[string]bool, len(enabled))
	for _, p := range enabled {
		out[p.Namespace] = true
	}
	return out
}

// joinKinds 把一组类型拼成人读的一句，保持登记顺序。
func joinKinds(kinds []baseline.Kind) string {
	names := make([]string, 0, len(kinds))
	for _, k := range kinds {
		names = append(names, string(k))
	}
	return strings.Join(names, "、")
}
