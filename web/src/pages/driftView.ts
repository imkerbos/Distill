import type { ClusterDriftResult, DriftResult } from '../api/types.ts'

/** 全部已登记的漂移结论，与后端枚举逐值对齐。 */
export const ALL_DRIFT_RESULTS: DriftResult[] = [
  'IN_SYNC', 'DRIFTED', 'NEVER_WRITTEN', 'ANCHOR_MISSING', 'UNKNOWN',
]

/** driftView 是一个结论在界面上的说法。 */
export interface DriftView {
  label: string
  /** action 说的是操作者该做什么 —— 五种结论的处置各不相同。 */
  action: string
  /** tone 决定呈现的轻重；未登记的取值与 UNKNOWN 同档。 */
  tone: 'ok' | 'warn' | 'danger' | 'muted'
}

const VIEWS: Record<DriftResult, DriftView> = {
  IN_SYNC: {
    label: '与上次下发一致',
    action: '不用做什么。',
    tone: 'ok',
  },
  DRIFTED: {
    label: '内容已被改动',
    action: '去看是谁改的，再决定要不要重推一次。',
    tone: 'warn',
  },
  NEVER_WRITTEN: {
    label: '从未下发过',
    action: '先推一次，之后才有可比对的基准。',
    tone: 'muted',
  },
  ANCHOR_MISSING: {
    // 与 DRIFTED 分开：那条历史里我们那次提交连同它的审计线索一起没了。
    label: '基准提交已不在仓库里（历史被改写）',
    action: '查是谁 force push 了这个分支 —— 我们那次提交与它的审计线索一起没了。',
    tone: 'danger',
  },
  UNKNOWN: {
    // **绝不能读成一致。** 一次网络抖动读成"一致"，操作者就以为下发的
    // 东西还在，而它可能早被人删了（design doc §3）。
    label: '没能查到（够不到仓库或凭据不可用）',
    action: '这不是"一致"——平台没看到仓库。先修连通性或凭据，再看一次。',
    tone: 'muted',
  },
}

/**
 * driftView 返回该结论的说法；**未登记的取值按 UNKNOWN 处理**。
 *
 * 与 dataSourceView 同一条纪律：一个读不懂的取值不能当成最让人安心的那个。
 */
export function driftView(result: DriftResult | undefined): DriftView {
  return (result && VIEWS[result]) || VIEWS.UNKNOWN
}

/** 全部已登记的集群漂移结论，与后端枚举逐值对齐。 */
export const ALL_CLUSTER_DRIFT_RESULTS: ClusterDriftResult[] = [
  'CONVERGED', 'PENDING', 'CLUSTER_AHEAD', 'UNKNOWN',
]

/**
 * 集群漂移与仓库漂移是**两个问题**，因此两套说法（design doc 2026-08-25 §5）：
 *
 *   driftView        仓库 vs 平台最后写过的 —— 「仓库被别人改过吗」
 *   clusterDriftView 集群 vs 仓库 —— 「GitOps 到底有没有把它落下去」
 *
 * 一个仓库没被动过、但 controller 三天没同步的集群，前者是 IN_SYNC、
 * 后者是 PENDING。只显示前者，那三天没有人会知道。
 */
const CLUSTER_VIEWS: Record<ClusterDriftResult, DriftView> = {
  CONVERGED: {
    label: '仓库里的都已生效',
    action: '不用做什么。',
    tone: 'ok',
  },
  PENDING: {
    label: '仓库里有、集群里还没有',
    action: 'GitOps controller 可能没同步或同步失败了 —— 去看它的状态，不要重推。',
    tone: 'warn',
  },
  CLUSTER_AHEAD: {
    label: '集群里有平台的对象、仓库里却没有',
    action: '有人手工 apply 过，或者仓库被回退了而集群没跟上。以仓库为准，查是谁动的。',
    tone: 'danger',
  },
  UNKNOWN: {
    // 同 driftView 的 UNKNOWN：**绝不能读成已生效**。一个从没被采过的集群
    // 与一个真的收敛了的集群在数据里长得一样，而后者会让人以为下发已经生效。
    label: '没能判断（仓库或采集数据够不到）',
    action: '这不是"已生效"——平台没看全。先修连通性或先采一轮，再看一次。',
    tone: 'muted',
  },
}

/** clusterDriftView 返回该结论的说法；**未登记的取值按 UNKNOWN 处理**。 */
export function clusterDriftView(result: ClusterDriftResult | undefined): DriftView {
  return (result && CLUSTER_VIEWS[result]) || CLUSTER_VIEWS.UNKNOWN
}
