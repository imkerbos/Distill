import { isNoRunError, isUnknownClusterError } from '../pages/collectionView'
import type {
  Account, ClusterAgent, ClusterWrite, CollectionState, CollectionSummary,
  CurrentSession, Decision, Envelope, FlowFilter, FlowPage, GitBindingWrite, IssuedAgentToken,
  GitRepo, GitRepoWrite, Granularity, Identity, ImportRole, ImportSource, IngestSummary, OverrideDecision,
  PathVerifyStatus, PlatformSettingView, PlatformSettingWrite, PolicyImportItem, PolicyPreview,
  Quality, RegisteredCluster, RepoVerifyStatus, Role, SecurityReport, Topology, TopologyLevel,
  WritebackPlanResult, WritebackPushResult,
  DriftStatus,
} from './types'

/** ApiError 同时携带 HTTP 状态与业务码，调用方两者都可能需要判断。 */
export class ApiError extends Error {
  readonly code: number
  readonly msg: string
  readonly status: number

  constructor(code: number, msg: string, status: number) {
    super(msg)
    this.name = 'ApiError'
    this.code = code
    this.msg = msg
    this.status = status
  }
}

type UnauthorizedHandler = () => void
let onUnauthorizedCb: UnauthorizedHandler | null = null

/**
 * onUnauthorized 注册全局未认证回调。
 *
 * 401 在这一层集中处理，而不是散落在每个页面：会话随时可能过期，
 * 任何一个请求都可能是第一个撞上它的，逐个页面处理必然漏。
 */
export function onUnauthorized(cb: UnauthorizedHandler) {
  onUnauthorizedCb = cb
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    ...init,
    headers: { 'Content-Type': 'application/json', ...(init?.headers ?? {}) },
  })

  if (res.status === 401) {
    onUnauthorizedCb?.()
  }

  let body: Envelope<T>
  try {
    body = await res.json()
  } catch {
    // 后端在任何情况下都应返回包络；解析不了说明连不上或被代理拦了。
    throw new ApiError(-1, '服务无响应', res.status)
  }

  if (body.code !== 0) {
    throw new ApiError(body.code, body.msg, res.status)
  }
  return body.data as T
}

function query(filter: FlowFilter): string {
  const p = new URLSearchParams()
  if (filter.cluster) p.set('cluster', filter.cluster)
  if (filter.verdict) p.set('verdict', filter.verdict)
  if (filter.confidence) p.set('confidence', filter.confidence)
  if (filter.limit !== undefined) p.set('limit', String(filter.limit))
  const s = p.toString()
  return s ? `?${s}` : ''
}

/** 一次导出下载：服务端产出的字节，与它自带的文件名。 */
export interface PolicyExportFile {
  /** 响应体原样。前端不重建 YAML，也不重建注释头（design doc 2026-08-14 §2、§3）。 */
  readonly blob: Blob
  readonly filename: string
}

/**
 * 从 Content-Disposition 取文件名，取不到时退回一个固定名。
 *
 * 逐字符过滤而不是原样使用：这个取值来自响应头，而它会被当作
 * `<a download>` 的文件名落到操作者的磁盘上——路径分隔符与控制字符
 * 在那一步不是显示问题（规范 §26）。退回的名字只是名字，文件内容
 * 始终是服务端那一份。
 */
function exportFilename(header: string | null): string {
  const fallback = 'distill-policy-export.yaml'
  const m = header?.match(/filename="?([^";]+)"?/)
  if (!m) return fallback
  const safe = m[1].replace(/[^A-Za-z0-9._-]/g, '-')
  return safe === '' ? fallback : safe
}

/**
 * 只接受本平台自己的 API 路径。
 *
 * 取值目前恒来自 pages/writebackView.ts，这一行是纵深防御：一个收路径的
 * 请求函数早晚会被某次改动接上一个来自响应体的地址，而写回那两个端点是
 * 会改仓库的（规范 §13、§30）。抛而不是静默改写——被改写过的路径会让
 * 调用方以为自己请求的是另一个东西。
 */
function assertApiPath(path: string): string {
  if (!path.startsWith('/api/v1/')) {
    throw new ApiError(-1, '请求路径不合法', 0)
  }
  return path
}

export const api = {
  login: (username: string, password: string) =>
    request<Identity>('/api/v1/sessions', {
      method: 'POST',
      body: JSON.stringify({ username, password }),
    }),

  logout: () => request<null>('/api/v1/sessions/current', { method: 'DELETE' }),

  // 当前会话是界面获知自己角色的**唯一**来源（design doc 2026-08-14 §8）。
  // 登录响应里没有角色，Cookie 是 HttpOnly 读不到，本地也不存任何身份 ——
  // 于是「我是不是管理员」在客户端根本无从自称，只能来自这一次请求，而
  // 服务端回的是它本次授权判定用过的那个角色（handleCurrentSession）。
  me: () => request<CurrentSession>('/api/v1/sessions/current'),

  /* 账号管理。全部端点服务端声明 accessAdmin，只读账号一律被拒（§34）。 */

  accounts: () => request<Account[]>('/api/v1/accounts'),

  // 新建账号固定建成只读，请求体里没有 role：服务端的 createAccountRequest
  // 也没有这个字段，提权只能走 updateAccountRole 那一次单独的、有自己审计
  // 动作的操作。带一个不会被采纳的 role 上去，调用方会以为自己建出了管理员。
  //
  // 明文密码走请求体，不走路径也不走查询串：URL 会进浏览器历史、Referer
  // 与服务端访问日志，而那三处都不该出现密码（规范 §19、§21）。响应里
  // 只有用户名与角色 —— 服务端不回显它刚收到的那个密码。
  createAccount: (username: string, password: string) =>
    request<{ username: string; role: Role }>('/api/v1/accounts', {
      method: 'POST',
      body: JSON.stringify({ username, password }),
    }),

  // 全平台唯一一处把 role 放进请求体的调用，而它的含义是「要改成什么」，
  // 不是「我是什么」（规范 §9）。降级最后一个启用中的管理员由服务端在
  // 同一事务内拒绝，界面上的计数只是提前把话说在前面（design doc §5）。
  updateAccountRole: (username: string, role: Role) =>
    request<{ username: string; role: string }>(
      `/api/v1/accounts/${encodeURIComponent(username)}/role`,
      { method: 'PUT', body: JSON.stringify({ role }) },
    ),

  disableAccount: (username: string) =>
    request<{ username: string }>(
      `/api/v1/accounts/${encodeURIComponent(username)}/disable`, { method: 'POST' },
    ),

  enableAccount: (username: string) =>
    request<{ username: string }>(
      `/api/v1/accounts/${encodeURIComponent(username)}/enable`, { method: 'POST' },
    ),

  // 软删除：审计行记录着这个账号做过什么，用户名也不再回收（design doc §3）。
  deleteAccount: (username: string) =>
    request<{ username: string }>(
      `/api/v1/accounts/${encodeURIComponent(username)}`, { method: 'DELETE' },
    ),

  // 管理员重置他人密码。不带当前密码 —— 管理员并不知道对方的密码，要求他
  // 提供等于把这个功能变成做不到的操作；由审计（RESET_PASSWORD）负责事后
  // 可查。新密码只在请求体里，响应回的是用户名。
  resetAccountPassword: (username: string, password: string) =>
    request<{ username: string }>(
      `/api/v1/accounts/${encodeURIComponent(username)}/password`,
      { method: 'POST', body: JSON.stringify({ password }) },
    ),

  // 改自己的密码。目标取自服务端的会话，路径里没有用户名 —— 让调用方指定
  // 目标，一个只读账号就能改管理员的密码（internal/httpapi/router.go 的注释）。
  //
  // **必须带当前密码**（规范 §28）：会话可能是一台没锁屏的机器留下的，而
  // 改密码会把账号的控制权永久转移给改的人。两个密码都在请求体里。
  changeOwnPassword: (currentPassword: string, newPassword: string) =>
    request<{ username: string }>('/api/v1/me/password', {
      method: 'POST',
      body: JSON.stringify({ currentPassword, newPassword }),
    }),

  // 平台设置。每次进页面都现读，不在模块里缓存：设置整体是"按需读取"的
  // （design doc 2026-08-13 §1.1），一份缓存下来的设置会在别处改完之后
  // 继续显示旧值，而操作者据此做的下一次保存是拿旧值整体覆盖。
  settings: () => request<PlatformSettingView>('/api/v1/settings'),

  // PUT 而非 PATCH：服务端是一条整行 UPDATE，请求体缺省的字段会被写成
  // 零值（internal/mysqlregistry/setting.go）。用 PATCH 命名它，早晚有人
  // 按 PATCH 的语义只发一个字段——落点是超时被清成 0（等于关掉超时保护）
  // 或 host key 被清空（信任锚消失）。PlatformSettingWrite 全字段必填与
  // 这条同源。
  //
  // 响应仍然只回指纹，不回 host key 原文：保存路径不是回显的旁路。
  updateSettings: (body: PlatformSettingWrite) =>
    request<PlatformSettingView>('/api/v1/settings', {
      method: 'PUT',
      body: JSON.stringify(body),
    }),

  clusters: () => request<RegisteredCluster[]>('/api/v1/clusters'),

  createCluster: (body: ClusterWrite) =>
    request<{ id: string }>('/api/v1/clusters', {
      method: 'POST',
      body: JSON.stringify(body),
    }),

  // PUT 而非 PATCH：服务端写整行，请求体缺省的字段会被写成空值。用
  // PATCH 命名一个整体替换的端点，早晚有人按 PATCH 的语义只发一个
  // 字段，然后把 podCidr 清成空串——那不是一次失败的请求，是此后
  // 每一次判定都用错了网段分类。ClusterWrite 全字段必填与这条同源。
  updateCluster: (cluster: string, body: ClusterWrite) =>
    request<{ id: string }>(`/api/v1/clusters/${encodeURIComponent(cluster)}`, {
      method: 'PUT',
      body: JSON.stringify(body),
    }),

  deleteCluster: (cluster: string) =>
    request<{ id: string }>(`/api/v1/clusters/${encodeURIComponent(cluster)}`, {
      method: 'DELETE',
    }),

  // 集群 agent（推送式采集的机器身份，design doc 2026-08-18 §3）。
  // 三条都声明 accessAdmin，只读账号一律 403。
  clusterAgents: (cluster: string) =>
    request<ClusterAgent[]>(`/api/v1/clusters/${encodeURIComponent(cluster)}/agents`),

  // 签发返回明文 token —— **全平台唯一一处**。调用方展示一次即弃，不留存。
  issueClusterAgent: (cluster: string) =>
    request<IssuedAgentToken>(`/api/v1/clusters/${encodeURIComponent(cluster)}/agents`, {
      method: 'POST',
      body: '{}',
    }),

  // 吊销不可逆。clusterID 一并带上：少了它，一个管理员能吊销别的集群的 agent。
  revokeClusterAgent: (cluster: string, agentId: string) =>
    request<null>(
      `/api/v1/clusters/${encodeURIComponent(cluster)}/agents/${encodeURIComponent(agentId)}`,
      { method: 'DELETE' },
    ),

  // 策略仓库：独立于集群的资源，绑定只指向它（design doc 2026-08-13 §3.1）。
  gitRepos: () => request<GitRepo[]>('/api/v1/git-repos'),

  // 新建之后服务端会自动校验一次，因此回的是仓库级结论而不是「已保存」
  // （design doc §4）。校验失败不阻止登记：存下来和可信是两件事。
  createGitRepo: (body: GitRepoWrite) =>
    request<RepoVerifyStatus>('/api/v1/git-repos', {
      method: 'POST',
      body: JSON.stringify(body),
    }),

  // PUT 而非 PATCH：服务端写整行，请求体缺省的字段会被写成空值。
  //
  // repoId 取自路径，请求体里的 repoId 不参与——仓库 ID 是不可改键。改它
  // 等于让一次「改地址」把审计里 git-repo/<旧 ID> 那一串行变成无主记录，
  // 而审计行正是事后唯一能回答「这个仓库的凭据是谁换的」的东西。
  //
  // 修改之后服务端把结论清成 NOT_VERIFIED 且不自动校验（换了地址之后，
  // 旧的 OK 描述的是另一个仓库），所以这里回的是 repoId 而不是结论。
  updateGitRepo: (repoID: string, body: GitRepoWrite) =>
    request<{ repoId: string }>(`/api/v1/git-repos/${encodeURIComponent(repoID)}`, {
      method: 'PUT',
      body: JSON.stringify(body),
    }),

  // 仍被某个集群绑定时服务端拒绝，不做级联（design doc §4）：级联会让一次
  // 仓库清理静默解除某个集群的策略下发路径。拒绝是一个业务失败，msg 里写着
  // 该先去解除绑定——调用方必须原样展示它，不能收窄成一句「删除失败」。
  deleteGitRepo: (repoID: string) =>
    request<{ repoId: string }>(`/api/v1/git-repos/${encodeURIComponent(repoID)}`, {
      method: 'DELETE',
    }),

  // 仓库级手动重校验。没有请求体，理由同 verifyGitBinding：要校验的是库里
  // 那个仓库，不是调用方随手给的一份。
  verifyGitRepo: (repoID: string) =>
    request<RepoVerifyStatus>(
      `/api/v1/git-repos/${encodeURIComponent(repoID)}/verify`,
      { method: 'POST' },
    ),

  // 把一个集群绑定到一个已存在的策略仓库。
  //
  // 独立端点而不是集群 PUT 的一部分：绑定有自己的生命周期与自己的审计
  // 动作（design doc 2026-08-13 §5）。走集群写路径的后果不是多发一次
  // 请求，而是一次只想改网段的编辑顺手重写了绑定——服务端的 clusterPayload
  // 现在根本不收 git，那样的请求会成功返回而绑定原封不动。
  //
  // 请求体只有 repoId 与 policyPath：仓库地址、分支与凭据属于仓库，改它们
  // 走 updateGitRepo。从这条路径带过去的地址服务端根本不收，而界面会显示
  // 保存生效——两个真相来源比一个错的更难查。
  //
  // 返回的是**路径级**校验结论而不是「已保存」：保存会顺带跑一次只读校验，
  // 形状与重校验端点一致，两处显示的是同一套结论。
  bindGitRepo: (cluster: string, body: GitBindingWrite) =>
    request<PathVerifyStatus>(`/api/v1/clusters/${encodeURIComponent(cluster)}/git-binding`, {
      method: 'PUT',
      body: JSON.stringify(body),
    }),

  // 解绑是自己的动词，不是「把四个字段清空后保存一次」：后者要靠调用方
  // 与服务端就一个约定俗成的空值形状达成默契，而任何一次误发的空请求体
  // 都会变成一次无声的解绑。
  unbindGitRepo: (cluster: string) =>
    request<{ id: string }>(`/api/v1/clusters/${encodeURIComponent(cluster)}/git-binding`, {
      method: 'DELETE',
    }),

  // 路径级手动重校验。没有请求体：要校验的是库里那个绑定，不是调用方随手
  // 给的一份——能由请求体指定校验目标，就等于允许拿一个能通过的仓库地址去
  // 换取另一个绑定的 OK。存在的理由是凭据轮换与权限修复之后需要一个新鲜
  // 结论（design doc §3.3）；平台不做后台定时校验。
  //
  // 这个端点只刷新路径级结论，不落仓库级结论：要刷新仓库那一层走
  // verifyGitRepo，两层在审计里始终是两条可以分开读的记录。
  verifyGitBinding: (cluster: string) =>
    request<PathVerifyStatus>(
      `/api/v1/clusters/${encodeURIComponent(cluster)}/git-binding/verify`,
      { method: 'POST' },
    ),

  // 漂移检测：写进去的那份策略现在还在不在。
  //
  // GET 而非 POST：它只读 —— 不写仓库、不改绑定、不动锚点，也不落结论
  // （design doc 2026-08-18-drift-detection §4）。
  gitBindingDrift: (cluster: string) =>
    request<DriftStatus>(
      `/api/v1/clusters/${encodeURIComponent(cluster)}/git-binding/drift`,
    ),

  policyImports: (cluster: string) =>
    request<PolicyImportItem[]>(
      `/api/v1/clusters/${encodeURIComponent(cluster)}/policy-imports`,
    ),

  createImport: (
    cluster: string,
    body: { role: ImportRole; source: ImportSource; yaml: string; gitCommitSha: string },
  ) =>
    request<{ importId: string }>(
      `/api/v1/clusters/${encodeURIComponent(cluster)}/policy-imports`,
      { method: 'POST', body: JSON.stringify(body) },
    ),

  deleteImport: (cluster: string, importID: string) =>
    request<{ importId: string }>(
      `/api/v1/clusters/${encodeURIComponent(cluster)}/policy-imports/${encodeURIComponent(importID)}`,
      { method: 'DELETE' },
    ),

  topology: (cluster: string, level: TopologyLevel = 'namespace') =>
    request<Topology>(
      `/api/v1/clusters/${encodeURIComponent(cluster)}/topology?level=${level}`,
    ),

  /**
   * 最近一次资产采集的摘要。
   *
   * **这份数据不参与任何结论**（spec §5.2）：上面每一个读取端点仍然读
   * 合成数据集，这一条读的是真实集群采回来的资产。两者不相通，页面必须
   * 把这件事说给操作者听（见 pages/collectionView.ts 的
   * COLLECTION_FEEDS_NOTHING）。
   *
   * 「从未采集过」在这里被转成一种状态而不是一次错误：它是一个正常状态，
   * 而 catch 分支里的一句错误文案会让它和"读取失败"长得一样。其余错误
   * 照常抛出 —— 一次读取故障不能被伪装成"还没采过"。
   *
   * 服务端声明 accessAdmin，只读账号一律 403（规范 §34）。
   */
  collection: async (cluster: string): Promise<CollectionState> => {
    try {
      const summary = await request<CollectionSummary>(
        `/api/v1/clusters/${encodeURIComponent(cluster)}/collection`,
      )
      return { kind: 'RUN', summary }
    } catch (e) {
      if (isNoRunError(e)) return { kind: 'NO_RUN' }
      if (isUnknownClusterError(e)) return { kind: 'UNKNOWN_CLUSTER' }
      throw e
    }
  },

  quality: (cluster: string) =>
    request<Quality>(`/api/v1/clusters/${encodeURIComponent(cluster)}/quality`),
  security: (cluster: string) =>
    request<SecurityReport>(`/api/v1/clusters/${encodeURIComponent(cluster)}/security`),

  flows: (filter: FlowFilter = {}) =>
    request<FlowPage>(`/api/v1/flows${query(filter)}`),

  decision: (flowID: string) =>
    request<Decision>(`/api/v1/flows/${encodeURIComponent(flowID)}/decision`),

  /**
   * 最近一次流量摄入。
   *
   * 从未摄入过时服务端答 CodeNoIngestRun —— 调用方据此显示"从未"，
   * 而不是"没有流量"。两者的处置相反。
   */
  flowIngest: (cluster: string) =>
    request<IngestSummary>(`/api/v1/clusters/${encodeURIComponent(cluster)}/flow-ingest`),

  policyPreview: (cluster: string, namespace?: string, granularity?: Granularity) => {
    const p = new URLSearchParams()
    if (namespace) p.set('namespace', namespace)
    // 小写：后端 parseGranularity 大小写不敏感，但查询参数沿用拓扑那套
    // 小写词汇（?level=namespace），两处保持一致。
    if (granularity === 'NAMESPACE') p.set('granularity', 'namespace')
    const q = p.toString()
    return request<PolicyPreview>(
      `/api/v1/clusters/${encodeURIComponent(cluster)}/policy-preview${q ? `?${q}` : ''}`,
    )
  },

  /**
   * 下载一次已确认策略导出。
   *
   * 收一个已经拼好的路径而不是自己再拼一次：路径必须来自
   * `policyExportPath`，且时间窗必须是页面此刻正在显示的那一个
   * （见 pages/policyExportView.ts）。在这里重拼就又多了一个可以与
   * 页面分歧的取数点，而分歧的后果是文件与操作者刚读过的四个数字
   * 描述两次不同的计算（design doc 2026-08-14 §2）。
   *
   * 响应体原样带回，不解析、不重排、不重建注释头：文件必须是服务端
   * 产出的那一份，逐字节相同，否则"它与预测同源"这个保证就没了。
   */
  policyExport: async (path: string): Promise<PolicyExportFile> => {
    // 只接受本平台自己的 API 路径。取值目前恒来自 policyExportPath，
    // 这一行是纵深防御：一个能指向任意 URL 的下载函数，早晚会被某次
    // 改动接上一个来自响应体的地址。
    if (!path.startsWith('/api/v1/')) {
      throw new ApiError(-1, '导出路径不合法', 0)
    }

    const res = await fetch(path, { headers: { Accept: 'text/yaml' } })
    if (res.status === 401) {
      onUnauthorizedCb?.()
    }

    // 失败一律是 JSON 包络（response.write 固定 application/json），
    // 成功是 text/yaml。**服务端的拒绝是权威的**：界面上那两处禁用只是
    // 提前把原因说出来，不是这条判断的替代（规范 §34）。
    if ((res.headers.get('Content-Type') ?? '').includes('application/json')) {
      let body: Envelope<null>
      try {
        body = await res.json()
      } catch {
        throw new ApiError(-1, '服务无响应', res.status)
      }
      throw new ApiError(body.code, body.msg || '导出失败', res.status)
    }
    if (!res.ok) {
      throw new ApiError(-1, '导出失败', res.status)
    }
    return { blob: await res.blob(), filename: exportFilename(res.headers.get('Content-Disposition')) }
  },

  /**
   * 出一份写回计划。**写不了任何东西**：这个端点是写回的 dry-run，也是它的
   * 默认形态（design doc 2026-08-14 §5）。
   *
   * 收一个已经拼好的路径，理由同 policyExport：路径必须来自
   * `writebackView`，且时间窗必须是页面此刻正在显示的那一段。在这里重拼就
   * 又多了一个可以与页面分歧的取数点，而这里分歧的后果比导出更硬——推送时
   * 服务端会拿请求里的时间窗重算整份计划再比指纹。
   */
  policyWritebackPlan: (path: string) =>
    request<WritebackPlanResult>(assertApiPath(path), { method: 'POST' }),

  /**
   * 把操作者确认过的那份计划推到一条新分支上。
   *
   * 请求体原样收 `writebackPushBody` 的产出，**这一层不拼、不补、不改**：
   * 里面只有分支名与指纹两项，且都必须逐字来自操作者刚看过的那份计划。
   * 在这里补一个字段（哪怕是"顺手带上计数"）就等于让写回请求自述影响面，
   * 而影响面必须由平台在写前重算（§4）。
   *
   * 服务端的拒绝是权威的：不带指纹拒、指纹对不上拒、仓库级校验不是 OK 拒、
   * 非管理员拒（规范 §34）。调用方要做的是把 msg 原样展示，不收窄成一句
   * "推送失败"——"这份计划过期了"与"服务出错了"的下一步动作完全相反。
   */
  policyWritebackPush: (path: string, body: { branch: string; fingerprint: string }) =>
    request<WritebackPushResult>(assertApiPath(path), {
      method: 'POST',
      body: JSON.stringify(body),
    }),

  createOverride: (
    cluster: string,
    body: {
      namespace: string; workload: string; fingerprint: string
      decision: OverrideDecision; reason: string
    },
  ) =>
    request<{ fingerprint: string }>(
      `/api/v1/clusters/${encodeURIComponent(cluster)}/rule-overrides`,
      { method: 'POST', body: JSON.stringify(body) },
    ),

  deleteOverride: (
    cluster: string, namespace: string, workload: string, fingerprint: string,
  ) => {
    const p = new URLSearchParams({ namespace, workload, fingerprint })
    return request<{ fingerprint: string }>(
      `/api/v1/clusters/${encodeURIComponent(cluster)}/rule-overrides?${p}`,
      { method: 'DELETE' },
    )
  },
}
