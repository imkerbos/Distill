export type Verdict = 'ALLOW' | 'DENY' | 'UNKNOWN'
export type Confidence = 'TRUSTED' | 'DEGRADED'

/** 后端统一响应包络。code 为 0 表示成功，非 0 时 data 为 null。 */
export interface Envelope<T> {
  code: number
  msg: string
  data: T | null
}

export interface Identity { username: string }

export interface ClusterSummary {
  id: string
  namespaceCount: number
  podCount: number
  flowCount: number
  ccnpPresent: boolean
}

export interface TopologyNode {
  id: string
  cluster: string
  namespace: string
  inMesh: boolean
  hasPolicy: boolean
  podCount: number
  unmanagedPodCount: number
  /** 该命名空间不属于本次查询的集群，本集群策略管不到它。 */
  foreign: boolean
}

export interface TopologyEdge {
  source: string
  target: string
  verdict: Verdict
  confidence: Confidence
  crossCluster: boolean
  flowCount: number
  ports: number[]
  /** 任一条成员流量的一端不受 NetworkPolicy 管控（如 hostNetwork Pod）。 */
  unmanaged: boolean
}

export interface Topology {
  nodes: TopologyNode[]
  edges: TopologyEdge[]
  /** 因端点身份缺失而无法定位到节点的流量数。必须展示，不得静默忽略。 */
  unplaceableFlowCount: number
}

export interface FlowRecord {
  id: string
  /** 流量发生时刻，RFC3339。 */
  timestamp: string
  sourceLabel: string
  destLabel: string
  protocol: string
  port: number
  verdict: Verdict
  confidence: Confidence
  unknownReason: string
  crossCluster: boolean
  unmanaged: boolean
}

/** 左闭右开的查询时间窗 [from, to)。 */
export interface TimeWindow {
  from: string
  to: string
}

/** 分页信息必须完整传到界面：returned < total 时用户有权知道被截断了。 */
export interface FlowPage {
  items: FlowRecord[]
  total: number
  returned: number
  limit: number
  /**
   * 实际生效的时间窗。理由同 total：一个按时间筛过的列表若不说明筛的是
   * 哪一段，在界面上与全量列表无法区分。
   */
  window: TimeWindow
}

export interface DecisionReason {
  direction: string
  isolated: boolean
  unmanaged: boolean
  matchedPolicy: string
  matchedRuleIdx: number
  detail: string
}

export interface Decision extends FlowRecord {
  reason: DecisionReason
}

export interface Quality {
  cluster: string
  totalFlows: number
  trustedRate: number
  unknownRate: number
  degradedRate: number
  unknownCount: number
  /** UNKNOWN 的构成明细。只报一个比例无法告诉运维该去修哪个子系统。 */
  unknownComposition: Record<string, number>
  crossClusterCount: number
  nakedPodCount: number
  unmanagedPodCount: number
  policyCoverage: number
}

export interface FlowFilter {
  cluster?: string
  verdict?: Verdict
  confidence?: Confidence
  limit?: number
}

/**
 * unknownReason 是后端的封闭枚举；这份文案是唯一副本，抽屉和数据质量页
 * 都从这里取，不各自维护一份——两份文案一旦分别维护就必然措辞漂移，
 * 而后端新增枚举值时也只有一处需要同步（对应 CLAUDE.md「新增原因要
 * 同步更新枚举与统计口径」）。未收录的值直接展示原始字符串，不丢弃、
 * 不留空。
 */
export const UNKNOWN_REASON_LABEL: Record<string, string> = {
  POLICY_MALFORMED: '策略本身无法解析',
  SNAPSHOT_MISSING: '缺少对应时刻的资产快照',
  IP_AMBIGUOUS: '同集群内 IP 复用，时间上不可区分',
  CLUSTER_AMBIGUOUS: '跨集群网段重叠，归属不唯一',
  IDENTITY_LOST_MESH: 'sidecar 导致源身份丢失',
  CCNP_PRESENT: '存在 Cilium 策略，标准 NetworkPolicy 结论不可靠',
  NAT_TRANSLATED: '地址被转换，无法还原原始主体',
  EXTERNAL_NO_IDENTITY: '公网流量无可归属主体',
  NAMED_PORT_UNRESOLVED: '命名端口无法解析为具体端口号',
  LOG_SAMPLED_OUT: '日志采样或限流导致记录缺失',
  UNSPECIFIED: '未记录具体原因',
}
