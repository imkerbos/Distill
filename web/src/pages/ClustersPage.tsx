import { useState, type CSSProperties, type FormEvent, type ReactNode } from 'react'
import { api, ApiError } from '../api/client'
import {
  IMPORT_ROLE_LABEL, IMPORT_SOURCE_LABEL, ONBOARD_STATE_LABEL,
  type APIServer, type ImportRole, type ImportSource, type PolicyImportItem,
  type RegisteredCluster,
} from '../api/types'
import { useResource } from '../api/useResource'
import { Card, Chip, EmptyState, Field, PageHeader, Section, Select, TableCard } from '../components/ui'

/**
 * 集群管理页：注册、下线、Git 绑定展示、策略导入。
 *
 * 三节共用同一个 refreshKey——注册/下线/导入/删除任一操作成功后自增，
 * 驱动集群列表与导入清单重新拉取。不各自维护一套本地状态叠加服务端
 * 响应：服务端是唯一真相源（接入状态尤其如此，由服务端推进，本页
 * 任何表单字段都不能影响它）。
 */
export default function ClustersPage() {
  const [refreshKey, setRefreshKey] = useState(0)
  const bump = () => setRefreshKey((k) => k + 1)

  const { data: clusters, error, loading } = useResource(
    `clusters:${refreshKey}`,
    () => api.clusters(),
  )

  return (
    <div>
      <PageHeader
        title="集群管理"
        description="登记新集群、查看 GitOps 绑定、导入已有 NetworkPolicy。接入状态（已登记/学习中/可产出候选策略）完全由服务端根据实际采集到的数据推进，本页任何表单都无法直接指定它。"
      />

      <ClusterListSection clusters={clusters} error={error} loading={loading} onChanged={bump} />
      <RegisterSection onCreated={bump} />
      <ImportSection clusters={clusters ?? []} refreshKey={refreshKey} onChanged={bump} />
    </div>
  )
}

/* ---------------------------------------------------------------------- */
/* 1. 已注册集群                                                           */
/* ---------------------------------------------------------------------- */

function ClusterListSection({ clusters, error, loading, onChanged }: {
  clusters: RegisteredCluster[] | null
  error: string
  loading: boolean
  onChanged: () => void
}) {
  const [busyId, setBusyId] = useState<string | null>(null)

  async function offboard(id: string) {
    if (!window.confirm(`确认下线集群 ${id}？`)) return
    setBusyId(id)
    try {
      await api.deleteCluster(id)
      onChanged()
    } catch (err) {
      window.alert(err instanceof ApiError ? err.msg : '下线失败，请稍后重试')
    } finally {
      setBusyId(null)
    }
  }

  return (
    <Section
      title="已注册集群"
      description="Git 绑定为空时显式写「未绑定」——空单元格会被读成「加载中」或「未知」，两者都不是这里想表达的事实。"
      meta={clusters ? `${clusters.length} 个` : undefined}
    >
      {error ? (
        <p style={{ color: 'var(--verdict-deny)' }}>{error}</p>
      ) : loading || !clusters ? (
        <p style={{ color: 'var(--text-muted)' }}>加载中…</p>
      ) : clusters.length === 0 ? (
        <EmptyState message="尚未注册任何集群。" detail="使用下方表单登记第一个集群。" />
      ) : (
        <TableCard>
          <thead>
            <tr>
              <th>ID</th>
              <th>显示名</th>
              <th>Pod 网段</th>
              <th>Node 网段</th>
              <th>apiserver</th>
              <th>接入状态</th>
              <th>Git 绑定</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            {clusters.map((c) => (
              <tr key={c.id}>
                <td className="mono">{c.id}</td>
                <td>{c.displayName}</td>
                <td className="mono">{c.podCidr}</td>
                <td className="mono">{c.nodeCidr}</td>
                <td><ApiServerList servers={c.apiServers} /></td>
                <td><Chip strong={c.state === 'READY'}>{ONBOARD_STATE_LABEL[c.state]}</Chip></td>
                <td>
                  {c.git
                    ? (
                      <span className="mono" style={{ fontSize: 'var(--text-sm)' }}>
                        {c.git.repoUrl}@{c.git.branch}
                      </span>
                    )
                    : <span style={{ color: 'var(--text-muted)' }}>未绑定</span>}
                </td>
                <td>
                  <button
                    onClick={() => offboard(c.id)}
                    disabled={busyId === c.id}
                    style={buttonStyle}
                  >
                    {busyId === c.id ? '下线中…' : '下线'}
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </TableCard>
      )}
    </Section>
  )
}

/* ---------------------------------------------------------------------- */
/* 2. 注册新集群                                                           */
/* ---------------------------------------------------------------------- */

/** apiserver 表单行的本地形态：port 保持字符串，交给用户在提交前编辑；提交时才转数字。 */
interface ApiServerRow { host: string; cidr: string; port: string }
const emptyApiServerRow = (): ApiServerRow => ({ host: '', cidr: '', port: '443' })

/**
 * Git 绑定四个字段在表单里的合法组合只有两种：全空，或 repoUrl/branch/
 * policyPath 三项全填（credentialRef 可选，但只要它非空就已经表达了
 * "这是一处真实绑定"的意图，此时同样要求三项必填齐全）——否则
 * credentialRef 会在三项检查之外被静默丢弃，成为唯一录入了值却从不
 * 出现在提交请求里的字段。
 */
const REQUIRED_GIT_FIELDS: readonly ['repoUrl', 'branch', 'policyPath'] = ['repoUrl', 'branch', 'policyPath']

function RegisterSection({ onCreated }: { onCreated: () => void }) {
  const [id, setId] = useState('')
  const [displayName, setDisplayName] = useState('')
  const [podCidr, setPodCidr] = useState('')
  const [nodeCidr, setNodeCidr] = useState('')
  const [apiServerRows, setApiServerRows] = useState<ApiServerRow[]>([emptyApiServerRow()])
  const [healthChecks, setHealthChecks] = useState('')
  const [repoUrl, setRepoUrl] = useState('')
  const [branch, setBranch] = useState('')
  const [policyPath, setPolicyPath] = useState('')
  const [credentialRef, setCredentialRef] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  function updateApiServerRow(i: number, patch: Partial<ApiServerRow>) {
    setApiServerRows((rows) => rows.map((r, idx) => (idx === i ? { ...r, ...patch } : r)))
  }
  function addApiServerRow() {
    setApiServerRows((rows) => [...rows, emptyApiServerRow()])
  }
  function removeApiServerRow(i: number) {
    setApiServerRows((rows) => (rows.length <= 1 ? rows : rows.filter((_, idx) => idx !== i)))
  }

  async function submit(e: FormEvent) {
    e.preventDefault()
    setError('')

    const gitValues: Record<'repoUrl' | 'branch' | 'policyPath' | 'credentialRef', string> = {
      repoUrl, branch, policyPath, credentialRef,
    }
    const anyGitFilled = Object.values(gitValues).some((v) => v.trim() !== '')
    const missingGit = REQUIRED_GIT_FIELDS.filter((k) => gitValues[k].trim() === '')
    if (anyGitFilled && missingGit.length > 0) {
      setError(
        `Git 绑定缺少：${missingGit.join('、')}。repoUrl / branch / policyPath 三项在你填写了 `
        + `Git 绑定的任意一项（含 credentialRef）时都是必需的，否则已填的值不会被保存。`,
      )
      return
    }

    const apiServers: APIServer[] = apiServerRows
      .filter((r) => r.host.trim() !== '')
      .map((r) => ({ host: r.host.trim(), cidr: r.cidr.trim(), port: Number(r.port) || 0 }))
    const healthCheckSources = healthChecks
      .split('\n')
      .map((s) => s.trim())
      .filter(Boolean)

    setBusy(true)
    try {
      await api.createCluster({
        id: id.trim(),
        displayName: displayName.trim(),
        podCidr: podCidr.trim(),
        nodeCidr: nodeCidr.trim(),
        apiServers,
        healthCheckSources,
        ...(anyGitFilled
          ? {
            git: {
              repoUrl: repoUrl.trim(),
              branch: branch.trim(),
              policyPath: policyPath.trim(),
              credentialRef: credentialRef.trim(),
              lastWrittenCommit: '',
            },
          }
          : {}),
      })
      setId(''); setDisplayName(''); setPodCidr(''); setNodeCidr('')
      setApiServerRows([emptyApiServerRow()]); setHealthChecks('')
      setRepoUrl(''); setBranch(''); setPolicyPath(''); setCredentialRef('')
      onCreated()
    } catch (err) {
      // 后端把校验失败的具体字段写进 msg（比如「podCIDR "10.20.0/14" 不是
      // 合法网段」）——原样展示，不要收窄成一句「请求参数不合法」，
      // 否则运维猜不出四类网段里到底是哪一类写错了。
      setError(err instanceof ApiError ? err.msg : '注册失败，请稍后重试')
    } finally {
      setBusy(false)
    }
  }

  return (
    <Section
      title="注册新集群"
      description="仅登记元数据；接入状态从「已登记」起步，不受本表单任何字段影响，包括你在这里可能填的任何值。"
    >
      <Card style={{ padding: 'var(--space-4)' }}>
        <form onSubmit={submit}>
          <FormGrid>
            <TextField label="集群 ID" value={id} onChange={setId} required />
            <TextField label="显示名" value={displayName} onChange={setDisplayName} required />
            <TextField label="Pod CIDR" value={podCidr} onChange={setPodCidr} required mono />
            <TextField label="Node CIDR" value={nodeCidr} onChange={setNodeCidr} required mono />
          </FormGrid>

          <SubHeading>
            apiserver（可选，可添加多个 —— HA 控制面通常不止一个端点，
            漏填一个就是漏了一条 baseline 放行规则，后果是生产阻断而不是注册报错）
          </SubHeading>
          {apiServerRows.map((row, i) => (
            <div key={i} style={{
              display: 'flex', gap: 'var(--space-3)', alignItems: 'flex-end',
              marginBottom: 'var(--space-2)',
            }}>
              <div style={{ flex: 1 }}>
                <TextField label="host" value={row.host} onChange={(v) => updateApiServerRow(i, { host: v })} mono />
              </div>
              <div style={{ flex: 1 }}>
                <TextField label="cidr" value={row.cidr} onChange={(v) => updateApiServerRow(i, { cidr: v })} mono />
              </div>
              <div style={{ width: 110 }}>
                <TextField label="port" value={row.port} onChange={(v) => updateApiServerRow(i, { port: v })} mono />
              </div>
              <button
                type="button"
                onClick={() => removeApiServerRow(i)}
                disabled={apiServerRows.length <= 1}
                style={{
                  ...secondaryButtonStyle,
                  opacity: apiServerRows.length <= 1 ? 0.5 : 1,
                  cursor: apiServerRows.length <= 1 ? 'default' : 'pointer',
                }}
              >
                删除
              </button>
            </div>
          ))}
          <button type="button" onClick={addApiServerRow} style={secondaryButtonStyle}>
            + 添加 apiserver
          </button>

          <div style={{ marginTop: 'var(--space-4)' }}>
            <SubHeading>健康检查网段（可选，每行一个 CIDR）</SubHeading>
            <textarea
              className="ctl"
              value={healthChecks}
              onChange={(e) => setHealthChecks(e.target.value)}
              rows={3}
              style={textareaStyle}
            />
          </div>

          <div style={{ marginTop: 'var(--space-3)' }}>
            <SubHeading>
              Git 绑定（可选；一旦填写任意一项——含 credentialRef——repoUrl / branch / policyPath 三项均为必填）
            </SubHeading>
            <FormGrid>
              <TextField label="repoUrl" value={repoUrl} onChange={setRepoUrl} mono />
              <TextField label="branch" value={branch} onChange={setBranch} mono />
              <TextField label="policyPath" value={policyPath} onChange={setPolicyPath} mono />
              <TextField label="credentialRef" value={credentialRef} onChange={setCredentialRef} mono />
            </FormGrid>
          </div>

          {error && <FormError>{error}</FormError>}

          <button type="submit" disabled={busy} style={{ ...buttonStyle, marginTop: 'var(--space-3)' }}>
            {busy ? '提交中…' : '注册集群'}
          </button>
        </form>
      </Card>
    </Section>
  )
}

/* ---------------------------------------------------------------------- */
/* 3. 策略导入                                                             */
/* ---------------------------------------------------------------------- */

function ImportSection({ clusters, refreshKey, onChanged }: {
  clusters: RegisteredCluster[]
  refreshKey: number
  onChanged: () => void
}) {
  const [selected, setSelected] = useState('')
  const [role, setRole] = useState<ImportRole>('BASELINE_CURRENT')
  const [source, setSource] = useState<ImportSource>('PASTE')
  const [yaml, setYaml] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  const importsKey = selected ? `imports:${selected}:${refreshKey}` : ''
  const { data: imports, error: listError, loading } = useResource(
    importsKey,
    () => api.policyImports(selected),
  )

  async function submit(e: FormEvent) {
    e.preventDefault()
    setError('')
    if (!selected) {
      setError('请先选择集群。')
      return
    }
    setBusy(true)
    try {
      await api.createImport(selected, { role, source, yaml })
      setYaml('')
      onChanged()
    } catch (err) {
      setError(err instanceof ApiError ? err.msg : '导入失败，请稍后重试')
    } finally {
      setBusy(false)
    }
  }

  async function remove(importId: string) {
    if (!selected) return
    if (!window.confirm('确认删除该导入记录？')) return
    try {
      await api.deleteImport(selected, importId)
      onChanged()
    } catch (err) {
      window.alert(err instanceof ApiError ? err.msg : '删除失败，请稍后重试')
    }
  }

  return (
    <Section
      title="策略导入"
      description="导入既有 NetworkPolicy YAML，作为现状基线或候选补充。本表单提交的记录一律标注「未经 Git 核对」——只有经漂移检测流水线比对过 commit 的记录才会显示已核对，该流水线不在本轮范围内。"
    >
      <Card style={{ padding: 'var(--space-4)', marginBottom: 'var(--space-3)' }}>
        <form onSubmit={submit}>
          <FormGrid>
            <label style={{ display: 'block' }}>
              <span style={fieldLabelStyle}>集群</span>
              <Select
                value={selected}
                ariaLabel="导入目标集群"
                onChange={setSelected}
                options={[
                  ['', '请选择集群'],
                  ...clusters.map((c) => [c.id, `${c.id}（${ONBOARD_STATE_LABEL[c.state]}）`] as [string, string]),
                ]}
                style={{ width: '100%' }}
              />
            </label>
            <Field label="角色">
              <Select
                value={role}
                ariaLabel="导入角色"
                onChange={(v) => setRole(v as ImportRole)}
                options={Object.entries(IMPORT_ROLE_LABEL) as [string, string][]}
              />
            </Field>
            <Field label="来源">
              <Select
                value={source}
                ariaLabel="导入来源"
                onChange={(v) => setSource(v as ImportSource)}
                options={Object.entries(IMPORT_SOURCE_LABEL) as [string, string][]}
              />
            </Field>
          </FormGrid>

          <SubHeading>NetworkPolicy YAML</SubHeading>
          <textarea
            className="ctl"
            value={yaml}
            onChange={(e) => setYaml(e.target.value)}
            rows={10}
            required
            style={textareaStyle}
          />

          {error && <FormError>{error}</FormError>}

          <button type="submit" disabled={busy} style={{ ...buttonStyle, marginTop: 'var(--space-3)' }}>
            {busy ? '提交中…' : '提交导入'}
          </button>
        </form>
      </Card>

      {!selected ? (
        <EmptyState message="选择一个集群以查看它的导入清单。" detail="清单按集群独立维护，不跨集群合并展示。" />
      ) : listError ? (
        <p style={{ color: 'var(--verdict-deny)' }}>{listError}</p>
      ) : loading || !imports ? (
        <p style={{ color: 'var(--text-muted)' }}>加载中…</p>
      ) : imports.length === 0 ? (
        <EmptyState message={`${selected} 尚无已导入的策略。`} detail="使用上方表单导入第一条。" />
      ) : (
        <TableCard>
          <thead>
            <tr>
              <th>namespace/name</th>
              <th>角色</th>
              <th>来源</th>
              <th>导入人</th>
              <th>导入时间</th>
              <th>Git 核对</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            {imports.map((it) => (
              <tr key={it.importId}>
                <td className="mono" style={{ fontSize: 'var(--text-sm)' }}>{it.namespace}/{it.name}</td>
                <td><Chip>{IMPORT_ROLE_LABEL[it.role] ?? it.role}</Chip></td>
                <td><Chip>{IMPORT_SOURCE_LABEL[it.source] ?? it.source}</Chip></td>
                <td>{it.importedBy}</td>
                <td className="mono" style={{ fontSize: 'var(--text-xs)' }}>{formatTime(it.importedAt)}</td>
                <td><GitVerifiedMark item={it} /></td>
                <td>
                  <button onClick={() => remove(it.importId)} style={buttonStyle}>删除</button>
                </td>
              </tr>
            ))}
          </tbody>
        </TableCard>
      )}
    </Section>
  )
}

/**
 * 「未经 Git 核对」是对"当前状态可信程度"的事实陈述，不是次要信息——
 * 不得用灰字弱化，与 VerdictBadge 里 DEGRADED 用描边而非灰化是同一条
 * 纪律。已核对时展示 commit 前 7 位，使结论可核验而不是空喊一句"已核对"。
 */
function GitVerifiedMark({ item }: { item: PolicyImportItem }) {
  const verified = item.source === 'GIT' && item.gitCommitSha !== ''
  if (verified) {
    return <Chip strong>已用 Git 核对 · {item.gitCommitSha.slice(0, 7)}</Chip>
  }
  return (
    <span style={{
      display: 'inline-flex', alignItems: 'center', padding: '2px 8px',
      fontSize: 'var(--text-xs)', fontWeight: 500, borderRadius: 999,
      color: 'var(--verdict-unknown)',
      border: 'var(--degraded-stroke-width) solid var(--verdict-unknown)',
    }}>
      未经 Git 核对
    </span>
  )
}

/**
 * apiserver 列表：渲染全部条目而不是只取第一个。HA 控制面通常有多个
 * apiserver 端点，只展示一个会让运维误以为集群"已完整登记"，而平台
 * 实际只认识其中一个——baseline 推导依赖这份清单的完整性，漏一条
 * 后果是漏一条放行规则，事后表现为生产阻断而不是注册时的报错。
 */
function ApiServerList({ servers }: { servers?: APIServer[] | null }) {
  if (!servers || servers.length === 0) {
    return <span style={{ color: 'var(--text-muted)' }}>未配置</span>
  }
  return (
    <span className="mono" style={{
      display: 'flex', flexDirection: 'column', gap: 2, fontSize: 'var(--text-xs)',
    }}>
      {servers.map((s, i) => <span key={`${i}-${s.host}`}>{s.host}:{s.port}（{s.cidr}）</span>)}
    </span>
  )
}

/* ---------------------------------------------------------------------- */
/* 共享小件                                                                 */
/* ---------------------------------------------------------------------- */

function formatTime(iso: string): string {
  return new Date(iso).toISOString().replace('T', ' ').replace(/\.\d+Z$/, ' UTC')
}

function FormGrid({ children }: { children: ReactNode }) {
  return (
    <div style={{
      display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(200px, 1fr))',
      gap: 'var(--space-3)', marginBottom: 'var(--space-3)',
    }}>
      {children}
    </div>
  )
}

function SubHeading({ children }: { children: ReactNode }) {
  return (
    <div style={{
      fontSize: 'var(--text-xs)', color: 'var(--text-muted)',
      marginBottom: 'var(--space-2)',
    }}>
      {children}
    </div>
  )
}

const fieldLabelStyle: CSSProperties = {
  display: 'block', marginBottom: 4, fontSize: 'var(--text-xs)', color: 'var(--text-muted)',
}

function TextField({ label, value, onChange, required, mono }: {
  label: string
  value: string
  onChange: (v: string) => void
  required?: boolean
  mono?: boolean
}) {
  return (
    <label style={{ display: 'block' }}>
      <span style={fieldLabelStyle}>{label}</span>
      <input
        className="ctl"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        required={required}
        style={{ width: '100%', fontFamily: mono ? 'var(--mono)' : undefined }}
      />
    </label>
  )
}

function FormError({ children }: { children: ReactNode }) {
  return (
    <p role="alert" style={{
      margin: 'var(--space-3) 0 0', padding: 'var(--space-2)',
      background: 'var(--verdict-deny-bg)', color: 'var(--verdict-deny)',
      borderRadius: 'var(--radius)', fontSize: 'var(--text-sm)',
    }}>
      {children}
    </p>
  )
}

const textareaStyle: CSSProperties = {
  width: '100%', height: 'auto', fontFamily: 'var(--mono)',
  fontSize: 'var(--text-sm)', padding: 'var(--space-2)', resize: 'vertical',
}

const buttonStyle: CSSProperties = {
  padding: '6px 14px', fontSize: 'var(--text-sm)', fontWeight: 500,
  color: 'var(--text-on-dark)', background: 'var(--accent)',
  border: 'none', borderRadius: 'var(--radius-sm)', cursor: 'pointer',
}

const secondaryButtonStyle: CSSProperties = {
  padding: '6px 12px', fontSize: 'var(--text-sm)', fontWeight: 500,
  color: 'var(--text)', background: 'var(--surface)',
  border: '1px solid var(--border-strong)', borderRadius: 'var(--radius-sm)', cursor: 'pointer',
}
