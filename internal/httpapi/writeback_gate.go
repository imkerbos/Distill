package httpapi

import (
	"sort"
	"strings"

	networkingv1 "k8s.io/api/networking/v1"

	"github.com/imkerbos/Distill/internal/baseline"
	"github.com/imkerbos/Distill/internal/store"
)

// enforcingBlockers 报告这次写回为什么不该发生，一切齐备时返回空串。
//
// **推送就是进入 Enforcing。** 生成器给每条候选策略固定
// policyTypes: Ingress+Egress，规则为空即 default-deny（policygen/generate.go
// 的注释）—— 文件一旦被合并、被 Argo 应用，被选中的工作负载就只剩规则里
// 列出的那些放行。V4 spec §7.3 的 G3 校验项落在这里：五类 Baseline 在任何
// (cluster, namespace) 进入 Enforcing 前必须齐备
// （design doc 2026-08-18-enforcing-gate §1）。
//
// 两栏都挡，理由不同：
//
//   - MissingBaselines —— 依据看过了，推不出这一类。那一类的流量真的没有
//     被放行，下发之后它会被打断。
//   - NotAssessedBaselines —— 依据这次没采回来，无从判断。**没做过的检查
//     不是通过了的检查**：放它过去等于让一次采集失败变成一次放行，而失败
//     方向必须朝关（安全规范 §49），与 RepoVerifyResult 为 NOT_VERIFIED
//     时不写是同一条。
//
// 不适用的那几类不在任何一栏里 —— 无论是推导出来的（namespace 里没有
// 推导对象）还是人工声明的（NoNodeAgentsReason，带审计）。**改变对现实的
// 记录可以放行，跳过检查不行**，因此本函数没有任何绕过参数。
func enforcingBlockers(pv store.PolicyPreview) string {
	var parts []string

	if missing := missingInPushedNamespaces(pv); len(missing) > 0 {
		parts = append(parts, "这些命名空间的必备 Baseline 尚未齐备："+strings.Join(missing, "；"))
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
	return "写回会让这些策略在集群里生效，而每条策略都是 default-deny：" +
		strings.Join(parts, "。") +
		"。补齐之后重新出一次计划。若某一类在本集群确实不需要，" +
		"请在集群登记里写下理由 —— 平台不提供跳过这道检查的开关。"
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
