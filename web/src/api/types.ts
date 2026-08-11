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
  /**
   * 做出判定的方向。NetworkPolicy 是有方向的，一条 DENY 边究竟该改
   * 源端的 egress 规则还是目的端的 ingress 规则，只看边本身答不出来。
   * 两侧都出现过时为 MIXED —— 给一个五五开的答案比不给更糟。
   */
  decidedBy: 'INGRESS' | 'EGRESS' | 'MIXED' | ''
}

/** 拓扑聚合粒度。 */
export type TopologyLevel = 'namespace' | 'workload'


export interface Topology {
  nodes: TopologyNode[]
  edges: TopologyEdge[]
  /** 因端点身份缺失而无法定位到节点的流量数。必须展示，不得静默忽略。 */
  unplaceableFlowCount: number
  /** 实际生效的聚合粒度，回显给界面。 */
  level: TopologyLevel
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

/** 高风险端口的风险来源。封闭枚举，与后端 store.RiskCategory 对齐。 */
export type RiskCategory = 'ADMIN_PLAINTEXT' | 'DATABASE' | 'FILE_SHARE'

/**
 * RiskCategory 的中文标签，供 SecurityPage 与 PolicyPage 共用——两页都会
 * 展示同一枚举，分别维护一份必然措辞漂移（同 UNKNOWN_REASON_LABEL 的理由）。
 * 键类型用 string 而非 RiskCategory：PolicyPage 里 CandidateRule.risk.category
 * 是后端的裸 string，未收录的取值必须按原样展示，不丢弃、不因类型收窄而报错。
 */
export const RISK_CATEGORY_LABEL: Record<string, string> = {
  ADMIN_PLAINTEXT: '明文管理端口',
  DATABASE: '数据库直连',
  FILE_SHARE: '文件共享',
}

/** 风险连接所处的位置。与 RiskCategory 分列，不合成单一分数。 */
export type RiskPosition = 'EGRESS_INTERNET' | 'CROSS_NAMESPACE' | 'SAME_NAMESPACE'

export interface RiskPort {
  port: number
  name: string
  category: RiskCategory
}

export interface RiskyFlow extends FlowRecord {
  category: RiskCategory
  position: RiskPosition
  portName: string
}

export interface EgressTarget {
  address: string
  ports: number[]
  flowCount: number
  /** 被放行的条数。与总数分列：畅通的外联与已被挡住的外联不是一回事。 */
  allowedCount: number
  unknownCount: number
}

export interface NakedPod {
  cluster: string
  namespace: string
  name: string
}

export interface SecurityReport {
  cluster: string
  /** 流量类发现的时间窗。nakedPods 来自资产快照，不受此窗口约束。 */
  window: TimeWindow
  riskyFlows: RiskyFlow[]
  egressTargets: EgressTarget[]
  nakedPods: NakedPod[]
  /** 判定所用的端口清单。报告为空时靠它区分"查过没发现"与"没查"。 */
  riskPortCatalog: RiskPort[]
}

/** 规则来源。BASELINE 由基础设施事实推导，LEARNED 由观测到的流量学习。 */
export type RuleOrigin = 'BASELINE' | 'LEARNED'

/** LEARNED 规则的证据等级，决定是否默认启用。 */
export type EvidenceClass =
  | 'TRUSTED_ALLOW' | 'TRUSTED_DENY' | 'INTERNET_EGRESS' | 'CROSS_CLUSTER'

/** BASELINE 规则的五类基础设施事实。 */
export type Kind =
  | 'DNS' | 'LB_HEALTH_CHECK' | 'METRICS_SCRAPE' | 'CONTROL_PLANE' | 'NODE_AGENT'

/** 一条流量无法生成候选规则的封闭原因枚举。 */
export type UngeneratableReason =
  | 'NO_WORKLOAD_LABEL' | 'IDENTITY_UNKNOWN' | 'DEGRADED_EVIDENCE' | 'UNMANAGED_ENDPOINT'

/** dry-run 预测的四类变化，WOULD_OPEN 不是"通过"而是敞口扩大。 */
export type ChangeKind = 'WOULD_BREAK' | 'WOULD_OPEN' | 'UNCHANGED' | 'UNKNOWN'

/** 一条 BASELINE 规则的推导依据；CLAUDE.md §3 要求 baseline 落库带依据，不得硬编码。 */
export interface Derivation {
  sourceKind: string
  cluster: string
  namespace: string
  name: string
  field: string
}

export interface CandidateRule {
  origin: RuleOrigin
  evidence?: EvidenceClass
  baseline?: Kind
  derivations?: Derivation[]
  risk?: { port: number; name: string; category: string }
  enabled: boolean
  direction: 'INGRESS' | 'EGRESS'
  flowCount: number
  /** 对端：selector 对端为 namespace/workload，ipBlock 对端为 CIDR。 */
  peers: string[]
  /** 端口，形如 TCP/8080。 */
  ports: string[]
}

export interface CandidatePolicy {
  cluster: string
  namespace: string
  workload: string
  rules: CandidateRule[]
}

export interface MissingBaseline {
  namespace: string
  kinds: Kind[]
}

export interface UngeneratableItem {
  flowId: string
  reason: UngeneratableReason
  detail: string
}

export interface ChangedFlow {
  flowId: string
  sourceLabel: string
  destLabel: string
  protocol: string
  port: number
  current: string
  predicted: string
  unknownReason: string
  confidence: string
  crossCluster: boolean
  unmanaged: boolean
}

export interface PredictionReport {
  changes: Record<ChangeKind, ChangedFlow[]>
  counts: Record<ChangeKind, number>
  /** UNKNOWN 的构成明细，与 Quality 页同一口径：只报比例无法定位该修哪个子系统。 */
  unknownComposition: Record<string, number>
  trustedCount: number
  degradedCount: number
  /** 可信度取值不在枚举内的条数，正常恒为 0；三者之和等于 totalEvaluated。 */
  unratedCount: number
  crossClusterCount: number
  unmanagedCount: number
  totalEvaluated: number
}

/** 集群接入状态：登记完成 → 学习中 → 可产出候选策略。由服务端推进，前端只读。 */
export type OnboardState = 'REGISTERED' | 'OBSERVING' | 'READY'
export type ImportRole = 'BASELINE_CURRENT' | 'CANDIDATE_ADDITION'
export type ImportSource = 'PASTE' | 'GIT' | 'CLUSTER'

export interface APIServer {
  host: string
  cidr: string
  port: number
}

export interface GitBinding {
  repoUrl: string
  branch: string
  policyPath: string
  credentialRef: string
  lastWrittenCommit: string
}

/**
 * 已注册集群。取代旧的 ClusterSummary（namespaceCount/podCount/flowCount
 * 三个字段已不存在）—— GET /api/v1/clusters 现在直接返回 registry.Cluster。
 * apiServers/healthCheckSources 为空时后端落库为 null，因此这两项与
 * git 均标为可选，不能假设它们总是数组/对象。
 */
export interface RegisteredCluster {
  id: string
  displayName: string
  podCidr: string
  nodeCidr: string
  ccnpPresent: boolean
  state: OnboardState
  apiServers?: APIServer[] | null
  healthCheckSources?: string[] | null
  git?: GitBinding
}

export interface PolicyImportItem {
  clusterId: string
  importId: string
  plane: string
  role: ImportRole
  source: ImportSource
  namespace: string
  name: string
  yaml: string
  specHash: string
  gitCommitSha: string
  importedBy: string
  importedAt: string
}

export const ONBOARD_STATE_LABEL: Record<OnboardState, string> = {
  REGISTERED: '已登记 · 尚未采集到流量',
  OBSERVING: '学习中',
  READY: '可产出候选策略',
}

export const IMPORT_ROLE_LABEL: Record<ImportRole, string> = {
  BASELINE_CURRENT: '现状（当前生效策略）',
  CANDIDATE_ADDITION: '候选补充',
}

export const IMPORT_SOURCE_LABEL: Record<ImportSource, string> = {
  PASTE: '粘贴',
  GIT: 'Git',
  CLUSTER: '集群',
}

export interface PolicyPreview {
  cluster: string
  namespace: string
  window: TimeWindow
  candidates: CandidatePolicy[]
  missingBaselines: MissingBaseline[]
  ungeneratable: UngeneratableItem[]
  prediction: PredictionReport
  /** 全量 baseline 类型清单，用于把 missingBaselines 的"没缺"与"没查"区分开。 */
  baselineKinds: Kind[]
}
