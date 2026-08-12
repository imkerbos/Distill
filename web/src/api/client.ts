import type {
  ClusterWrite, Decision, Envelope, FlowFilter, FlowPage, GitBindingWrite,
  Identity, ImportRole, ImportSource, OverrideDecision, PolicyImportItem, PolicyPreview,
  Quality, RegisteredCluster, SecurityReport, Topology, TopologyLevel, VerifyStatus,
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

  // 绑定或改绑一个集群的策略仓库。
  //
  // 独立端点而不是集群 PUT 的一部分：绑定有自己的生命周期与自己的审计
  // 动作（design doc 2026-08-13 §5）。走集群写路径的后果不是多发一次
  // 请求，而是一次只想改网段的编辑顺手重写了绑定——服务端的 clusterPayload
  // 现在根本不收 git，那样的请求会成功返回而绑定原封不动。
  //
  // 返回的是校验结论而不是「已保存」：保存会顺带跑一次只读校验，形状与
  // 重校验端点一致，两处显示的是同一套结论。
  bindGitRepo: (cluster: string, body: GitBindingWrite) =>
    request<VerifyStatus>(`/api/v1/clusters/${encodeURIComponent(cluster)}/git-binding`, {
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

  // 手动重校验。没有请求体：要校验的是库里那个绑定，不是调用方随手给的
  // 一份——能由请求体指定校验目标，就等于允许拿一个能通过的仓库地址去
  // 换取另一个绑定的 OK。存在的理由是凭据轮换与权限修复之后需要一个新鲜
  // 结论（design doc §3.3）；平台不做后台定时校验。
  verifyGitBinding: (cluster: string) =>
    request<VerifyStatus>(
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
