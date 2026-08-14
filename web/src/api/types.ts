export type Verdict = 'ALLOW' | 'DENY' | 'UNKNOWN'
export type Confidence = 'TRUSTED' | 'DEGRADED'

/** 后端统一响应包络。code 为 0 表示成功，非 0 时 data 为 null。 */
export interface Envelope<T> {
  code: number
  msg: string
  data: T | null
}

export interface Identity { username: string }

/**
 * 平台角色的封闭枚举，取值与服务端 registry.Role 一一对应。
 *
 * 界面拿到它只用来决定渲染什么（design doc 2026-08-14 §8）。**它不是权限**：
 * 授权判定在服务端每次请求现读账号记录（internal/auth Verifier.RoleOf），
 * 这里这个字符串只是那次判定结果的一份回显（规范 §34）。
 */
export type Role = 'ADMIN' | 'VIEWER'

/**
 * GET /api/v1/sessions/current 的响应。
 *
 * 与 Identity 分开而不是给它加一个可选的 role：登录（POST /sessions）的
 * 响应里**没有**角色，服务端刻意只回用户名。合成一个可选字段的类型，
 * 调用方就分不出「这次没带角色」和「这个人没有角色」，而两者的正确处置
 * 相反 —— 前者要去问一次当前会话端点，后者要什么都不渲染。
 */
export interface CurrentSession {
  username: string
  role: Role
}

/**
 * 一条平台账号的读模型，对应服务端 accountView。
 *
 * **这里没有密码，也没有密码哈希，这是刻意的**：服务端的 accountView 与
 * registry.Account 同样没有那个字段（规范 §19、§20、§35）。三层都没有它，
 * 「某条路径把哈希带到界面上」就成了写不出来的状态，而不是每次改动都要
 * 有人记得去检查的一条规则。
 */
export interface Account {
  username: string
  role: Role
  /**
   * 为空表示账号处于启用状态。
   *
   * 是时刻而不是一个 disabled 布尔：停用是可逆的暂停，「什么时候被停的」
   * 是操作者要看的信息，布尔答不出来（design doc §3）。
   */
  disabledAt: string | null
  createdAt: string
  updatedAt: string
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
  | 'LABEL_KEY_CONFLICT'

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
  /** 规则内容的 SHA-256，人工覆盖决定（RuleOverride）挂在它上面。 */
  fingerprint: string
}

/** 人工决定的方向。ENABLE 覆盖一条默认禁用的规则，DISABLE 覆盖一条默认启用的规则。 */
export type OverrideDecision = 'ENABLE' | 'DISABLE'

/**
 * 一条人工确认或否决。与 registry.RuleOverride 逐字段对齐
 * （Task 5 实测响应，见 task-5-report.md）。
 */
export interface RuleOverride {
  clusterId: string
  namespace: string
  workload: string
  fingerprint: string
  decision: OverrideDecision
  /** 做出该决定的理由，非空——半年后有人问「这条规则为什么是开的」，答案在这里。 */
  reason: string
  decidedBy: string
  decidedAt: string
  /** 落进 Git 的 commit；本轮恒为空，"已提交决定"与"已在集群生效"是两回事。 */
  mergedCommitSha: string
}

/**
 * 一条已失效的确认：规则内容变了，指纹不再匹配当前候选集，
 * 或者当初的决定试图禁用一条 BASELINE 规则（不接受）。
 */
export interface StaleOverride {
  /** 当初的决定，含当时的理由与时间。 */
  override: RuleOverride
  /** 该 (namespace, workload) 当前的全部规则描述；workload 已不存在时为空数组。 */
  currentRules: string[]
  /** 失效原因，固定文案（封闭集合，非自由文本）。 */
  reason: string
}

/** 应用人工决定之后的候选策略与预测，与默认推荐并列展示。 */
export interface OverriddenView {
  candidates: CandidatePolicy[]
  prediction: PredictionReport
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

/**
 * 一个 Pod 未进入候选策略花名册的封闭原因枚举。
 *
 * 与 UngeneratableReason 分列，不合并：后者说的是"某条流量表达不成规则"，
 * 这一套说的是更早一步的缺口——这个 Pod 从未进入名册，因此不会作为主体
 * 出现在任何一条流量判定里，UngeneratableReason 报不出它。
 */
export type WorkloadExclusionReason =
  | 'UNMANAGED_ENDPOINT' | 'NO_WORKLOAD_LABEL' | 'LABEL_KEY_CONFLICT'

/** 一个从未进入候选策略花名册的 Pod：它没有任何候选策略覆盖。 */
export interface ExcludedWorkload {
  namespace: string
  pod: string
  /** 该 Pod 的原始标签，供排查是否只是标签键拼错或用了平台还未识别的键。 */
  labels: Record<string, string>
  reason: WorkloadExclusionReason
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

/**
 * 仓库级只读校验的结论。封闭枚举，与后端 registry.RepoVerifyResult 逐值对齐。
 *
 * 与路径级的 PathVerifyResult 拆成两个类型，而不是一个枚举配两套「哪些
 * 取值在这一层合法」的约定（design doc 2026-08-13 §3.3）：后者的约定只活在
 * 注释与 review 里，迟早会漂，而漂的方向是把一句仓库级的话当成路径级的
 * 结论渲染出去 —— 「认证被拒绝」显示在 policyPath 那一格上，读的人会以为
 * 那是关于路径的判断，然后去改一个根本没问题的路径。
 *
 * 类型是联合而不是 string：这份取值表在界面上要一一对应到文案，用 string
 * 就等于允许后端某天多一个取值而界面照旧渲染出一个空格。校验结论是
 * 「这个仓库可不可信」的判断，空格会被读成「没问题」。
 */
export type RepoVerifyResult =
  | 'NOT_VERIFIED' | 'OK' | 'CREDENTIAL_UNRESOLVED' | 'AUTH_FAILED'
  | 'REPO_UNREACHABLE' | 'BRANCH_MISSING'

/**
 * 路径级只读校验的结论。封闭枚举，与后端 registry.BindingVerifyResult 逐值对齐。
 *
 * 只回答一个问题：policyPath 在仓库的那个分支上是否存在。仓库级的四个
 * 失败取值不在这里 —— 拆分的理由写在 RepoVerifyResult 上。
 *
 * 路径级以仓库级为前提：仓库级不是 OK 时这一层只能是 NOT_VERIFIED，
 * 不得报 PATH_MISSING（design doc §3.3）。这条由服务端落实，前端不重算，
 * 只是照着它给的结论渲染。
 */
export type PathVerifyResult = 'NOT_VERIFIED' | 'OK' | 'PATH_MISSING'

/**
 * 一个策略仓库，独立于集群存在（design doc 2026-08-13 §3.1）。
 *
 * 它在任何集群绑定它之前就存在，因此没有 clusterId：把集群塞进这里等于
 * 宣称一个仓库属于某个集群，而这正是本次拆分要消除的东西。
 */
export interface GitRepo {
  repoId: string
  repoUrl: string
  branch: string
  /** Secret Manager 中凭据的引用。凭据本身永不经由接口传输。 */
  credentialRef: string
  /** 最近一次仓库级只读校验的结论。 */
  verifyResult: RepoVerifyResult
  /**
   * 最近一次仓库级校验发生的时间。
   *
   * 后端带 omitempty，从未校验过时这个键根本不出现——因此是可选而不是
   * 恒存在的 null。它是历史事实，不是当前状态：一个三天前的 OK 说的是
   * 三天前那一刻，不能拿来声明此刻的仓库可信（design doc §3.4）。
   */
  verifiedAt?: string | null
}

/**
 * 仓库写入体（POST /git-repos 与 PUT /git-repos/{repoID} 共用）。
 *
 * 用 Pick 而不是 Omit，理由同 GitBindingWrite：白名单只放行点名的四项，
 * 读模型将来多一个平台自产的字段时它不会漏进写路径。
 *
 * verifyResult / verifiedAt 不在这里：它们是平台对仓库可信度的判断，由
 * 服务端在校验后自己写入。一个能由调用方提交的结论可以被填成 OK，于是
 * 「已校验通过」这句话再也无法被证伪。
 *
 * repoId 在这里，但 PUT 时服务端只认路径参数（internal/httpapi/repo_handler.go）：
 * 仓库 ID 是不可改键，允许改它等于让一次「改地址」把审计里那一串行变成
 * 无主记录。前端也就不给它编辑入口。
 */
export type GitRepoWrite = Pick<GitRepo, 'repoId' | 'repoUrl' | 'branch' | 'credentialRef'>

/**
 * 集群与一个已存在策略仓库的绑定。
 *
 * **不含仓库地址、分支与凭据**：它们属于 GitRepo（design doc §3.2）。留在
 * 这里就是两个真相来源 —— 操作者在集群页改了地址，仓库页显示的还是旧的，
 * 而平台真正会去连的是哪一个，只能靠读代码才知道。
 */
export interface GitBinding {
  repoId: string
  policyPath: string
  lastWrittenCommit: string
  /** 最近一次路径级只读校验的结论。 */
  verifyResult: PathVerifyResult
  /** 最近一次路径级校验发生的时间。语义同 GitRepo.verifiedAt。 */
  verifiedAt?: string | null
}

/**
 * POST /git-repos/{repoID}/verify 与 POST /git-repos 的响应。
 *
 * 后端只回结论，不回整个仓库（见 httpapi 的 repoVerifyStatus）——调用方已经
 * 有仓库地址与引用了。本页拿到它之后仍然重新拉一次仓库列表：服务端是
 * 唯一真相源，就地把这两个字段贴进本地行会让界面出现一份没人能核对的
 * 拼接状态。
 */
export interface RepoVerifyStatus {
  verifyResult: RepoVerifyResult
  verifiedAt?: string | null
}

/**
 * PUT / POST /clusters/{id}/git-binding[/verify] 的响应。
 *
 * 与 RepoVerifyStatus 是两个类型而不是一个带 string 字段的通用形状，理由
 * 与后端 bindingVerifyStatus 完全一样：两个枚举各自封闭，共用一个载体就
 * 等于允许把 AUTH_FAILED 当成路径级的结论传下去。
 */
export interface PathVerifyStatus {
  verifyResult: PathVerifyResult
  verifiedAt?: string | null
}

/**
 * 已注册集群。GET /api/v1/clusters 现在直接返回 registry.Cluster——
 * 旧形状（namespaceCount/podCount/flowCount）已整体不存在，不保留两套类型。
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

/**
 * 集群写入体（POST /clusters 与 PUT /clusters/{id} 共用）。
 *
 * 刻意不是 `Partial<RegisteredCluster>`：PUT 是整体替换，服务端把请求体
 * 的每一个字段都写进那一行，缺省的字段被写成空值而不是保持原样。而
 * podCidr / nodeCidr 是求值层做网段分类的依据，悄悄清零它们会污染此后
 * 每一次判定。因此每一项都是必填——少给一项要在编译期就是错误，不能
 * 留给运行时。
 *
 * **git 不在这里**：绑定是有自己生命周期的资源，走自己的两条路由
 * （PUT / DELETE /clusters/{id}/git-binding）与自己的审计动作
 * （design doc 2026-08-13 §5、§7）。服务端的 clusterPayload 已经不再
 * 接受它，留一个字段在这一侧不是多一个没用的键 —— 而是操作者填了仓库
 * 地址、请求返回成功、绑定却从未写下，直到某天有人发现这个集群的策略
 * 一直没有下发。
 *
 * state 不在这里：接入状态由服务端根据实际采集到的数据推进，请求体
 * 里提供它就等于允许调用方把"还没有数据"标成"可以出推荐了"。
 *
 * ccnpPresent 与 state 恰恰相反，必须在这里：它是操作者告诉平台的一个
 * 事实（这个集群里另有 CiliumClusterwideNetworkPolicy 在生效），没有任何
 * 自动探测在维护它，而它的用途是**强制把该集群的判定降级为 DEGRADED**。
 * 漏掉它的后果是单向的：整体替换会把它写成 false，于是一次与它无关的
 * 编辑让平台看上去比它应该的样子更有把握——正是 CLAUDE.md §3 禁止的
 * 「为了好看的结论放宽严谨性」，且悄无声息。
 *
 * 也不能改成"服务端保留库里的现值"：那会让这一项永远改不了，而集群里
 * 装上 Cilium 是随时可能发生的事。既然它由调用方提供，就必须由调用方
 * 每次原样带上。
 */
export interface ClusterWrite {
  id: string
  displayName: string
  podCidr: string
  nodeCidr: string
  ccnpPresent: boolean
  apiServers: APIServer[]
  healthCheckSources: string[]
}

/**
 * Git 绑定写入体：PUT /clusters/{id}/git-binding 的请求体，独立于 ClusterWrite。
 *
 * 两个字段就是绑定的全部可写内容（design doc 2026-08-13 §4），共同点是
 * 都由操作者提供、平台自己产生不了。仓库地址、分支与凭据引用不在这里：
 * 它们属于仓库，由 /git-repos 那组端点管，在集群页上只读展示。凡是平台
 * 自己产生的东西同样不在这里：
 *
 * lastWrittenCommit 是平台对"我最近一次往这个仓库写了什么"的断言，漂移
 * 检测拿它与 Git 现状比对。它由服务端从库里的现值推导，请求体带上去也
 * 不会被采纳（见 internal/httpapi 的 gitBindingPayload）——所以这里根本
 * 不给它位置：一个永远不会被采纳的字段留在类型里，只会让下一个调用方
 * 以为它有用。
 *
 * verifyResult / verifiedAt 出于同一条理由被排除，且理由更硬：它们是
 * 平台对绑定可信度的判断，由服务端在保存时自己校验后写入。一个能由
 * 调用方提交的结论可以被填成 OK，于是"已校验通过"这句话再也无法被
 * 证伪——这正是 verdict 与 confidence 必须分列的同一条纪律。
 *
 * 用 Pick 而不是 Omit：白名单只放行点名的两项，读模型将来多一个平台
 * 自产的字段时它不会漏进写路径；黑名单则要靠每次新增字段的人记得回来
 * 补一笔，漏掉了没有任何东西会提醒他。
 */
export type GitBindingWrite = Pick<GitBinding, 'repoId' | 'policyPath'>

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
  /**
   * 从未进入候选策略花名册的 Pod。
   *
   * 类型带 `| null`，理由同 overrides：后端用 `var excluded []ExcludedWorkload`
   * 收集，零条时是 nil 切片，`encoding/json` 序列化成 `null` 而不是 `[]`。
   */
  excludedWorkloads: ExcludedWorkload[] | null
  prediction: PredictionReport
  /** 全量 baseline 类型清单，用于把 missingBaselines 的"没缺"与"没查"区分开。 */
  baselineKinds: Kind[]
  /**
   * 当前生效的人工决定。
   *
   * 类型带 `| null`——这不是照抄 Go 结构体，是实测出来的：mysqlregistry.
   * RuleOverrides 用 `var out []registry.RuleOverride` 收集结果，零条时
   * 是 nil 切片，`encoding/json` 把它序列化成 `null` 而不是 `[]`。没有
   * 覆盖是本页最常见的初始状态，这个 null 必须由调用方兜底，不能假设
   * 后端总给数组。
   */
  overrides: RuleOverride[] | null
  /** 已失效的确认，单独报出而不是静默丢弃。 */
  staleOverrides: StaleOverride[]
  /** 应用人工决定之后的版本，与默认推荐（本对象其余字段）并列展示。 */
  overridden: OverriddenView
}

/* ---------------------------------------------------------------------- */
/* Git 写回                                                                 */
/* ---------------------------------------------------------------------- */

/**
 * 计划里的一个待写文件，对应 registry.WritebackFile。
 *
 * content 是整份渲染好的文件，不是补丁：写回只新增与更新整份文件
 * （design doc 2026-08-14 §3）。界面**不展示也不解析它**——文件必须逐字节
 * 是服务端产出的那一份，要看内容走导出（规范 §20：只展示需要的那部分）。
 */
export interface WritebackFile {
  path: string
  content: string
}

/**
 * 一次写回的完整计划，对应 registry.WritebackPlan。
 *
 * 它是操作者二次确认的对象，因此界面上的每一项都必须原样来自这里：文件
 * 清单、目标分支、提交信息、重算后的四类计数，一样都不在前端重新推导
 * （design doc §4）。指纹覆盖以上全部内容，推送时原样回带。
 */
export interface WritebackPlan {
  /** 将要新增或更新的文件。平台从不删除仓库里的文件。 */
  files: WritebackFile[]
  /** 目标分支，永远是新建的 distill/* 分支，不是绑定里那条部署分支（§2）。 */
  branch: string
  /**
   * 提交信息。**合并请求上的评审人唯一会读的那句话**（§7），因此必须在
   * 推送之前展示给操作者：它会永久留在仓库历史里。
   */
  commitMessage: string
  /** 写回这一刻重新跑出来的四类计数（§4）。 */
  counts: Record<ChangeKind, number>
  /**
   * 仓库路径下已有、但本次候选集不包含的文件，交人工处置（§3）。
   *
   * 带 `| null`：后端用 `[]string` 收集，零条时序列化成 `null` 而不是 `[]`
   * （同 PolicyPreview.overrides 的理由）。
   */
  extraneous: string[] | null
  /** 这份计划的内容指纹，推送时原样回带。前端不重算，也无从重算。 */
  fingerprint: string
}

/**
 * 出计划端点的响应。
 *
 * 两层校验结论与计划一起回：计划是操作者二次确认的对象，而"平台刚刚重新
 * 校验过、结论是什么"正是他决定确不确认的依据之一（design doc §4）。
 * 两个结论各自是封闭枚举、各自一个字段，与后端 writebackPlanView 同形状——
 * 合成一个字段就等于允许把仓库级的 AUTH_FAILED 当成路径级的结论渲染出去。
 */
export interface WritebackPlanResult {
  plan: WritebackPlan
  repoVerifyResult: RepoVerifyResult
  bindingVerifyResult: PathVerifyResult
}

/**
 * 推送端点的响应。
 *
 * commit 是平台推上去的那个，**不是合并后的**（design doc §8）：合并由人做，
 * 平台不知道也不该假装知道。counts 是写之前重算的那一套，即真正随这次提交
 * 落进仓库的那几个数。
 */
export interface WritebackPushResult {
  branch: string
  commit: string
  files: number
  counts: Record<ChangeKind, number>
}

/* ---------------------------------------------------------------------- */
/* 平台设置                                                                 */
/* ---------------------------------------------------------------------- */

/**
 * 凭据解析后端。封闭枚举，与后端 registry.SecretsBackend 逐值对齐。
 *
 * 联合而不是 string：这份取值表在设置页上要一一对应到文案与一组字段
 * 约束，用 string 就等于允许后端某天多一个取值而界面照旧渲染出一个空
 * 选项——而选中一个界面读不出名字的凭据后端，等于平台去哪里取身份这
 * 件事没人说得清。
 */
export type SecretsBackend = 'NONE' | 'DIR' | 'SECRET_MANAGER'

/**
 * GET /api/v1/settings 的响应。
 *
 * **没有 gitVerifyHostKeys 字段**，这与后端 httpapi.settingView 是同一个
 * 刻意的形状：host key 原文只入不出，读取端点回的是指纹（design doc §1.3、
 * 规范 §19/§20）。类型里根本没有承载原文的位置，于是"设置页把 host key
 * 显示出来了"这件事在前端也写不出来，不靠页面自觉。
 */
export interface PlatformSettingView {
  sessionTtlSeconds: number
  httpReadTimeoutMs: number
  httpWriteTimeoutMs: number
  httpShutdownTimeoutMs: number
  secretsBackend: SecretsBackend
  secretsProject: string
  secretsPrefix: string
  secretsDir: string
  gitVerifyTimeoutMs: number
  /** 当前 host key 原文的 SHA-256 指纹；未配置时为空串。 */
  gitVerifyHostKeysFingerprint: string
}

/**
 * PUT /api/v1/settings 的请求体。
 *
 * 全字段必填，理由同 ClusterWrite：服务端写整行，缺省的字段会被写成零值
 * （internal/mysqlregistry/setting.go UpdateSetting 是一条整行 UPDATE）。
 * 让类型强制每一项都出现，调用方就不可能"只发一个字段"然后把超时清成 0。
 */
export interface PlatformSettingWrite {
  sessionTtlSeconds: number
  httpReadTimeoutMs: number
  httpWriteTimeoutMs: number
  httpShutdownTimeoutMs: number
  secretsBackend: SecretsBackend
  secretsProject: string
  secretsPrefix: string
  secretsDir: string
  gitVerifyTimeoutMs: number
  /**
   * known_hosts 原文。只入不出：读取端点没有对应字段，无从回显。
   *
   * 唯一可选的一项，**缺席 = 保持不变**（后端 httpapi.settingPayload 的
   * 那个指针字段）。它是唯一一个读不回来的字段，因此也是唯一一个在整行
   * 替换里表达不出「这次不动它」的字段 —— 不给它一个缺席的位置，一个手上
   * 不再留着 known_hosts 原文的操作者就连会话 TTL 都改不了。
   *
   * 显式给出空串是「清空」，那件事由服务端 registry.ValidateSettingUpdate
   * 拒绝；本页从不构造那种请求体。
   */
  gitVerifyHostKeys?: string
}
