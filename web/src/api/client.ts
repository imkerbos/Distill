import type {
  ClusterSummary, Decision, Envelope, FlowFilter, FlowPage,
  Identity, PolicyPreview, Quality, SecurityReport, Topology, TopologyLevel,
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

  clusters: () => request<ClusterSummary[]>('/api/v1/clusters'),

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
}
