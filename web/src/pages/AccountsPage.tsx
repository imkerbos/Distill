import { Fragment, useState, type CSSProperties, type FormEvent, type ReactNode } from 'react'
import { api, ApiError } from '../api/client'
import type { Account } from '../api/types'
import { useResource } from '../api/useResource'
import { useSession } from '../auth/SessionContext'
import { Button, Card, Chip, EmptyState, ErrorNotice, Notice, PageHeader, Section, Skeleton, TableCard } from '../components/ui'
import {
  BOOTSTRAP_LOCKOUT_WARNING, accountStatusLabel, blankNewAccount, blankOwnPassword,
  blankResetPassword, enabledAdmins, previewAccountAction, resolveNewAccount, resolveOwnPassword,
  resolveResetPassword, roleLabel, showsAccountAdminEntry, signedInViaBootstrap,
  type AccountActionKind, type NewAccountValues, type OwnPasswordValues, type ResetPasswordValues,
} from './accountForm'
import { formatUtcTime } from './verifyView'

/**
 * 账号管理页与改密页。
 *
 * 两个组件放在同一个文件里，是因为它们说的是同一件事的两面 —— 账号与凭据 ——
 * 并且共用 accountForm.ts 那一套折算。它们的路由与可见性不同：账号管理是
 * 管理员的事，改自己的密码任何角色都能做（design doc 2026-08-14 §8）。
 *
 * **密码在这一页上只往一个方向流动**：输入框里打进来，折算成请求体，发出去，
 * 然后当场从组件状态里清掉。它不回显、不写 localStorage / sessionStorage、
 * 不进 URL，服务端也不会把它回给我们（规范 §19、§20、§21）。
 */

/* ---------------------------------------------------------------------- */
/* 1. 账号管理                                                              */
/* ---------------------------------------------------------------------- */

export default function AccountsPage() {
  const { identity } = useSession()

  // 角色只来自服务端的当前会话端点（见 SessionContext）。这里拿它决定
  // 渲染什么，**不拿它决定允许什么** —— 后者在服务端每次请求现读账号
  // 记录之后判定（规范 §34）。
  if (!identity) return null
  if (!showsAccountAdminEntry(identity.role)) return <AdminOnlyNotice />
  return <AccountsAdmin me={identity.username} />
}

/**
 * 只读账号直接敲地址进来时看到的东西。
 *
 * 说清楚"服务端已经拒绝"，而不是渲染一张永远加载失败的空表：后者会让
 * 使用者以为平台坏了，然后去找运维。导航里不放这个入口只是省掉一次
 * 无用点击，拦住请求的从来不是导航（规范 §34）。
 */
function AdminOnlyNotice() {
  return (
    <div>
      <PageHeader title="账号管理" />
      <Notice>
        这一页需要管理员角色。你的账号是只读，
        服务端会拒绝这一页上的每一个请求 —— 导航里不显示这个入口只是省掉一次无用点击，
        真正拦住请求的是服务端每次请求现读的角色，不是这个界面。
        要改自己的密码，请走左侧的「修改密码」。
      </Notice>
    </div>
  )
}

function AccountsAdmin({ me }: { me: string }) {
  const [refreshKey, setRefreshKey] = useState(0)
  const bump = () => setRefreshKey((k) => k + 1)

  const { data: accounts, error, loading } = useResource(
    `accounts:${refreshKey}`,
    () => api.accounts(),
  )

  // 引导账号的判定与服务端同源：账号表里查不到自己（见 signedInViaBootstrap）。
  const viaBootstrap = signedInViaBootstrap(me, accounts)

  return (
    <div>
      <PageHeader
        title="账号管理"
        description="账号存在数据库里，角色在每次请求时现读 —— 降权或停用一个账号，下一次请求就生效，不必等它手里那张会话过期。新建的账号一律是只读，提升为管理员是一次单独的操作，有它自己的审计行。停用是可逆的暂停，删除是软删除：审计里的历史留着，用户名也不再回收。"
      />

      {/*
        引导账号的警告必须在任何操作**之前**就在屏幕上，而不是等某个按钮
        被点开才出现：把一个账号提升为管理员的那一刻，操作者手里的引导凭据
        当场作废，事后再解释已经没有意义（design doc §2）。确认面板里还会
        针对那一次操作再说一遍。
      */}
      {viaBootstrap && <Notice>{BOOTSTRAP_LOCKOUT_WARNING}</Notice>}

      <AccountListSection
        me={me} accounts={accounts} error={error} loading={loading} onChanged={bump}
      />
      <CreateAccountSection viaBootstrap={viaBootstrap} onCreated={bump} />
    </div>
  )
}

/** 表格上正在被确认的那一次操作。null 表示此刻没有待确认的操作。 */
interface Pending {
  kind: AccountActionKind
  username: string
}

function AccountListSection({ me, accounts, error, loading, onChanged }: {
  me: string
  accounts: Account[] | null
  error: string
  loading: boolean
  onChanged: () => void
}) {
  const [pending, setPending] = useState<Pending | null>(null)

  const rows = accounts ?? []
  const adminCount = enabledAdmins(rows).length

  return (
    <Section
      title="账号"
      description="每一个改变账号的动作都要先确认，确认面板里写着这一次会把哪个账号变成什么。已知会被服务端拒绝的（动最后一个启用中的管理员）在点确认之前就说出来 —— 真正的判定在服务端的同一个事务里，界面这份计数只是刚拉到的一份快照。"
      meta={accounts ? `${accounts.length} 个账号，其中 ${adminCount} 个启用中的管理员` : undefined}
    >
      {error ? (
        <p className="text-deny">{error}</p>
      ) : loading || !accounts ? (
        <Skeleton />
      ) : accounts.length === 0 ? (
        <EmptyState
          message="账号表是空的。"
          detail="平台还没有任何库内账号，此刻唯一能登录的是配置文件里的引导账号。用下方的表单建第一个账号，它会是只读；把它提升为管理员的那一刻，引导账号就失效了。"
        />
      ) : (
        <TableCard>
          <thead>
            <tr>
              <th>用户名</th>
              <th>角色</th>
              <th>状态</th>
              <th>创建于</th>
              <th>最近变更</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            {accounts.map((a) => (
              // Fragment 而非单个 <tr>：确认面板作为紧随其后的整宽行展开，
              // 与被操作的那一行同处一张表 —— 操作者不必在弹层与表格之间
              // 比对自己正在动哪个账号。
              <Fragment key={a.username}>
                <tr>
                  <td className="mono">
                    {a.username}
                    {a.username === me && (
                      <span className="ml-[6px] text-ink-muted">（你自己）</span>
                    )}
                  </td>
                  <td>
                    {/* 管理员用 strong chip 稍微压出分量：这一列一眼要能扫出
                        谁能改集群、谁只读。文本仍来自 roleLabel（带测试）。 */}
                    <Chip strong={a.role === 'ADMIN'}>{roleLabel(a.role)}</Chip>
                  </td>
                  <td>
                    <span className="inline-flex items-center gap-[6px]"
                      style={{ color: a.disabledAt ? 'var(--text-muted)' : undefined }}>
                      <StatusDot on={!a.disabledAt} />
                      {accountStatusLabel(a)}
                    </span>
                  </td>
                  <td className="text-xs">{formatUtcTime(a.createdAt)}</td>
                  <td className="text-xs">{formatUtcTime(a.updatedAt)}</td>
                  <td>
                    <div className="flex flex-wrap gap-2">
                      {actionsFor(a).map((kind) => (
                        <Button
                          key={kind}
                          type="button"
                          onClick={() => setPending(
                            pending?.username === a.username && pending.kind === kind
                              ? null
                              : { kind, username: a.username },
                          )}
                          variant="secondary"
                        >
                          {previewAccountAction(kind, a, rows).verb}
                        </Button>
                      ))}
                    </div>
                  </td>
                </tr>
                {pending?.username === a.username && (
                  <tr>
                    <td colSpan={6} className="bg-sunken">
                      {/*
                        key 绑定账号与动作：换一个动作展开时必须重新挂载，
                        复用同一份状态会把上一次输入的密码带进来。
                      */}
                      <ConfirmPanel
                        key={`${a.username}:${pending.kind}`}
                        kind={pending.kind}
                        target={a}
                        accounts={rows}
                        isSelf={a.username === me}
                        onCancel={() => setPending(null)}
                        onDone={() => { setPending(null); onChanged() }}
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
 * 一行账号上可做的动作。
 *
 * 角色那一项按现值给出相反的方向，不给"改成当前角色"这个空操作：一个
 * 点下去什么都不会变的按钮，会让操作者以为自己改过了。
 */
function actionsFor(a: Account): AccountActionKind[] {
  return [
    a.role === 'ADMIN' ? 'DEMOTE' : 'PROMOTE',
    a.disabledAt ? 'ENABLE' : 'DISABLE',
    'RESET_PASSWORD',
    'DELETE',
  ]
}

/**
 * 确认面板：先写清楚这一次会发生什么，再给按钮。
 *
 * 三段话各有分工，一段都不能省：
 *
 *   - change   这次把哪个账号变成什么。六个动作从按钮上看长得一样。
 *   - refusal  服务端已知会拒绝的（动最后一个启用中的管理员），提前说，
 *              不让操作者点完才发现（design doc §5）。
 *   - bootstrap 这一步会不会让引导账号当场失效（design doc §2）。
 *
 * 服务端真的拒绝时，把它的 msg 原样留在屏幕上，不收窄成一句"操作失败"：
 * 收窄掉的正是操作者据以行动的那句话（同 GitReposPage 对 ErrRepoInUse 的处置）。
 */
function ConfirmPanel({ kind, target, accounts, isSelf, onDone, onCancel }: {
  kind: AccountActionKind
  target: Account
  accounts: Account[]
  isSelf: boolean
  onDone: () => void
  onCancel: () => void
}) {
  const [values, setValues] = useState<ResetPasswordValues>(blankResetPassword)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const preview = previewAccountAction(kind, target, accounts)

  async function run() {
    setError('')

    // 密码先折算再置忙：折算失败时不该留下一个转着的按钮。
    let password = ''
    if (kind === 'RESET_PASSWORD') {
      const resolved = resolveResetPassword(values)
      if (!resolved.ok) {
        setError(resolved.error)
        return
      }
      password = resolved.body.password
    }

    setBusy(true)
    try {
      await callFor(kind, target.username, password)
      // 明文不在组件状态里多留一秒：请求已经发出去，它在这里再无用处。
      setValues(blankResetPassword())
      onDone()
    } catch (err) {
      // 服务端对"最后一个启用中的管理员"给的是一个业务失败，msg 里写着
      // 该先去提一个管理员上来（internal/httpapi/cluster_handler.go 的
      // ErrLastAdmin 分支）。原样展示 —— 收窄成"操作失败"之后，操作者
      // 只会以为平台坏了然后重试，而重试永远得到同一个结果。
      setError(err instanceof ApiError ? err.msg : '操作失败，请稍后重试')
    } finally {
      setBusy(false)
    }
  }

  return (
    <Card className="my-3 p-4">
      <div className="mb-2 text-sm">
        {preview.change}
      </div>
      {isSelf && (
        <ConsequenceLine tone="warn">
          这是你自己的账号。做完之后你可能就没有权限把它改回来了。
        </ConsequenceLine>
      )}
      {preview.bootstrap && (
        <ConsequenceLine tone="warn">{preview.bootstrap}</ConsequenceLine>
      )}
      {preview.refusal && (
        <ConsequenceLine tone="deny">{preview.refusal}</ConsequenceLine>
      )}

      {kind === 'RESET_PASSWORD' && (
        <div className="mt-3">
          <PasswordField
            label="新密码"
            value={values.password}
            autoComplete="new-password"
            onChange={(v) => setValues((s) => ({ ...s, password: v }))}
          />
          <PasswordField
            label="再输一次新密码"
            value={values.confirm}
            autoComplete="new-password"
            onChange={(v) => setValues((s) => ({ ...s, confirm: v }))}
          />
        </div>
      )}

      {error && <ErrorNotice>{error}</ErrorNotice>}

      <div className="mt-3 flex gap-2">
        <Button type="button" onClick={run} disabled={busy} variant="primary">
          {busy ? '执行中…' : `确认${preview.verb}`}
        </Button>
        <Button type="button" onClick={onCancel} variant="secondary">取消</Button>
      </div>
    </Card>
  )
}

/**
 * 把一个动作发给它对应的端点。
 *
 * 单独一个 switch 而不是在每个按钮上各写一次调用：六个动作里有三个会让
 * 平台失去管理员，把"哪个按钮打哪个端点"摊在一处，接错线是看得见的。
 */
function callFor(kind: AccountActionKind, username: string, password: string): Promise<unknown> {
  switch (kind) {
    case 'PROMOTE': return api.updateAccountRole(username, 'ADMIN')
    case 'DEMOTE': return api.updateAccountRole(username, 'VIEWER')
    case 'DISABLE': return api.disableAccount(username)
    case 'ENABLE': return api.enableAccount(username)
    case 'DELETE': return api.deleteAccount(username)
    case 'RESET_PASSWORD': return api.resetAccountPassword(username, password)
  }
}

/* ---------------------------------------------------------------------- */
/* 2. 新建账号                                                              */
/* ---------------------------------------------------------------------- */

function CreateAccountSection({ viaBootstrap, onCreated }: {
  viaBootstrap: boolean
  onCreated: () => void
}) {
  const [values, setValues] = useState<NewAccountValues>(blankNewAccount)
  const [error, setError] = useState('')
  const [done, setDone] = useState('')
  const [busy, setBusy] = useState(false)

  const resolution = resolveNewAccount(values)

  async function submit(e: FormEvent) {
    e.preventDefault()
    setError('')
    setDone('')
    if (!resolution.ok) {
      setError(resolution.error)
      return
    }
    setBusy(true)
    try {
      const created = await api.createAccount(resolution.body.username, resolution.body.password)
      // 表单当场清空：明文密码不在组件状态里多留一秒，也不写进任何存储。
      // 回执里只有用户名与角色 —— 服务端不回显密码，这里也没有可回显的东西。
      setValues(blankNewAccount())
      setDone(`已建好账号 ${created.username}，角色是${roleLabel(created.role)}。`
        + '新密码不会再显示第二次，请现在就把它交给使用者。')
      onCreated()
    } catch (err) {
      // 服务端的拒绝理由原样展示：与引导账号撞名、密码太短、密码超过
      // bcrypt 的 72 字节上限，每一条都点着名说了该怎么改。
      setError(err instanceof ApiError ? err.msg : '新建失败，请稍后重试')
    } finally {
      setBusy(false)
    }
  }

  return (
    <Section
      title="新建账号"
      description="新账号一律建成只读，这是服务端定死的 —— 提升为管理员是另一次操作，有它自己的审计行。密码由你在这里指定，服务端只存 bcrypt 哈希：它不会回显给你，也没有任何接口能把它读回来，忘了只能重置。密码规则（最短 12 个字符、最长 72 字节）由服务端判定，不满足时它会点名说清楚。"
    >
      <Card className="p-4">
        <form onSubmit={submit}>
          {viaBootstrap && (
            <ConsequenceLine tone="warn">
              建号本身不会让引导账号失效 —— 新账号是只读的。真正的那一刻是你把它
              提升为管理员，所以请先记住这里设的密码，再去做那一步。
            </ConsequenceLine>
          )}
          <div style={{
            display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(220px, 1fr))',
            gap: 'var(--space-3)', marginTop: 'var(--space-3)',
          }}>
            <label className="block">
              <span style={fieldLabelStyle}>用户名</span>
              <input
                className="ctl"
                value={values.username}
                autoComplete="off"
                onChange={(e) => setValues((s) => ({ ...s, username: e.target.value }))}
                style={{ width: '100%', fontFamily: 'var(--mono)' }}
              />
            </label>
            <PasswordField
              label="密码"
              value={values.password}
              autoComplete="new-password"
              onChange={(v) => setValues((s) => ({ ...s, password: v }))}
            />
            <PasswordField
              label="再输一次密码"
              value={values.confirm}
              autoComplete="new-password"
              onChange={(v) => setValues((s) => ({ ...s, confirm: v }))}
            />
          </div>

          <p style={{
            margin: 'var(--space-2) 0 0', fontSize: 'var(--text-xs)',
            color: resolution.ok ? 'var(--text-secondary)' : 'var(--verdict-unknown)',
          }}>
            {resolution.ok ? resolution.summary : resolution.error}
          </p>

          {error && <ErrorNotice>{error}</ErrorNotice>}
          {done && <FormNote>{done}</FormNote>}

          <Button type="submit" disabled={busy} variant="primary" className="mt-3">
            {busy ? '提交中…' : '新建账号'}
          </Button>
        </form>
      </Card>
    </Section>
  )
}

/* ---------------------------------------------------------------------- */
/* 3. 改自己的密码：任何角色都能用                                            */
/* ---------------------------------------------------------------------- */

/**
 * 改自己的密码。
 *
 * 走的是 /api/v1/me/password，目标取自服务端的会话 —— 路径里没有用户名，
 * 因此这一页上不存在"改别人的密码"这个形状（规范 §7、§9）。
 *
 * **当前密码必填**（规范 §28）：会话可能是一台没锁屏的机器留下的。
 */
export function OwnPasswordPage() {
  const { identity } = useSession()
  const [values, setValues] = useState<OwnPasswordValues>(blankOwnPassword)
  const [error, setError] = useState('')
  const [done, setDone] = useState('')
  const [busy, setBusy] = useState(false)

  async function submit(e: FormEvent) {
    e.preventDefault()
    setError('')
    setDone('')

    const resolved = resolveOwnPassword(values)
    if (!resolved.ok) {
      setError(resolved.error)
      return
    }

    setBusy(true)
    try {
      await api.changeOwnPassword(resolved.body.currentPassword, resolved.body.newPassword)
      // 两个明文当场从状态里清掉。它们不回显、不进任何存储、不进 URL。
      setValues(blankOwnPassword())
      setDone('密码已修改。下次登录请用新密码；已经签发的会话不会因此失效。')
    } catch (err) {
      // 当前密码不对时服务端回的是登录用的那一句，原样展示。引导账号在
      // 这里会拿到一句"密码在配置文件里，不能从这里改" —— 那句话也必须
      // 原样出现，否则操作者会以为是自己点错了地方。
      setError(err instanceof ApiError ? err.msg : '修改失败，请稍后重试')
    } finally {
      setBusy(false)
    }
  }

  // 两次新密码的一致性只用来给一个当场的提示，**不替代** resolveOwnPassword：
  // 提交时仍以那一个纯函数为准（它带测试）。这里只是让人不必等到提交才知道
  // 自己拼错了第二遍。
  const bothFilled = values.newPassword !== '' && values.confirm !== ''
  const matches = values.newPassword === values.confirm
  // 三个框都非空才让提交。不在这里拦「两次不一致」——那由 resolveOwnPassword
  // 回一句明确的话，拦在按钮上只会让人对着一个灰按钮猜为什么点不动。
  const canSubmit = values.currentPassword !== '' && bothFilled && !busy

  return (
    <div className="mx-auto max-w-[540px]">
      <PageHeader
        title="修改密码"
        description="改的永远是你自己的密码——目标取自服务端的会话，这一页发不出「改别人密码」的请求。"
      />
      <Card className="overflow-hidden">
        {/* 身份条：改的是谁的密码，先说清。会话被捡到时，这一行就是那句
            「你确定坐在这里的是本人吗」的视觉锚点。 */}
        <div className="flex items-center gap-3 border-b border-line bg-sunken px-5 py-4">
          <IdentityGlyph />
          <div className="min-w-0">
            <div className="text-xs text-ink-muted">当前登录身份</div>
            <div className="truncate text-sm font-medium text-ink">
              {identity?.username ?? '未知'}
            </div>
          </div>
        </div>

        <form onSubmit={submit} className="px-5 py-5">
          <PasswordField
            label="当前密码"
            value={values.currentPassword}
            autoComplete="current-password"
            onChange={(v) => setValues((s) => ({ ...s, currentPassword: v }))}
          />
          <PasswordField
            label="新密码"
            value={values.newPassword}
            autoComplete="new-password"
            onChange={(v) => setValues((s) => ({ ...s, newPassword: v }))}
          />
          <PasswordField
            label="再输一次新密码"
            value={values.confirm}
            autoComplete="new-password"
            onChange={(v) => setValues((s) => ({ ...s, confirm: v }))}
            hint={bothFilled ? <MatchHint ok={matches} /> : undefined}
          />

          {error && <ErrorNotice>{error}</ErrorNotice>}
          {done && <FormNote>{done}</FormNote>}

          <Button
            type="submit"
            disabled={!canSubmit}
            variant="primary"
            className="mt-4 w-full justify-center"
          >
            {busy ? '提交中…' : '修改密码'}
          </Button>
        </form>

        {/* 安全脚注：把原先挤在标题里的两条保证挪到这里，各带一个字形，
            读起来是「这一步做了什么、没做什么」，而不是一段免责声明。 */}
        <div className="border-t border-line px-5 py-4">
          <SecurityNote>必须先输入当前密码：一张被捡到的会话不该能把账号的控制权拿走。</SecurityNote>
          <SecurityNote>改完后已经签发的会话不会失效——包括你现在这一张。</SecurityNote>
        </div>
      </Card>
    </div>
  )
}

/**
 * 账号状态的小圆点。启用中用 allow 绿，已停用用中性灰。
 *
 * 只是「状态」列文本旁的一个视觉锚点，不承载任何判定语义 —— 文本（启用中／
 * 已停用）仍由 accountStatusLabel 给出，圆点脱落也不丢信息。
 */
function StatusDot({ on }: { on: boolean }) {
  return (
    <span
      className="inline-block h-[7px] w-[7px] shrink-0 rounded-full"
      style={{ background: on ? 'var(--verdict-allow)' : 'var(--border-strong)' }}
      aria-hidden
    />
  )
}

/** 身份条左侧的字形：一把钥匙，指向「这是谁的凭据」。 */
function IdentityGlyph() {
  return (
    <span
      className="flex h-9 w-9 shrink-0 items-center justify-center rounded-chip border border-line bg-surface text-ink-muted"
      aria-hidden
    >
      <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2"
        strokeLinecap="round" strokeLinejoin="round">
        <circle cx="8" cy="15" r="4" />
        <path d="M10.85 12.15 19 4" />
        <path d="m18 5 2 2" />
        <path d="m15 8 2 2" />
      </svg>
    </span>
  )
}

/** 两次新密码是否一致的当场提示。绿色表示一致，判定红表示不一致。 */
function MatchHint({ ok }: { ok: boolean }) {
  const color = ok ? 'var(--verdict-allow)' : 'var(--verdict-deny)'
  return (
    <span className="inline-flex items-center gap-1 text-xs" style={{ color }}>
      <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="3"
        strokeLinecap="round" strokeLinejoin="round" aria-hidden>
        {ok ? <path d="M20 6 9 17l-5-5" /> : <><path d="M18 6 6 18" /><path d="m6 6 12 12" /></>}
      </svg>
      {ok ? '两次输入一致' : '两次输入不一致'}
    </span>
  )
}

/** 安全脚注的一行：一个盾牌字形 + 一句话。 */
function SecurityNote({ children }: { children: ReactNode }) {
  return (
    <p className="m-0 flex items-start gap-2 text-xs leading-relaxed text-ink-muted [&+&]:mt-2">
      <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2"
        strokeLinecap="round" strokeLinejoin="round" className="mt-[2px] shrink-0" aria-hidden>
        <path d="M20 13c0 5-3.5 7.5-7.66 8.95a1 1 0 0 1-.67-.01C7.5 20.5 4 18 4 13V6a1 1 0 0 1 1-1c2 0 4.5-1.2 6.24-2.72a1.17 1.17 0 0 1 1.52 0C14.51 3.81 17 5 19 5a1 1 0 0 1 1 1z" />
      </svg>
      <span>{children}</span>
    </p>
  )
}

/* ---------------------------------------------------------------------- */
/* 共享小件                                                                 */
/* ---------------------------------------------------------------------- */

/**
 * 密码输入框。
 *
 * 全页所有密码都走这一个组件，且它把 `type="password"` 写死 —— 不给调用方
 * 一个"这一处显示成明文"的开关。一个可选参数迟早会有人打开：那一刻密码
 * 就出现在屏幕上、出现在录屏里、出现在肩后（规范 §19）。
 *
 * 值只活在调用方的组件状态里，提交后当场清空；这里不碰 localStorage、
 * sessionStorage，也不把值放进任何 URL。
 */
function PasswordField({ label, value, onChange, autoComplete, hint }: {
  label: string
  value: string
  onChange: (v: string) => void
  autoComplete: 'current-password' | 'new-password'
  // hint 是标签右侧的一个当场提示（如两次是否一致）。可选：多数框不需要它。
  hint?: ReactNode
}) {
  return (
    <label className="mb-4 block">
      <span className="mb-1 flex items-center justify-between gap-2">
        <span style={fieldLabelStyle} className="mb-0">{label}</span>
        {hint}
      </span>
      <input
        className="ctl"
        type="password"
        value={value}
        autoComplete={autoComplete}
        onChange={(e) => onChange(e.target.value)}
        style={{ width: '100%' }}
      />
    </label>
  )
}

/** 后果说明：warn 是"做完会怎样"，deny 是"服务端会拒绝"。 */
function ConsequenceLine({ tone, children }: { tone: 'warn' | 'deny'; children: ReactNode }) {
  const color = tone === 'deny' ? 'var(--verdict-deny)' : 'var(--verdict-unknown)'
  const background = tone === 'deny' ? 'var(--verdict-deny-bg)' : 'var(--verdict-unknown-bg)'
  return (
    <p style={{
      margin: 'var(--space-2) 0 0', padding: 'var(--space-2)',
      background, color, border: `1px solid ${color}`,
      borderRadius: 'var(--radius)', fontSize: 'var(--text-sm)',
    }}>
      {children}
    </p>
  )
}


function FormNote({ children }: { children: ReactNode }) {
  return (
    <p role="status" style={{
      margin: 'var(--space-3) 0 0', padding: 'var(--space-2)',
      background: 'var(--surface-sunken)', color: 'var(--text-secondary)',
      borderRadius: 'var(--radius)', fontSize: 'var(--text-sm)',
    }}>
      {children}
    </p>
  )
}

const fieldLabelStyle: CSSProperties = {
  display: 'block', marginBottom: 4, fontSize: 'var(--text-xs)', color: 'var(--text-muted)',
}


