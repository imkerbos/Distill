import { Fragment, useState, type CSSProperties, type FormEvent, type ReactNode } from 'react'
import { api, ApiError } from '../api/client'
import {
  IMPORT_ROLE_LABEL, IMPORT_SOURCE_LABEL, ONBOARD_STATE_LABEL,
  type APIServer, type GitBinding, type ImportRole, type ImportSource,
  type PolicyImportItem, type RegisteredCluster,
} from '../api/types'
import { useResource } from '../api/useResource'
import {
  blankFormValues, buildClusterWrite, describeVerifyStatus, emptyApiServerRow, formatUtcTime,
  formValuesOf, resolveGitBinding,
  type ApiServerRow, type ClusterFormValues, type VerifyStatusView, type VerifyTone,
} from './clusterForm'
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
  const [editingId, setEditingId] = useState<string | null>(null)

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
      description="Git 绑定为空时显式写「未绑定」——空单元格会被读成「加载中」或「未知」，两者都不是这里想表达的事实；同一条理由，没校验过的绑定写「未校验」而不是留白。校验只做只读查询：它能确认仓库可达、认证通过、分支与路径存在，但它从不向仓库提交任何内容，因此证明不了平台能往那个路径提交——那要等真正提交一次才知道。「编辑」在行内展开，用于补上或改动登记信息（含 Git 绑定）；保存是整体替换，表单已按现值预填。"
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
              <th>CCNP</th>
              <th>Git 绑定</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            {clusters.map((c) => (
              // Fragment 而非单个 <tr>：编辑面板作为紧随其后的整宽行展开，
              // 与被编辑的那一行同处一张表，操作者不必在弹层与表格之间
              // 比对自己正在改哪个集群。
              <Fragment key={c.id}>
                <tr>
                  <td className="mono">{c.id}</td>
                  <td>{c.displayName}</td>
                  <td className="mono">{c.podCidr}</td>
                  <td className="mono">{c.nodeCidr}</td>
                  <td><ApiServerList servers={c.apiServers} /></td>
                  <td><Chip strong={c.state === 'READY'}>{ONBOARD_STATE_LABEL[c.state]}</Chip></td>
                  {/*
                    这一格存在的理由与「未绑定」「未校验」同一条：一个看不见
                    的降级理由，等于操作者无法解释他看到的判定，也无法察觉
                    一次编辑把它清掉了。
                  */}
                  <td><CCNPMark present={c.ccnpPresent} /></td>
                  <td>
                    {c.git
                      ? <GitBindingCell clusterId={c.id} git={c.git} onChanged={onChanged} />
                      : <span style={{ color: 'var(--text-muted)' }}>未绑定</span>}
                  </td>
                  <td>
                    <div style={{ display: 'flex', gap: 'var(--space-2)' }}>
                      <button
                        onClick={() => setEditingId(editingId === c.id ? null : c.id)}
                        style={secondaryButtonStyle}
                      >
                        {editingId === c.id ? '收起' : '编辑'}
                      </button>
                      <button
                        onClick={() => offboard(c.id)}
                        disabled={busyId === c.id}
                        style={buttonStyle}
                      >
                        {busyId === c.id ? '下线中…' : '下线'}
                      </button>
                    </div>
                  </td>
                </tr>
                {editingId === c.id && (
                  <tr>
                    <td colSpan={9} style={{ background: 'var(--surface-sunken)' }}>
                      {/*
                        key 绑定集群 ID：换一个集群展开时必须重新播种，
                        复用同一份表单状态会把上一个集群的网段带进来。
                      */}
                      <EditClusterForm
                        key={c.id}
                        cluster={c}
                        onCancel={() => setEditingId(null)}
                        onSaved={() => { setEditingId(null); onChanged() }}
                      />
                    </td>
                  </tr>
                )}
              </Fragment>
            ))}
          </tbody>
        </TableCard>
      )}
    </Section>
  )
}

/**
 * Git 绑定这一格：仓库指向、校验结论、校验时刻、重新校验。
 *
 * 四样东西挤在同一格而不是拆成四列，是因为它们说的是同一件事的四个侧面
 * ——一个「只读校验通过」脱离了它指向的仓库和它发生的时刻就没有意义。
 *
 * 结论与时刻一律显示，不做「没问题就不说话」的省略：一格空白在这张表里
 * 会被读成「没什么要报告的」，而「从未校验过」与「校验通过」是相反的两
 * 件事实（同本节 description 里「未绑定」的理由）。
 */
function GitBindingCell({ clusterId, git, onChanged }: {
  clusterId: string
  git: GitBinding
  onChanged: () => void
}) {
  const [busy, setBusy] = useState(false)
  const view = describeVerifyStatus(git)

  async function reverify() {
    setBusy(true)
    try {
      await api.verifyGitBinding(clusterId)
      // 响应里的结论不就地贴进这一行：服务端是唯一真相源，重新拉一次
      // 列表（同本页 refreshKey 的纪律）。就地拼接会让这一行显示出一份
      // 由前端合成、没人能核对的状态。
      onChanged()
    } catch (err) {
      // 未绑定的集群这个端点回 404，但这一格只在已绑定时渲染，所以真正
      // 会撞上的是校验本身出错。原样展示后端的 msg，不收窄成一句「失败」。
      window.alert(err instanceof ApiError ? err.msg : '重新校验失败，请稍后重试')
    } finally {
      setBusy(false)
    }
  }

  return (
    <div style={{
      display: 'flex', flexDirection: 'column', alignItems: 'flex-start',
      gap: 'var(--space-1)', maxWidth: 340,
    }}>
      <span className="mono" style={{ fontSize: 'var(--text-sm)' }}>
        {git.repoUrl}@{git.branch}
      </span>
      <VerifyBadge view={view} />
      <span style={{ fontSize: 'var(--text-xs)', color: 'var(--text-muted)' }}>
        {view.checkedAt}
      </span>
      <span style={{ fontSize: 'var(--text-xs)', color: 'var(--text-secondary)' }}>
        {view.detail}
      </span>
      <button
        type="button"
        onClick={reverify}
        disabled={busy}
        style={{ ...secondaryButtonStyle, marginTop: 'var(--space-1)' }}
      >
        {busy ? '校验中…' : '重新校验（只读）'}
      </button>
    </div>
  )
}

/**
 * 校验结论的样式：三种语气三种画法。
 *
 * 借用判定语义色（本该只归 VerdictBadge）是有意为之，与同文件里
 * GitVerifiedMark 同一条理由：这里陈述的正是「这个绑定可不可信」，是
 * 判断结论而不是元信息。未校验用描边而非灰化——灰掉等于把「没查过」
 * 弱化成一句次要提示，而它恰恰是这一格里最需要被看见的事实。
 */
const VERIFY_TONE_STYLE: Record<VerifyTone, CSSProperties> = {
  ok: {
    color: 'var(--verdict-allow)', background: 'var(--verdict-allow-bg)',
    border: '1px solid var(--verdict-allow)',
  },
  bad: {
    color: 'var(--verdict-deny)', background: 'var(--verdict-deny-bg)',
    border: '1px solid var(--verdict-deny)',
  },
  unverified: {
    color: 'var(--verdict-unknown)', background: 'transparent',
    border: 'var(--degraded-stroke-width) solid var(--verdict-unknown)',
  },
}

function VerifyBadge({ view }: { view: VerifyStatusView }) {
  return (
    <span style={{
      display: 'inline-flex', alignItems: 'center', padding: '2px 8px',
      fontSize: 'var(--text-xs)', fontWeight: 500, borderRadius: 999,
      ...VERIFY_TONE_STYLE[view.tone],
    }}>
      {view.label}
    </span>
  )
}

/* ---------------------------------------------------------------------- */
/* 2. 集群表单：注册与编辑共用                                              */
/* ---------------------------------------------------------------------- */

/**
 * 集群表单的公共字段。
 *
 * 注册与编辑共用同一份字段与同一套校验（见 clusterForm.ts）：两份拷贝
 * 一定会漂移，而漂移的落点是策略下发路径——一个只在注册表单拦得住的
 * 半截 Git 绑定，会在编辑表单被放进库里。
 *
 * mode 只影响两处：集群 ID 在编辑时只读（它是全平台的身份主键，改它
 * 不是改这个集群而是指向另一个集群），以及「解除 Git 绑定」只在编辑
 * 已绑定集群时出现。接入状态两种模式下都没有控件：它由服务端根据
 * 实际采集到的数据推进，表单能提交它就等于允许把「还没有数据」标成
 * 「可以出推荐了」。
 */
function ClusterFields({ values, patch, mode, current }: {
  values: ClusterFormValues
  patch: (p: Partial<ClusterFormValues>) => void
  mode: 'create' | 'edit'
  current: GitBinding | null
}) {
  function updateApiServerRow(i: number, rowPatch: Partial<ApiServerRow>) {
    patch({
      apiServerRows: values.apiServerRows.map((r, idx) => (idx === i ? { ...r, ...rowPatch } : r)),
    })
  }
  function addApiServerRow() {
    patch({ apiServerRows: [...values.apiServerRows, emptyApiServerRow()] })
  }
  function removeApiServerRow(i: number) {
    if (values.apiServerRows.length <= 1) return
    patch({ apiServerRows: values.apiServerRows.filter((_, idx) => idx !== i) })
  }

  return (
    <>
      <FormGrid>
        <TextField
          label={mode === 'edit' ? '集群 ID（不可修改）' : '集群 ID'}
          value={values.id}
          onChange={(v) => patch({ id: v })}
          required
          readOnly={mode === 'edit'}
          mono
        />
        <TextField label="显示名" value={values.displayName} onChange={(v) => patch({ displayName: v })} required />
        <TextField label="Pod CIDR" value={values.podCidr} onChange={(v) => patch({ podCidr: v })} required mono />
        <TextField label="Node CIDR" value={values.nodeCidr} onChange={(v) => patch({ nodeCidr: v })} required mono />
      </FormGrid>

      {/*
        这一项不是技术开关，措辞也就不能写成技术开关。它声明的是「这个
        集群里还有别的东西在影响连通性」，而平台不求值 CCNP，因此凡是
        勾上的集群，回放判定一律降级为 DEGRADED——标签要说的是这个后果，
        不是「有没有装 Cilium」。

        表单里必须有它、且必须按现值预填：PUT 是整体替换，界面上不出现
        就等于每次编辑都把它清成 false，让一个本该降级的集群显示成正常
        判定。这是"看上去更有把握"的方向，也是最难被发现的方向。
      */}
      <label style={{
        display: 'flex', alignItems: 'flex-start', gap: 'var(--space-2)',
        fontSize: 'var(--text-sm)', marginBottom: 'var(--space-3)',
      }}>
        <input
          type="checkbox"
          checked={values.ccnpPresent}
          onChange={(e) => patch({ ccnpPresent: e.target.checked })}
          style={{ marginTop: 3 }}
        />
        <span>
          该集群存在 CiliumClusterwideNetworkPolicy
          <span style={{ display: 'block', color: 'var(--text-muted)', fontSize: 'var(--text-xs)' }}>
            勾选后，这个集群的所有回放判定一律降级为「结论不可信（DEGRADED）」——
            平台不求值 CCNP，看不见它放行或拒绝了什么，因此给不出可信的结论。
            装了却不勾，平台会用一个它其实解释不了的模型给出笃定的结论。
          </span>
        </span>
      </label>

      <SubHeading>
        apiserver（可选，可添加多个 —— HA 控制面通常不止一个端点，
        漏填一个就是漏了一条 baseline 放行规则，后果是生产阻断而不是注册报错。
        每一行要么整行留空（会被忽略），要么 host / cidr / port 三项都填）
      </SubHeading>
      {values.apiServerRows.map((row, i) => (
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
            <TextField
              label="port" value={row.port} onChange={(v) => updateApiServerRow(i, { port: v })}
              mono placeholder="443"
            />
          </div>
          <button
            type="button"
            onClick={() => removeApiServerRow(i)}
            disabled={values.apiServerRows.length <= 1}
            style={{
              ...secondaryButtonStyle,
              opacity: values.apiServerRows.length <= 1 ? 0.5 : 1,
              cursor: values.apiServerRows.length <= 1 ? 'default' : 'pointer',
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
          aria-label="健康检查网段"
          value={values.healthChecks}
          onChange={(e) => patch({ healthChecks: e.target.value })}
          rows={3}
          style={textareaStyle}
        />
      </div>

      <div style={{ marginTop: 'var(--space-3)' }}>
        <SubHeading>
          Git 绑定（可选；一旦填写任意一项——含 credentialRef——repoUrl / branch / policyPath 三项均为必填）。
          repoUrl 必须是 SSH 形态（ssh://git@host/path 或 git@host:path）：平台按仓库发放 deploy key，
          https:// 地址连拨号都不会发生，服务端会在保存时直接拒绝。
        </SubHeading>
        {/*
          「解除绑定」是一个勾选动作，不是「把四个字段清空」的推断：整体
          替换下两者提交结果相同，但一次误删输入框内容不该静默切断集群与
          策略仓库的关联。勾上后四个输入框禁用——留着可编辑会让界面同时
          显示「仓库地址」与「将解除绑定」两句互相矛盾的话。
        */}
        {mode === 'edit' && current && (
          <label style={{
            display: 'flex', alignItems: 'center', gap: 'var(--space-2)',
            fontSize: 'var(--text-sm)', marginBottom: 'var(--space-2)',
          }}>
            <input
              type="checkbox"
              checked={values.clearGit}
              onChange={(e) => patch({ clearGit: e.target.checked })}
            />
            解除 Git 绑定（该集群将不再有策略仓库，平台不会再向它写策略）
          </label>
        )}
        <FormGrid>
          <TextField
            label="repoUrl" value={values.git.repoUrl} mono disabled={values.clearGit}
            placeholder="ssh://git@gitlab.example.com/net/policies.git"
            onChange={(v) => patch({ git: { ...values.git, repoUrl: v } })}
          />
          <TextField
            label="branch" value={values.git.branch} mono disabled={values.clearGit}
            onChange={(v) => patch({ git: { ...values.git, branch: v } })}
          />
          <TextField
            label="policyPath" value={values.git.policyPath} mono disabled={values.clearGit}
            onChange={(v) => patch({ git: { ...values.git, policyPath: v } })}
          />
          <TextField
            label="credentialRef" value={values.git.credentialRef} mono disabled={values.clearGit}
            onChange={(v) => patch({ git: { ...values.git, credentialRef: v } })}
          />
        </FormGrid>
        <GitOutcome values={values} current={current} />
      </div>
    </>
  )
}

/**
 * 提交前把「这一次会把绑定改成什么」写出来。
 *
 * 绑定的三种去向（保持、改指向、解除）在表单上长得很像——都是四个
 * 输入框的不同填法。让结果只在提交后从列表里的一行文字体现，等于把
 * 「我以为我只是清空了一个字段」留到已经写进库之后才被发现。
 */
function GitOutcome({ values, current }: { values: ClusterFormValues; current: GitBinding | null }) {
  const resolution = resolveGitBinding(values.git, { current, clearRequested: values.clearGit })
  return (
    <p style={{
      margin: 'var(--space-2) 0 0', fontSize: 'var(--text-xs)',
      color: resolution.ok ? 'var(--text-secondary)' : 'var(--verdict-unknown)',
    }}>
      {resolution.ok ? resolution.summary : resolution.error}
    </p>
  )
}

function RegisterSection({ onCreated }: { onCreated: () => void }) {
  const [values, setValues] = useState<ClusterFormValues>(blankFormValues)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  const patch = (p: Partial<ClusterFormValues>) => setValues((v) => ({ ...v, ...p }))

  async function submit(e: FormEvent) {
    e.preventDefault()
    setError('')

    // 注册时没有「当前绑定」，传 null：清空即未绑定，不存在「解除」这件事。
    const built = buildClusterWrite(values, null)
    if (!built.ok) {
      setError(built.error)
      return
    }

    setBusy(true)
    try {
      await api.createCluster(built.body)
      setValues(blankFormValues())
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
      description="仅登记元数据；接入状态从「已登记」起步，不受本表单任何字段影响，包括你在这里可能填的任何值。Git 绑定可以留到之后在上方列表里补，注册时不知道仓库路径是常态。"
    >
      <Card style={{ padding: 'var(--space-4)' }}>
        <form onSubmit={submit}>
          <ClusterFields values={values} patch={patch} mode="create" current={null} />

          {error && <FormError>{error}</FormError>}

          <button type="submit" disabled={busy} style={{ ...buttonStyle, marginTop: 'var(--space-3)' }}>
            {busy ? '提交中…' : '注册集群'}
          </button>
        </form>
      </Card>
    </Section>
  )
}

/**
 * 编辑已注册集群。
 *
 * 表单从集群现值播种，一项都不能省：PUT 是整体替换，表单里空着的字段
 * 提交后就是库里空着的字段。一个只想改仓库地址的操作者不该因此丢掉
 * apiserver 清单——那少掉的是一条 control-plane 放行规则，事后表现为
 * 生产阻断，而不是提交时报错。
 *
 * 已知限制：整体替换 + 无版本号意味着两个人同时编辑同一个集群时后写
 * 覆盖先写，且双方都不会收到提示。乐观锁需要在存储层引入版本列，不在
 * 本轮范围。
 */
function EditClusterForm({ cluster, onSaved, onCancel }: {
  cluster: RegisteredCluster
  onSaved: () => void
  onCancel: () => void
}) {
  const [values, setValues] = useState<ClusterFormValues>(() => formValuesOf(cluster))
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  const current = cluster.git ?? null
  const patch = (p: Partial<ClusterFormValues>) => setValues((v) => ({ ...v, ...p }))

  async function submit(e: FormEvent) {
    e.preventDefault()
    setError('')

    const built = buildClusterWrite(values, current)
    if (!built.ok) {
      setError(built.error)
      return
    }

    setBusy(true)
    try {
      await api.updateCluster(cluster.id, built.body)
      onSaved()
    } catch (err) {
      setError(err instanceof ApiError ? err.msg : '保存失败，请稍后重试')
    } finally {
      setBusy(false)
    }
  }

  return (
    <Card style={{ padding: 'var(--space-4)', margin: 'var(--space-3) 0' }}>
      <form onSubmit={submit}>
        <SubHeading>
          编辑 {cluster.id}：提交会整体替换该集群的登记信息，表单里的每一项都会被写入，
          因此下面的值已按当前登记内容预填——清空一项就是把它从库里清空。
          接入状态不在此列，它由服务端推进。
        </SubHeading>
        <ClusterFields values={values} patch={patch} mode="edit" current={current} />

        {error && <FormError>{error}</FormError>}

        <div style={{ display: 'flex', gap: 'var(--space-2)', marginTop: 'var(--space-3)' }}>
          <button type="submit" disabled={busy} style={buttonStyle}>
            {busy ? '保存中…' : '保存修改'}
          </button>
          <button type="button" onClick={onCancel} style={secondaryButtonStyle}>取消</button>
        </div>
      </form>
    </Card>
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
  const [gitCommitSha, setGitCommitSha] = useState('')
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
    // 来源与 commit 必须互相印证，与后端 registry.ValidatePolicyImport
    // 同一条规则。在这里先拦一道不是为了省一次请求，而是因为这个约束
    // 本身要在界面上说清楚：一条没有 commit 的 GIT 记录会被读成「与仓库
    // 一致的现状」，而轮 3 的漂移检测正是拿这个 commit 做基准。
    const sha = gitCommitSha.trim()
    if (source === 'GIT' && !/^[0-9a-fA-F]{40}$/.test(sha)) {
      setError('来源选择 GIT 时必须填写 40 位完整 commit SHA —— '
        + '没有 commit 就无法证明这份 YAML 与仓库里的内容一致。')
      return
    }
    setBusy(true)
    try {
      await api.createImport(selected, {
        role, source, yaml,
        // 非 GIT 来源一律不带 commit：一个不指向任何同步动作的 commit
        // 是一句凭空的溯源声明，后端会拒绝它。
        gitCommitSha: source === 'GIT' ? sha : '',
      })
      setYaml('')
      setGitCommitSha('')
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
            {/*
              commit 字段只在来源为 GIT 时出现，且此时必填。让它常驻会诱使
              人给一份粘贴的 YAML 填个 commit —— 那是一句没有东西支撑的
              溯源声明；而 GIT 却不给填的话，界面就把「未经核对」做成了
              GIT 来源的默认结局。
            */}
            {source === 'GIT' && (
              <TextField
                label="commit SHA（40 位，必填）"
                value={gitCommitSha}
                onChange={setGitCommitSha}
                mono
                placeholder="0123456789abcdef0123456789abcdef01234567"
              />
            )}
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
                <td className="mono" style={{ fontSize: 'var(--text-xs)' }}>{formatUtcTime(it.importedAt)}</td>
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
 * CCNP 这一格：装了就说判定被降级，没装就说没有，两种都写出来。
 *
 * 不做「有才显示、没有就留空」：空单元格会被读成「不知道」，而这一项
 * 恰恰是解释判定为什么降级的那个原因。用描边而非灰化，与 VerdictBadge
 * 处理 DEGRADED 同一条纪律——它是结论的一部分，不是次要元信息。
 */
function CCNPMark({ present }: { present: boolean }) {
  if (!present) {
    return <span style={{ color: 'var(--text-muted)', fontSize: 'var(--text-xs)' }}>无</span>
  }
  return (
    <span style={{
      display: 'inline-flex', alignItems: 'center', padding: '2px 8px',
      fontSize: 'var(--text-xs)', fontWeight: 500, borderRadius: 999,
      color: 'var(--verdict-unknown)',
      border: 'var(--degraded-stroke-width) solid var(--verdict-unknown)',
    }}>
      有 · 判定降级
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

function TextField({ label, value, onChange, required, mono, placeholder, readOnly, disabled }: {
  label: string
  value: string
  onChange: (v: string) => void
  required?: boolean
  mono?: boolean
  placeholder?: string
  readOnly?: boolean
  disabled?: boolean
}) {
  const inert = readOnly || disabled
  return (
    <label style={{ display: 'block' }}>
      <span style={fieldLabelStyle}>{label}</span>
      <input
        className="ctl"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        required={required}
        placeholder={placeholder}
        readOnly={readOnly}
        disabled={disabled}
        style={{
          width: '100%',
          fontFamily: mono ? 'var(--mono)' : undefined,
          // 不可编辑的输入框必须看得出不可编辑：一个外观正常却打不进字的
          // 输入框会被读成界面卡住，而不是"这一项不能改"。
          background: inert ? 'var(--surface-sunken)' : undefined,
          color: inert ? 'var(--text-muted)' : undefined,
        }}
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
