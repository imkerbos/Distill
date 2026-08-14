import type {
  ClusterWrite, Decision, Envelope, FlowFilter, FlowPage, GitBindingWrite,
  GitRepo, GitRepoWrite, Identity, ImportRole, ImportSource, OverrideDecision,
  PathVerifyStatus, PlatformSettingView, PlatformSettingWrite, PolicyImportItem, PolicyPreview,
  Quality, RegisteredCluster, RepoVerifyStatus, SecurityReport, Topology, TopologyLevel,
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

export const api = {
  login: (username: string, password: string) =>
    request<Identity>('/api/v1/sessions', {
      method: 'POST',
      body: JSON.stringify({ username, password }),
    }),

  logout: () => request<null>('/api/v1/sessions/current', { method: 'DELETE' }),

  me: () => request<Identity>('/api/v1/sessions/current'),

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

  quality: (cluster: string) =>
    request<Quality>(`/api/v1/clusters/${encodeURIComponent(cluster)}/quality`),
  security: (cluster: string) =>
    request<SecurityReport>(`/api/v1/clusters/${encodeURIComponent(cluster)}/security`),

  flows: (filter: FlowFilter = {}) =>
    request<FlowPage>(`/api/v1/flows${query(filter)}`),

  decision: (flowID: string) =>
    request<Decision>(`/api/v1/flows/${encodeURIComponent(flowID)}/decision`),

  policyPreview: (cluster: string, namespace?: string) => {
    const p = new URLSearchParams()
    if (namespace) p.set('namespace', namespace)
    const q = p.toString()
    return request<PolicyPreview>(
      `/api/v1/clusters/${encodeURIComponent(cluster)}/policy-preview${q ? `?${q}` : ''}`,
    )
  },

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
