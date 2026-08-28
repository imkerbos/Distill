import type { UnattachedBaselineReason, UnattachedBaselineRule } from '../api/types.ts'

/**
 * 挂不上主体的 Baseline 规则的展示口径（design doc 2026-08-28-exposed-ingress §6.2）。
 *
 * 与人工导入挂不上主体同一条纪律（importedView.ts），但理由更迫切：这里描述的
 * 是集群**已经真实存在**的对外暴露，不是操作者自己补的东西——放行范围已经算出
 * 来了（peers 已经算好），只是不知道该挂给哪个 workload。不广播（那会把这条放行
 * 发给这个 namespace 里完全不相干的主体），也不能静默丢弃：missingBaselines 是
 * kind 粒度的，只要同一个 namespace 里有另一个 Service 正常挂上了，这个 kind 就
 * 不再"缺失"，而这一个真实存在的暴露依然什么放行都没有、也没有任何信号。
 */

/** 每一类挂不上主体的原因该说的话，以及操作者该怎么办。封闭枚举，逐条写死。 */
const UNATTACHED_BASELINE_REASON: Record<UnattachedBaselineReason, string> = {
  NO_SELECTOR:
    '这个 Service 没有 spec.selector —— 手工维护 Endpoints 的外部后端就是这个'
    + '形态，合法且常见。这里根本没有 workload 可挂，要看的是这个 Service 到底'
    + '该不该在这里生成候选策略（多半是不该，它没有 Pod 主体）。',
  NO_SUCH_WORKLOAD:
    'Service selector 解出的 workload 取值，在候选花名册里找不到对应主体 —— '
    + '常见触发方式是 Helm 同时打 app 与 app.kubernetes.io/name 两个不同取值。'
    + '要看的是这个 Service 的 selector 和它真正想暴露的 workload，标签对不对得上。',
}

/** 一条挂不上主体的 Baseline 规则在界面上要显示的东西。 */
export interface UnattachedBaselineRow {
  key: string
  /** 形如 `istio-system/uat-istio-ingressgateway-extra`。 */
  label: string
  kind: string
  reason: string
}

/**
 * unattachedBaselineRows 把挂不上主体的 Baseline 规则排成一张表。
 *
 * **每一行都带一句该怎么办**，理由同 unattachedRows：一条只写着
 * `NO_SUCH_WORKLOAD` 的记录，读的人拿着它无法行动。
 */
export function unattachedBaselineRows(
  rules: readonly UnattachedBaselineRule[] | null | undefined,
): UnattachedBaselineRow[] {
  return (rules ?? []).map(r => ({
    key: `${r.kind}/${r.namespace}/${r.name}`,
    label: `${r.namespace}/${r.name}`,
    kind: r.kind,
    reason: UNATTACHED_BASELINE_REASON[r.reason]
      ?? '平台没有认出这个原因分类。这条暴露没有进候选集，请联系平台维护者。',
  }))
}

/**
 * unattachedBaselineNote 说明 `items` 为 `null` 时到底发生了什么。
 *
 * 契约要求这个字段恒为非 nil（design doc §6.2）：真拿到 `null` 说明这是一份
 * 老响应，或字段改了名，不是"零条挂不上"。**折进 `[]` 会把"服务端没回答"
 * 读成"都挂上了"**——而这一栏描述的正是集群已经真实存在、却可能悄悄失效
 * 的对外暴露，这个方向的错读后果最重，因此不像 unattachedImports 那样
 * 用 `?? []` 收口。
 */
export function unattachedBaselineNote(
  items: readonly UnattachedBaselineRule[] | null | undefined,
): string {
  if (items == null) {
    return '服务端没有返回这项数据（unattachedBaselines 缺席，按契约它恒为非 nil，'
      + '因此这是一份老响应或字段改了名）。在服务端答上来之前，不能把它读成'
      + '"全部对外暴露都挂上了"。'
  }
  return ''
}

/**
 * 挂不上区块的抬头。
 *
 * 说清后果而不只是说"有几条没挂上"：没进候选集意味着这个入口现在什么
 * 放行都没有，而 dry-run 看不出这个缺口——它只评估见过的连接，这里
 * 描述的恰恰是集群里已经真实存在、却还没有任何观测能替它说话的暴露。
 */
export const UNATTACHED_BASELINE_HELP =
  '这些暴露已经算出了放行范围，却挂不上任何 workload，因而没有进候选集 —— '
  + '这个入口现在什么放行都没有。dry-run 看不出这个缺口：它只评估见过的连接，'
  + '而这里描述的是集群已经真实存在的对外暴露，不是操作者自己补的东西。'

/** 一条都没有时显示的话。空区块读起来像"这一栏坏了"，而它是一条结论。 */
export const UNATTACHED_BASELINE_NONE = '全部对外暴露都已挂到对应的 workload 上。'
