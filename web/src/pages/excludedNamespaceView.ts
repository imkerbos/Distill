import type { ExcludedNamespace, NamespaceExclusionReason } from '../api/types.ts'

/**
 * 整片未生成候选策略的命名空间（policygen.systemNamespaces）。
 *
 * **这是一道保护，不是一个缺口。** 界面上要读得出"平台是有意不碰这一片"，
 * 而不是"这里出了问题"——两者的处置完全相反：前者不需要动作，后者会让人去
 * 找哪里配错了。
 */

/** 每一类原因该说的话，以及它防的是什么。封闭枚举，逐条写死。 */
const REASON: Record<NamespaceExclusionReason, string> = {
  SYSTEM_NAMESPACE:
    'Kubernetes 内置系统命名空间，平台默认不生成候选策略。'
    + '候选集会给每个 workload 装上 default-deny，而观测证明不了完整时学出来的'
    + '规则默认不启用 —— 一份下发到 kube-dns 的 default-deny 会让整个集群失去 DNS。'
    + '要交给平台管，在集群登记里显式声明并写下理由。',
}

/** 一行展示。 */
export interface ExcludedNamespaceRow {
  namespace: string
  reason: string
}

/** excludedNamespaceRows 把排除清单排成一张表。 */
export function excludedNamespaceRows(
  in_: readonly ExcludedNamespace[] | null | undefined,
): ExcludedNamespaceRow[] {
  return (in_ ?? []).map(ns => ({
    namespace: ns.namespace,
    reason: REASON[ns.reason]
      ?? '平台没有认出这个排除原因分类。这一片没有生成候选策略，请联系平台维护者。',
  }))
}

/**
 * 区块抬头。
 *
 * 说清**这是有意的**，以及它防的具体后果 —— 一句"这些命名空间被排除了"
 * 会让人去找哪里配错了。
 */
export const EXCLUDED_NS_HELP =
  '这些命名空间平台「有意不碰」：候选集是给每个 workload 装上 default-deny 再把'
  + '观测到的连接放回去，而一份下发到 kube-dns 的 default-deny 会让整个集群失去 DNS。'
  + '它们的流量照常参与判定与对账 —— 不生成策略不等于看不见。'

/** 一个都没有时显示的话。 */
export const EXCLUDED_NS_NONE =
  '没有命名空间被整片排除。（这个集群里没有 Kubernetes 内置系统命名空间，'
  + '或者它们已经被显式交给平台管理。）'
