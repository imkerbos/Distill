import type { ExposureWidening } from '../api/types.ts'

/**
 * 对外暴露挂靠粒度的展示口径（design doc 2026-08-28-exposed-ingress §4）。
 *
 * Service 的 selector 可以点名单个 Pod（StatefulSet 的
 * `statefulset.kubernetes.io/pod-name`），而候选策略是 workload 粒度的——
 * 生成的规则会覆盖这个 workload 的全部 Pod。这是一次放宽，且它发生在
 * 平台自己推导出来的规则上，操作者没有第二个地方能发现它：候选策略那一栏
 * 只显示最终的 podSelector，读起来就是"按 Service 放行"。
 */

/** 一条暴露挂靠在界面上要显示的东西。 */
export interface ExposureWideningRow {
  key: string
  /** 形如 `devops/zk-0-lb`。 */
  label: string
  /** 规则实际挂靠的 workload。 */
  workload: string
  /** 形如 `1 / 3`：Service 点名了几个，规则实际覆盖几个。 */
  coverage: string
  extraPods: number
  /** 这一条放宽了没有，以及放宽了意味着什么。 */
  note: string
}

/**
 * exposureWideningRows 把每条暴露的挂靠粒度排成一张表。
 *
 * **无损的那些也留着**（extraPods === 0），不在这里过滤掉：spec §4 明写
 * "为 0 也要能答出来"——把无损的与真的放宽了的混在一起，操作者分不出哪几条
 * 值得回到 Pod 粒度去看；而只显示放宽了的那些，等于把"这一条我们算过、
 * 恰好无损"与"这一条没算过"合成同一个空白。
 */
export function exposureWideningRows(
  list: readonly ExposureWidening[] | null | undefined,
): ExposureWideningRow[] {
  return (list ?? []).map(w => ({
    key: `${w.namespace}/${w.service}/${w.workload}`,
    label: `${w.namespace}/${w.service}`,
    workload: w.workload,
    coverage: `${w.selectedPods} / ${w.workloadPods}`,
    extraPods: w.extraPods,
    note: w.extraPods === 0
      ? 'Service 点名的 Pod 与规则覆盖的 Pod 恰好相同，这次挂靠无损。'
      : `规则会多覆盖 ${w.extraPods} 个 Service 没有点名的 Pod —— `
        + '它们因此拿到同一条入站放行。要收窄，得回到 Pod 粒度看这个 Service '
        + '到底想暴露哪些实例。',
  }))
}

/**
 * exposureWideningNote 说明 `list` 为 `null` 时到底发生了什么。
 *
 * 契约要求这个字段恒为非 nil：真拿到 `null` 说明这是一份老响应，或字段改了
 * 名，不是"零条放宽"。**折进 `[]` 会把"服务端没回答"读成"一条都没放宽"** ——
 * 而这一栏描述的正是平台自己悄悄放宽了的授权范围。
 */
export function exposureWideningNote(
  list: readonly ExposureWidening[] | null | undefined,
): string {
  if (list == null) {
    return '服务端没有返回这项数据（exposureWidenings 缺席，按契约它恒为非 nil，'
      + '因此这是一份老响应或字段改了名）。在服务端答上来之前，不能把它读成'
      + '"没有一条对外暴露被放宽"。'
  }
  return ''
}

/**
 * 放宽区块的抬头。
 *
 * 说清这次放宽从哪来：不是折叠粒度带来的（那是 widening 那一栏），而是
 * Service selector 与 workload podSelector 之间本来就有的差。
 */
export const EXPOSURE_WIDENING_HELP =
  '候选策略按 workload 挂靠，而 Service 的 selector 可以点名更少的 Pod（'
  + 'StatefulSet 的 statefulset.kubernetes.io/pod-name 就是这个形态）。'
  + '这一栏逐条说明每个对外暴露的放行实际会覆盖到几个 Pod，'
  + '以及其中有几个是 Service 没有点名的。'

/** 一条都没有时显示的话。空区块读起来像"这一栏坏了"，而它是一条结论。 */
export const EXPOSURE_WIDENING_NONE = '没有任何对外暴露的放行挂到了 workload 上。'
