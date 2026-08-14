import { Fragment, useState, type CSSProperties, type FormEvent, type ReactNode } from 'react'
import { api, ApiError } from '../api/client'
import type { GitRepo } from '../api/types'
import { useResource } from '../api/useResource'
import { VerifyBadge, VerifyOutcomeNote } from '../components/Verdict'
import { Card, EmptyState, PageHeader, Section, TableCard } from '../components/ui'
import {
  blankRepoValues, describeRepoVerifyOutcome, describeRepoVerifyStatus,
  repoFormValuesOf, resolveGitRepo, type RepoFormValues,
} from './gitRepoForm'
import type { VerifyOutcomeView } from './verifyView'

/**
 * 策略仓库管理页：登记、修改、删除、重新校验，展示**仓库级**结论。
 *
 * 仓库是一个独立于集群的资源（design doc 2026-08-13 §3.1）：它在任何集群
 * 绑定它之前就存在，一个集群通过 repoId 指向它。这一页是仓库地址、分支与
 * 凭据的唯一编辑入口 —— 集群页上那三项只读。两处都能改就是两个真相来源：
 * 操作者在集群页改了地址，这一页显示的还是旧的，而平台真正会去连的是哪
 * 一个，只能靠读代码才知道。
 *
 * 本页只展示仓库级结论，不展示任何路径级结论：`policyPath` 属于绑定，
 * 一个仓库可以被多个集群按不同路径绑定，把某一条绑定的路径结论摆在仓库
 * 行上，读的人会以为那是这个仓库的属性。
 */
export default function GitReposPage() {
  const [refreshKey, setRefreshKey] = useState(0)
  const bump = () => setRefreshKey((k) => k + 1)

  const { data: repos, error, loading } = useResource(
    `git-repos:${refreshKey}`,
    () => api.gitRepos(),
  )

  return (
    <div>
      <PageHeader
        title="策略仓库"
        description="平台把候选策略写进 Git，由 Config Sync 应用到集群 —— 仓库是策略的部署事实来源。仓库独立于集群存在：先在这里登记，再到集群管理页把某个集群绑定到它并指定 policyPath。校验只做只读查询：它能确认凭据可解析、仓库可达、认证通过、分支存在，但它从不向仓库提交任何内容，因此证明不了平台能往那里提交 —— 那要等真正提交一次才知道。"
      />

      <RepoListSection repos={repos} error={error} loading={loading} onChanged={bump} />
      <CreateRepoSection onCreated={bump} />
    </div>
  )
}

/* ---------------------------------------------------------------------- */
/* 1. 已登记仓库                                                            */
/* ---------------------------------------------------------------------- */

function RepoListSection({ repos, error, loading, onChanged }: {
  repos: GitRepo[] | null
  error: string
  loading: boolean
  onChanged: () => void
}) {
  const [busyId, setBusyId] = useState<string | null>(null)
  const [editingId, setEditingId] = useState<string | null>(null)
  // 删除被拒的理由要留在屏幕上，不用一弹就消失的浏览器提示框：那句话是
  // 「先去解除某个集群的绑定」，操作者读完还得照着做。
  const [actionError, setActionError] = useState('')

  async function remove(id: string) {
    if (!window.confirm(
      `确认下线策略仓库 ${id}？仍被集群绑定的仓库不会被删除，服务端会拒绝并说明该先解除哪一处绑定。`,
    )) return
    setActionError('')
    setBusyId(id)
    try {
      await api.deleteGitRepo(id)
      onChanged()
    } catch (err) {
      // 服务端对「仍被绑定」给的是一个业务失败，msg 里写明该先去解除绑定
      // （internal/httpapi/cluster_handler.go 的 ErrRepoInUse 分支）。原样
      // 展示，不收窄成一句「删除失败」—— 收窄掉的正是操作者据以行动的
      // 那句话，而级联删除更糟：它会静默解除某个集群的策略下发路径。
      setActionError(err instanceof ApiError ? err.msg : '删除失败，请稍后重试')
    } finally {
      setBusyId(null)
    }
  }

  return (
    <Section
      title="已登记仓库"
      description="没校验过的仓库写「仓库未校验」而不是留白 —— 空单元格会被读成「加载中」或「没什么要报告的」，而「从未校验过」与「校验通过」是相反的两件事实。「编辑」在行内展开：修改是整体替换，且服务端会把上一次的结论清成未校验（换了地址之后，旧的结论描述的是另一个仓库），要新结论请再点一次重新校验。"
      meta={repos ? `${repos.length} 个` : undefined}
    >
      {actionError && <FormError>{actionError}</FormError>}
      {error ? (
        <p style={{ color: 'var(--verdict-deny)' }}>{error}</p>
      ) : loading || !repos ? (
        <p style={{ color: 'var(--text-muted)' }}>加载中…</p>
      ) : repos.length === 0 ? (
        <EmptyState
          message="尚未登记任何策略仓库。"
          detail="使用下方表单登记第一个。在此之前，集群无法绑定策略仓库，平台也无处下发策略。"
        />
      ) : (
        <TableCard>
          <thead>
            <tr>
              <th>仓库 ID</th>
              <th>地址</th>
              <th>分支</th>
              <th>凭据引用</th>
              <th>仓库级校验</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            {repos.map((r) => (
              // Fragment 而非单个 <tr>：编辑面板作为紧随其后的整宽行展开，
              // 与被编辑的那一行同处一张表，操作者不必在弹层与表格之间
              // 比对自己正在改哪个仓库。
              <Fragment key={r.repoId}>
                <tr>
                  <td className="mono">{r.repoId}</td>
                  <td className="mono" style={{ fontSize: 'var(--text-xs)' }}>{r.repoUrl}</td>
                  <td className="mono">{r.branch}</td>
                  <td className="mono" style={{ fontSize: 'var(--text-xs)' }}>
                    {/*
                      空的凭据引用是一个合法状态（仓库可以先登记、凭据稍后
                      再配），但它不能显示成空白：那一格空着时，「还没配」与
                      「配了但界面没读到」在屏幕上长得一模一样。
                    */}
                    {r.credentialRef === ''
                      ? <span style={{ color: 'var(--text-muted)' }}>未配置</span>
                      : r.credentialRef}
                  </td>
                  <td><RepoVerifyCell repo={r} onChanged={onChanged} /></td>
                  <td>
                    <div style={{ display: 'flex', gap: 'var(--space-2)' }}>
                      <button
                        onClick={() => setEditingId(editingId === r.repoId ? null : r.repoId)}
                        style={secondaryButtonStyle}
                      >
                        {editingId === r.repoId ? '收起' : '编辑'}
                      </button>
                      <button
                        onClick={() => remove(r.repoId)}
                        disabled={busyId === r.repoId}
                        style={buttonStyle}
                      >
                        {busyId === r.repoId ? '删除中…' : '删除'}
                      </button>
                    </div>
                  </td>
                </tr>
                {editingId === r.repoId && (
                  <tr>
                    <td colSpan={6} style={{ background: 'var(--surface-sunken)' }}>
                      {/*
                        key 绑定仓库 ID：换一个仓库展开时必须重新播种，
                        复用同一份表单状态会把上一个仓库的地址带进来。
                      */}
                      <EditRepoForm
                        key={r.repoId}
                        repo={r}
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
 * 仓库级校验这一格：结论、校验时刻、处置说明、重新校验。
 *
 * 四样东西挤在同一格而不是拆成四列，是因为它们说的是同一件事的四个侧面
 * ——一个「仓库只读校验通过」脱离了它发生的时刻就没有意义。
 *
 * 结论与时刻一律显示，不做「没问题就不说话」的省略：一格空白在这张表里
 * 会被读成「没什么要报告的」。
 */
function RepoVerifyCell({ repo, onChanged }: { repo: GitRepo; onChanged: () => void }) {
  const [busy, setBusy] = useState(false)
  const [outcome, setOutcome] = useState<VerifyOutcomeView | null>(null)
  const [error, setError] = useState('')
  const view = describeRepoVerifyStatus(repo)

  async function reverify() {
    setBusy(true)
    setOutcome(null)
    setError('')
    try {
      const status = await api.verifyGitRepo(repo.repoId)
      // 上面那个徽章始终由服务端读模型驱动，响应里的结论不就地贴进去：
      // 服务端是唯一真相源，重新拉一次列表（同本页 refreshKey 的纪律）。
      //
      // 但「这一次发生了什么」必须单独说出来，否则未配置校验器时点一下
      // 「重新校验」界面毫无反应，操作者只能猜是按钮坏了还是结论没变。
      setOutcome(describeRepoVerifyOutcome(status))
      onChanged()
    } catch (err) {
      // 真正会撞上的是库里那行已经不合今天的规则：迁移把绑定里的
      // https:// 地址原样搬进了仓库表，而服务端在做出站之前就会指名
      // 「repoUrl 不是 SSH 形态」并说清后果。原样展示这句话，不收窄成
      // 一句「校验失败」—— 收窄掉的正是操作者据以行动的那句话。
      setError(err instanceof ApiError ? err.msg : '重新校验失败，请稍后重试')
    } finally {
      setBusy(false)
    }
  }

  return (
    <div style={{
      display: 'flex', flexDirection: 'column', alignItems: 'flex-start',
      gap: 'var(--space-1)', maxWidth: 340,
    }}>
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
      {outcome && <VerifyOutcomeNote outcome={outcome} />}
      {error && <FormError>{error}</FormError>}
    </div>
  )
}

/* ---------------------------------------------------------------------- */
/* 2. 仓库表单：登记与编辑共用字段                                            */
/* ---------------------------------------------------------------------- */

/**
 * 仓库表单的公共字段。
 *
 * 登记与编辑共用同一份字段与同一套折算（见 gitRepoForm.ts）：两份拷贝
 * 一定会漂移，而漂移的落点是策略下发路径。
 *
 * mode 只影响一处：仓库 ID 在编辑时只读。它是不可改键 —— 服务端的
 * handleUpdateGitRepo 只认路径参数，请求体里的 repoId 不参与；允许在这里
 * 改它，等于让一次「改地址」把审计里那一串行变成无主记录。
 */
function RepoFields({ values, patch, mode }: {
  values: RepoFormValues
  patch: (p: Partial<RepoFormValues>) => void
  mode: 'create' | 'edit'
}) {
  return (
    <FormGrid>
      <TextField
        label={mode === 'edit' ? '仓库 ID（不可修改）' : '仓库 ID'}
        value={values.repoId}
        onChange={(v) => patch({ repoId: v })}
        readOnly={mode === 'edit'}
        mono
      />
      <TextField
        label="repoUrl" value={values.repoUrl} mono
        placeholder="ssh://git@gitlab.example.com/net/policies.git"
        onChange={(v) => patch({ repoUrl: v })}
      />
      <TextField
        label="branch" value={values.branch} mono placeholder="main"
        onChange={(v) => patch({ branch: v })}
      />
      <TextField
        label="credentialRef（可留空）" value={values.credentialRef} mono
        onChange={(v) => patch({ credentialRef: v })}
      />
    </FormGrid>
  )
}

/**
 * 提交前把「这一次会把仓库改成什么」写出来。
 *
 * 四个输入框的不同填法长得都一样，让结果只在提交后从列表里的一行文字
 * 体现，等于把「我以为我只改了分支」留到已经写进库之后才被发现。
 */
function RepoOutcome({ values }: { values: RepoFormValues }) {
  const resolution = resolveGitRepo(values)
  return (
    <p style={{
      margin: 'var(--space-2) 0 0', fontSize: 'var(--text-xs)',
      color: resolution.ok ? 'var(--text-secondary)' : 'var(--verdict-unknown)',
    }}>
      {resolution.ok ? resolution.summary : resolution.error}
    </p>
  )
}

function CreateRepoSection({ onCreated }: { onCreated: () => void }) {
  const [values, setValues] = useState<RepoFormValues>(blankRepoValues)
  const [error, setError] = useState('')
  const [outcome, setOutcome] = useState<VerifyOutcomeView | null>(null)
  const [busy, setBusy] = useState(false)

  const patch = (p: Partial<RepoFormValues>) => setValues((v) => ({ ...v, ...p }))

  async function submit(e: FormEvent) {
    e.preventDefault()
    setError('')
    setOutcome(null)

    const resolved = resolveGitRepo(values)
    if (!resolved.ok) {
      setError(resolved.error)
      return
    }

    setBusy(true)
    try {
      const status = await api.createGitRepo(resolved.repo)
      setValues(blankRepoValues())
      // 登记会顺带跑一次只读校验，这句回执说的是那一次校验；列表里的
      // 徽章仍然由服务端读模型驱动。两者可能不一致，理由见
      // describeOutcome 的注释。
      setOutcome(describeRepoVerifyOutcome(status))
      onCreated()
    } catch (err) {
      // 后端把拒绝的具体理由写进 msg：https:// 地址会被指名「不是 SSH
      // 形态」，并说清后果（认证方法与传输对不上，失败会被误报成「仓库
      // 不可达」）。原样展示，不收窄成一句「登记失败」。
      setError(err instanceof ApiError ? err.msg : '登记失败，请稍后重试')
    } finally {
      setBusy(false)
    }
  }

  return (
    <Section
      title="登记新仓库"
      description="repoUrl 必须是 SSH 形态（ssh://git@host/path 或 git@host:path）：平台按仓库发放 deploy key，https:// 地址连拨号都不会发生，服务端会在保存时直接拒绝并说明理由。登记会顺带做一次只读校验，校验不通过不影响登记 —— 存下来和可信是两件事。"
    >
      <Card style={{ padding: 'var(--space-4)' }}>
        <form onSubmit={submit}>
          <RepoFields values={values} patch={patch} mode="create" />
          <RepoOutcome values={values} />

          {error && <FormError>{error}</FormError>}
          {outcome && <VerifyOutcomeNote outcome={outcome} />}

          <button type="submit" disabled={busy} style={{ ...buttonStyle, marginTop: 'var(--space-3)' }}>
            {busy ? '提交中…' : '登记仓库'}
          </button>
        </form>
      </Card>
    </Section>
  )
}

/**
 * 编辑已登记仓库。
 *
 * 表单从仓库现值播种，一项都不能省：PUT 是整体替换，表单里空着的字段
 * 提交后就是库里空着的字段。一个只想改分支的操作者不该因此清掉
 * credentialRef —— 少掉的是平台取凭据的那个引用。
 *
 * 保存之后**不自动重新校验**：服务端把结论清成未校验（换了地址之后，旧的
 * 结论描述的是另一个仓库），要新结论去点重新校验。这样「改配置」与
 * 「跑校验」在审计里始终是两条可以分开读的记录。
 *
 * 已知限制：整体替换 + 无版本号意味着两个人同时编辑同一个仓库时后写覆盖
 * 先写，且双方都不会收到提示。乐观锁需要在存储层引入版本列，不在本轮范围。
 */
function EditRepoForm({ repo, onSaved, onCancel }: {
  repo: GitRepo
  onSaved: () => void
  onCancel: () => void
}) {
  const [values, setValues] = useState<RepoFormValues>(() => repoFormValuesOf(repo))
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  const patch = (p: Partial<RepoFormValues>) => setValues((v) => ({ ...v, ...p }))

  async function submit(e: FormEvent) {
    e.preventDefault()
    setError('')

    const resolved = resolveGitRepo(values)
    if (!resolved.ok) {
      setError(resolved.error)
      return
    }

    setBusy(true)
    try {
      // ID 取自这一行的仓库而不是表单值：表单里那一格是只读的，但读它
      // 就等于让「ID 从哪里来」多一条路径，而那条路径上改一次 ID 会把
      // 审计里的历史留在一个不存在的仓库名下。
      await api.updateGitRepo(repo.repoId, resolved.repo)
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
          编辑 {repo.repoId}：提交会整体替换该仓库的登记信息，表单里的每一项都会被写入，
          因此下面的值已按当前登记内容预填——清空一项就是把它从库里清空。
          保存后服务端会把校验结论清成「仓库未校验」，要新结论请再点一次「重新校验」。
        </SubHeading>
        <RepoFields values={values} patch={patch} mode="edit" />
        <RepoOutcome values={values} />

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

function TextField({ label, value, onChange, mono, placeholder, readOnly }: {
  label: string
  value: string
  onChange: (v: string) => void
  mono?: boolean
  placeholder?: string
  readOnly?: boolean
}) {
  return (
    <label style={{ display: 'block' }}>
      <span style={fieldLabelStyle}>{label}</span>
      <input
        className="ctl"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        placeholder={placeholder}
        readOnly={readOnly}
        style={{
          width: '100%',
          fontFamily: mono ? 'var(--mono)' : undefined,
          // 不可编辑的输入框必须看得出不可编辑：一个外观正常却打不进字的
          // 输入框会被读成界面卡住，而不是"这一项不能改"。
          background: readOnly ? 'var(--surface-sunken)' : undefined,
          color: readOnly ? 'var(--text-muted)' : undefined,
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
