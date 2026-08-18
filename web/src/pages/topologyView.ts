import type { Topology } from '../api/types.ts'

/**
 * 没有观测到流量时，这一屏必须说出来的那句话。
 *
 * 单独成一个函数而不是写在 JSX 里：它是这份改动的安全绳（design doc
 * 2026-08-18 §2），要能被测试直接钉住。一句藏在渲染分支里的文案，
 * 删掉它不会有任何东西变红。
 */
export const NO_TRAFFIC_NOTICE =
  '这个集群还没有任何流量观测：下面的节点来自资产采集，而边一条都没有 —— ' +
  '这不表示它们之间没有通信，只表示我们还没看过。'

/** trafficNotice 在没有观测时给出提示，有观测时给 null。 */
export function trafficNotice(topo: Topology): string | null {
  return topo.trafficObserved ? null : NO_TRAFFIC_NOTICE
}

/**
 * edgeCountLabel 是「边」这一格该显示的东西。
 *
 * **没有观测时不显示 0。** 0 是一个数字，读者会把它当成一次计数的结果 ——
 * "这个集群有 0 条通信"。而事实是我们没有数过。
 */
export function edgeCountLabel(topo: Topology): string {
  return topo.trafficObserved ? String(topo.edges.length) : '尚未观测'
}

/**
 * showsGraph 判断这一屏该不该画那张图。
 *
 * 没有边的图是一堆互不相连的点：它看起来像一个"结构清晰、没有耦合"的集群，
 * 而那是最危险的误读。节点仍然要列出来（它们是真的），但不以图的形式。
 */
export function showsGraph(topo: Topology): boolean {
  return topo.trafficObserved && topo.edges.length > 0
}
