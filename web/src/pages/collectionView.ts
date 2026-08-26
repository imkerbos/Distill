import type { CollectionResource, CollectionSummary, CollectionWarning } from '../api/types'

/**
 * 这块可见面与平台其余部分的关系，说给操作者听的那一句。
 *
 * **不是免责声明，是这一屏的前提。** 采到的真实资产只落库、只在这里显示，
 * 不接进任何结论链路（spec §5.2）：拓扑、流量判定、dry-run、候选策略、
 * 导出与写回全部仍然跑在合成数据集上。
 *
 * 必须说出来的理由很具体：这一屏上是真实集群里真实存在的 Pod 与策略数量，
 * 而隔壁几屏是合成的。不说，一个看过这一屏的操作者会理所当然地认为
 * dry-run 的"这条连接会被拦断"讲的是他刚看过的那些真实工作负载 ——
 * 而那个结论建立在不存在的流量上。一个错误结论伪装成体检报告。
 *
 * 常量而非写死在 JSX 里：它要能被断言，也要在只有一处可改。
 */
export const COLLECTION_FEEDS_NOTHING =
  '这一屏的数字不参与平台的任何结论。它们来自对真实集群的一次只读采集，'
  + '只落库、只在这里显示。网络拓扑、流量判定、dry-run 预演、候选策略与导出'
  + '全部仍然跑在合成数据集上，与这里的任何一个数字无关。'
  + '把真实资产与合成流量拼在一起，会让 dry-run 对着真实工作负载给出'
  + '基于不存在的流量的阻断结论 —— 那是一个错误结论伪装成体检报告。'

/**
 * 这个集群已注册、但从未被采集过时，后端回的业务码。
 *
 * 它必须与三件事分开，缺一件都会让这一屏说错话：
 *  - "采集过、但某一类资源没采到" —— 那是摘要里的一条失败，不是没有摘要。
 *  - "这个集群根本不存在"（20002）—— 后端读取端查的是 collection_run，
 *    对拼错的 ID 同样查不到行，两者共用一个码时一次拼写错误会显示成
 *    "还没有采集记录"，于是操作者去查采集器为什么没跑。
 *  - "本部署没有采集读取端"（50002）—— 那是装配缺失。
 */
export const COLLECTION_NO_RUN_CODE = 20004

/** 集群本身不存在时，后端回的业务码。 */
export const COLLECTION_UNKNOWN_CLUSTER_CODE = 20002

/** isNoRunError 判断一次失败是不是"这个集群还没有过采集"。 */
export function isNoRunError(e: unknown): boolean {
  return errorCodeIs(e, COLLECTION_NO_RUN_CODE)
}

/** isUnknownClusterError 判断一次失败是不是"这个集群不存在"。 */
export function isUnknownClusterError(e: unknown): boolean {
  return errorCodeIs(e, COLLECTION_UNKNOWN_CLUSTER_CODE)
}

/** errorCodeIs 比对一次失败携带的业务码。 */
function errorCodeIs(e: unknown, code: number): boolean {
  return typeof e === 'object' && e !== null && 'code' in e
    && (e as { code: unknown }).code === code
}

/**
 * 一次采集运行的结果标签。与后端 snapshot.RunStatus 封闭枚举对齐。
 *
 * PARTIAL 的措辞刻意不含糊：一次部分成功的采集若被读成成功，下游会把一份
 * 缺了策略的快照当作完整事实使用（spec §4.2）。
 */
export const RUN_STATUS_LABEL: Record<string, string> = {
  OK: '全部资源都采到了',
  PARTIAL: '部分资源没采到',
  FAILED: '采集失败',
}

/** 资源类型标签。与后端 snapshot.ResourceKind 对齐。 */
export const RESOURCE_LABEL: Record<string, string> = {
  NAMESPACE: 'Namespace',
  POD: 'Pod',
  NODE: 'Node',
  REPLICASET: 'ReplicaSet',
  SERVICE: 'Service',
  ENDPOINTSLICE: 'EndpointSlice',
  NETWORKPOLICY: 'NetworkPolicy',
  INGRESS: 'Ingress',
  // ANP 与 BANP 共用一行：它们由同一次采集动作产出，一次失败必然同时
  // 影响两者。分成两行会在界面上说同一件事两遍。
  ADMINNETWORKPOLICY: 'AdminNetworkPolicy / BaselineAdminNetworkPolicy',
}

/**
 * 失败原因标签。与后端 snapshot.FailureReason 封闭枚举对齐。
 *
 * UNRECOGNIZED 不在后端那份枚举里，它是 API 边界上的产物：库里的取值
 * 不在封闭枚举内时交出来的东西（internal/httpapi/collection_handler.go）。
 * 真实取值只在服务端日志里 —— 因为那一列的邻座是 apiserver 的原始报错，
 * 里面有内部主机名。
 */
export const FAILURE_REASON_LABEL: Record<string, string> = {
  FORBIDDEN: '没有读取权限',
  NOT_FOUND: '集群上没有这个资源类型',
  TIMEOUT: '请求超时',
  UNAVAILABLE: 'apiserver 不可用',
  OTHER: '其它原因',
  UNRECOGNIZED: '原因不在封闭枚举内',
}

/**
 * 一次运行「根本没能开始」的原因标签。与后端 snapshot.RunErrorReason 对齐。
 *
 * 与 FAILURE_REASON_LABEL 分成两张表，不是重复：那张说的是某一类资源，
 * 这张说的是整一轮。合成一张会让「NetworkPolicy 被拒」与「采集器连不上
 * 这个集群」在界面上落进同一句话，而两者的下一步动作完全不同。
 */
export const RUN_ERROR_REASON_LABEL: Record<string, string> = {
  CREDENTIAL_UNAVAILABLE: '拿不到这个集群的凭据',
  CLIENT_UNAVAILABLE: '凭据拿到了，但连不上这个集群的 apiserver',
  READ_ONLY_UNPROVEN: '没能证明采集器对这个集群只读，因此一个资源都没有读',
  UNRECOGNIZED: '原因不在封闭枚举内',
}

/** 每种「没能开始」的原因对应的处置。 */
export const RUN_ERROR_REASON_ACTION: Record<string, string> = {
  CREDENTIAL_UNAVAILABLE: '检查这个集群登记的 kubeconfig 引用，以及后台设置里的凭据后端。',
  CLIENT_UNAVAILABLE: '检查 apiserver 地址是否可达，以及它是否被出站地址守卫拒绝。',
  READ_ONLY_UNPROVEN: '检查采集器凭据的 RBAC：它不得持有任何网络策略的写权限，也必须能做 SelfSubjectAccessReview。',
  UNRECOGNIZED: '这条记录写进库时用了一个平台不认识的原因，请查服务端日志。',
}

/**
 * runErrorNote 把「这一轮没能开始」讲成一句话，正常的一轮返回空串。
 *
 * 返回空串而不是一句"一切正常"：这条提示只在有事发生时出现，
 * 每一轮都顶着一句话会让真正出事的那一轮看起来和平常一样。
 */
export function runErrorNote(errorReason?: string): string {
  if (!errorReason) return ''
  const label = RUN_ERROR_REASON_LABEL[errorReason] ?? errorReason
  const action = RUN_ERROR_REASON_ACTION[errorReason] ?? ''
  return `这一轮没有开始：${label}。这不是「采到了零个资源」—— 平台根本没有看过这个集群。${action}`
}

/**
 * 每种失败原因对应的处置。
 *
 * 与标签分开，是因为 FORBIDDEN 与其余几种的下一步动作方向相反：它是唯一
 * 一种"集群是好的、是我们没被授权"的失败，处置是改 RBAC 而不是重试，
 * 而重试恰恰是它最常被误配的处置方式（migrations/000009 上的同一段注释）。
 */
export const FAILURE_REASON_ACTION: Record<string, string> = {
  FORBIDDEN: '重试没有用，需要给采集器的 ServiceAccount 补上这一类资源的 list 权限',
  NOT_FOUND: '确认这个集群的版本是否提供该资源类型',
  TIMEOUT: '查采集器到 apiserver 的网络与 apiserver 负载',
  UNAVAILABLE: '查 apiserver 是否可达',
  OTHER: '详细原因只写在服务端日志里，按 request_id 查',
  UNRECOGNIZED: '库里的取值不在封闭枚举内，按 request_id 查服务端日志',
}

/** 采集告警标签。与后端 snapshot.WarningKind 对齐。 */
export const WARNING_KIND_LABEL: Record<string, string> = {
  POD_IP_OUTSIDE_CLUSTER: 'Pod IP 落在登记的 Pod 网段之外',
  POD_IP_AMBIGUOUS: 'Pod IP 同时命中多个集群的网段',
  POD_IP_UNCLASSIFIABLE: 'Pod IP 无法归类（缺少网段登记）',
  POD_IP_UNPARSABLE: 'Pod IP 不是合法地址',
  SERVICE_WITHOUT_ENDPOINTS: 'Service 存在但没有后端',
  WORKLOAD_UNRESOLVED: 'Pod 的 ownerRef 链没能解到顶层控制器',
}

/**
 * 一类资源在表格里的一行。
 *
 * `observed` 为 false 时 `countText` **恒为空串**，不是 `'0'`。这是
 * spec §4.2 那条约束在渲染层的落点：后端在失败时根本不发 count 键，
 * 而这一层保证即使有人给它补一个默认值，那个数字也进不了表格。
 *
 * 三个文案字段而不是把原始值丢给 JSX 自己拼：拼的地方多一处，就多一处
 * 可以把 `?? 0` 写进去的位置。
 */
export interface CollectionRow {
  readonly resource: string
  readonly label: string
  /** 这一类到底采到没有。false 表示这一行讲的是一次失败。 */
  readonly observed: boolean
  /** 采到时是条数，没采到时是空串 —— 失败的资源没有数字可言。 */
  readonly countText: string
  /** 没采到时的原因枚举值；采到时为空串。 */
  readonly failureReason: string
  /** 没采到时的原因标签；采到时为空串。 */
  readonly failureLabel: string
  /** 没采到时的处置；采到时为空串。 */
  readonly failureAction: string
}

/**
 * collectionRows 把报文里的资源结果转成表格行。
 *
 * 判别式是 `failureReason` 这个键在不在，而不是某个数字等于 0：后端的
 * 两种形态在报文里是两组不相交的键（count 或 failureReason，从不同时
 * 出现）。用 `count === 0` 判别会把一个真实的 0 说成失败，用
 * `count ?? 0` 取值会把一次失败说成 0 —— 两个方向都错，而后者错在
 * "结果比现实好看"的那一侧。
 *
 * **不按枚举补齐缺席的资源类型。** REPLICASET 在后端枚举里但从不计数
 * （它只用于把 Pod 解到顶层控制器，不是被观测的资产，spec §4.2）。
 * 补齐会给它造出一行凭空的失败，把操作者引去查一个不存在的 RBAC 问题。
 */
export function collectionRows(resources: readonly CollectionResource[]): CollectionRow[] {
  return resources.map((r) => {
    const label = RESOURCE_LABEL[r.resource] ?? r.resource
    if (r.failureReason !== undefined) {
      return {
        resource: r.resource,
        label,
        observed: false,
        countText: '',
        failureReason: r.failureReason,
        // 未收录的取值照原样显示，不丢弃、不留空：少显示一种成因等于
        // 把一类系统性问题藏起来（同 UNKNOWN_REASON_LABEL 的处置）。
        failureLabel: FAILURE_REASON_LABEL[r.failureReason] ?? r.failureReason,
        failureAction: FAILURE_REASON_ACTION[r.failureReason] ?? '',
      }
    }
    return {
      resource: r.resource,
      label,
      observed: true,
      countText: String(r.count),
      failureReason: '',
      failureLabel: '',
      failureAction: '',
    }
  })
}

/** 一条采集告警在表格里的一行。 */
export interface WarningRow {
  readonly kind: string
  readonly label: string
  readonly count: number
}

/** collectionWarningRows 给告警配上标签，未收录的取值照原样显示。 */
export function collectionWarningRows(warnings: readonly CollectionWarning[]): WarningRow[] {
  return warnings.map((w) => ({
    kind: w.kind,
    label: WARNING_KIND_LABEL[w.kind] ?? w.kind,
    count: w.count,
  }))
}

/**
 * collectionCoverageNote 一句话说清这次采集看见了什么、没看见什么。
 *
 * 两个数字并排而不是只报"采到了 N 类"：只报成功那一半，一次
 * FORBIDDEN 会消失在一个看起来不错的数字后面（spec §4.2 的方向）。
 */
export function collectionCoverageNote(rows: readonly CollectionRow[]): string {
  const failed = rows.filter((r) => !r.observed).length
  if (failed === 0) return `${rows.length} 类资源全部采到`
  return `${rows.length} 类资源中有 ${failed} 类没采到`
}

/** formatCollectedAt 把时刻渲染成本地时间；取值不合法时原样返回。 */
export function formatCollectedAt(iso: string): string {
  const t = new Date(iso)
  return Number.isNaN(t.getTime()) ? iso : t.toLocaleString()
}

/**
 * collectionDuration 返回一次采集耗时的文案。
 *
 * 时间跨度不合法（顺序颠倒、无法解析）时返回空串而不是一个负数或
 * `NaN 秒`：一个显然错误的数字会被当成真的读。
 */
export function collectionDuration(startedAt: string, finishedAt: string): string {
  const ms = new Date(finishedAt).getTime() - new Date(startedAt).getTime()
  if (!Number.isFinite(ms) || ms < 0) return ''
  return `${(ms / 1000).toFixed(1)} 秒`
}

/**
 * summaryStatusTone 决定结果指标卡的语义色。
 *
 * PARTIAL 与 FAILED 都不是中性色：一次部分成功的采集在视觉上与全绿
 * 一样，就等于把它读成了成功（spec §17.1：能力边界必须与正常指标同等显著）。
 */
export function summaryStatusTone(status: string): 'unknown' | 'deny' | undefined {
  if (status === 'FAILED') return 'deny'
  if (status === 'OK') return undefined
  // PARTIAL 与任何未收录的取值都按"说不准"处理，不按正常处理。
  return 'unknown'
}

/** collectionSummaryRows 是页面渲染一份摘要所需的全部派生值。 */
export function collectionSummaryView(s: CollectionSummary) {
  const rows = collectionRows(s.resources)
  return {
    rows,
    warningRows: collectionWarningRows(s.warnings),
    statusLabel: RUN_STATUS_LABEL[s.status] ?? s.status,
    statusTone: summaryStatusTone(s.status),
    coverageNote: collectionCoverageNote(rows),
    collectedAt: formatCollectedAt(s.finishedAt),
    duration: collectionDuration(s.startedAt, s.finishedAt),
    // 空串表示这一轮真的读到了集群。页面据此决定要不要出这条提示 ——
    // 每一轮都顶着一句话，会让真正出事的那一轮看起来和平常一样。
    errorNote: runErrorNote(s.errorReason),
  }
}
