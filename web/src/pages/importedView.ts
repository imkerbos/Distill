import type { CandidateRule, UnattachedImport, UnattachedReason } from '../api/types.ts'

/**
 * 人工导入规则的展示口径（design doc 2026-08-25-existing-policies §3）。
 *
 * 导入这条路存在的理由是**观测看不见的东西**：月结批处理、季度对账、只在
 * 故障时走的灾备链路 —— 不在窗口里就学不出规则，而 dry-run 也报不出来
 * （它只评估见过的连接）。
 */

/**
 * IMPORTED_BASIS 是导入规则在"依据"那一栏该说的话。
 *
 * **必须解释 flowCount 为什么是 0。** 那个 0 不是"没有流量"—— 导入这条路
 * 存在的理由正是那条连接不在观测里。与学习规则混成一栏读，一条人工补上的
 * 月结批处理规则会显示成"没人用、可以收紧"。
 */
export const IMPORTED_BASIS =
  '人工导入：这条规则不是从流量学来的，因此流量条数恒为 0 —— '
  + '那不是"没有流量"，是它补的那条连接本来就不在观测窗口里'
  + '（月结批处理、季度对账、只在故障时走的灾备链路）。'

/** 每一类挂不上的原因该说的话，以及操作者该怎么办。封闭枚举，逐条写死。 */
const UNATTACHED_REASON: Record<UnattachedReason, string> = {
  NO_WORKLOAD_LABEL:
    'podSelector 里没有 workload 归属标签（app / app.kubernetes.io/name / k8s-app / component）。'
    + '候选集按 workload 组织，一条选不出主体的策略挂不上去 —— '
    + '空 podSelector（选中整个 namespace）与只用 matchExpressions 都会落在这里。'
    + '改成按标签选中某一个 workload 再导入。',
  NO_RULES:
    '这条策略一条 ingress/egress 规则都没有。空规则在 NetworkPolicy 语义里是 '
    + 'default-deny，那是**收紧**，而导入这条路只接受补充放行。要收紧走策略生成本身。',
  NO_SUCH_WORKLOAD:
    '这个集群里没有该主体的 Pod（还没部署，或者缩到了零）。'
    + '挂上去会生成一条选不中任何 Pod 的策略：它不报错，只是永远不生效。'
    + '等那个 workload 起来之后它会自动进候选集，不必重新导入。',
}

/** 一条挂不上的导入在界面上要显示的东西。 */
export interface UnattachedRow {
  importId: string
  /** 形如 `payment/monthly-settlement`。 */
  label: string
  reason: string
}

/**
 * unattachedRows 把挂不上的导入排成一张表。
 *
 * **每一行都带一句该怎么办**：一条只写着 `NO_WORKLOAD_LABEL` 的记录，读的人
 * 拿着它无法行动，而这一栏的全部意义就是让他知道这条补充没生效、以及怎么补。
 */
export function unattachedRows(
  imports: readonly UnattachedImport[] | null | undefined,
): UnattachedRow[] {
  return (imports ?? []).map(i => ({
    importId: i.importId,
    label: `${i.namespace}/${i.name}`,
    reason: UNATTACHED_REASON[i.reason]
      ?? '平台没有认出这个原因分类。这条导入没有进候选集，请联系平台维护者。',
  }))
}

/**
 * 挂不上区块的抬头。
 *
 * 说清后果而不只是说"有几条没挂上"：没进候选集就等于没补上，而操作者导入
 * 它正是因为 dry-run 看不见那条连接。
 */
export const UNATTACHED_HELP =
  '这些导入没有进候选集，也就不会被写回 —— 它们补的那条放行现在是缺的。'
  + 'dry-run 看不出这个缺口：它只评估见过的连接，而这些规则要补的恰恰是没见过的那些。'

/** 一条都没有时显示的话。空区块读起来像"这一栏坏了"，而它是一条结论。 */
export const UNATTACHED_NONE = '全部人工导入都已挂到对应的 workload 上。'

/** isImported 报告这条规则是否来自人工导入。 */
export function isImported(rule: Pick<CandidateRule, 'origin'>): boolean {
  return rule.origin === 'IMPORTED'
}
