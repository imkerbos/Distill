import { useState, type CSSProperties, type FormEvent, type ReactNode } from 'react'
import { api, ApiError } from '../api/client'
import type { PlatformSettingView, SecretsBackend } from '../api/types'
import { useResource } from '../api/useResource'
import {
  ALL_SECRETS_BACKENDS, NUMERIC_FIELDS, RESTART_NOTICE, SECRETS_BACKEND_LABEL,
  buildSettingsWrite, restartRequiredLabels, settingsDiff, settingsFormValuesOf,
  type SettingsDiffRow, type SettingsFormValues,
} from './settingsForm'
import { Button, Card, EmptyState, ErrorNotice, PageHeader, Section, Select, Skeleton, TableCard } from '../components/ui'

/**
 * 平台设置页。
 *
 * 这些设置此前住在配置文件里，改一次要改文件加重新部署；现在落库
 * （design doc 2026-08-13 §1），本页是改动它们的唯一入口。
 *
 * 页面本身尽量不做判断：能不能提交、要提交什么、这次改了什么、哪几项
 * 要等重启，全部由 settingsForm.ts 算出来，这里只负责把结果摆上去。
 * 这样分是因为本页的判断跑不了测试而那一层跑得了 —— 上一次前端事故正是
 * 「上面的 tile 读一份数据、下面的表读另一份」，tsc / lint / build 三关
 * 都不会察觉一个组件读错了字段。
 */
export default function SettingsPage() {
  const [refreshKey, setRefreshKey] = useState(0)
  const { data, error, loading } = useResource(
    `settings:${refreshKey}`,
    () => api.settings(),
  )

  return (
    <div>
      <PageHeader
        title="平台设置"
        description="平台自身的运行期配置。除下面标注的四项之外，其余每一项都在使用处现读，保存后立刻生效，不需要重启。这一页的每一次保存都会写一条审计记录，前后值完整落库。"
      />
      {error ? (
        <p className="text-deny">{error}</p>
      ) : loading || !data ? (
        <Skeleton />
      ) : (
        // key 绑在 refreshKey 上：保存成功后整块表单按服务端刚回的那一份
        // 重新播种，host key 输入框也随之清空——留着上一次粘进去的原文，
        // 下一次保存就会在操作者没打算碰信任锚的时候又提交一遍。
        <SettingsForm
          key={refreshKey}
          current={data}
          onSaved={() => setRefreshKey((k) => k + 1)}
        />
      )}
    </div>
  )
}

function SettingsForm({ current, onSaved }: {
  current: PlatformSettingView
  onSaved: () => void
}) {
  const [values, setValues] = useState<SettingsFormValues>(() => settingsFormValuesOf(current))
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  const patch = (p: Partial<SettingsFormValues>) => setValues((v) => ({ ...v, ...p }))

  // 每次渲染都重算：差异区要在操作者按下保存**之前**就显示这次会写什么，
  // 而不是在提交时才算一次。用的是与提交同一个函数的同一个结果，界面上
  // 看到的那份差异因此就是会被发出去的那一份。
  const built = buildSettingsWrite(values)
  const rows = built.ok ? settingsDiff(current, built.body) : []

  async function submit(e: FormEvent) {
    e.preventDefault()
    setError('')
    if (!built.ok) {
      setError(built.error)
      return
    }
    setBusy(true)
    try {
      await api.updateSettings(built.body)
      onSaved()
    } catch (err) {
      // 后端把校验失败的具体字段写进 msg（比如「secretsBackend 为 DIR 时
      // secretsProject/secretsPrefix 必须为空」）——原样展示，收窄成一句
      // 「保存失败」会让操作者猜不出是哪一项不成立。
      setError(err instanceof ApiError ? err.msg : '保存失败，请稍后重试')
    } finally {
      setBusy(false)
    }
  }

  return (
    <form onSubmit={submit}>
      <Section
        title="运行参数"
        description="超时与有效期。每一项都必须为正：填 0 不是「不限制」，而是会话立即过期或超时保护被关闭。"
      >
        <Card className="p-4">
          <FormGrid>
            {NUMERIC_FIELDS.map((f) => (
              <NumberField
                key={f.key}
                label={f.label}
                hint={f.hint}
                // 这枚标记与差异区的「重启后生效」出自同一张表（NUMERIC_FIELDS
                // 的 effect），不是页面另写的一份判断。
                afterRestart={f.effect === 'AFTER_RESTART'}
                value={values[f.key]}
                onChange={(v) => patch({ [f.key]: v } as Partial<SettingsFormValues>)}
              />
            ))}
          </FormGrid>
          <SubHeading>标「重启后生效」的项：{RESTART_NOTICE}</SubHeading>
        </Card>
      </Section>

      <Section
        title="凭据解析后端"
        description="平台去哪里取访问策略仓库的凭据。后端选择是唯一真相：选中的后端与填写的字段必须互相印证，否则库里会同时留着两套配置而只有一套在生效——服务端会拒绝这样的组合，这里在提交前就说明它。"
      >
        <Card className="p-4">
          <FormGrid>
            <label className="block">
              <span style={fieldLabelStyle}>凭据后端</span>
              <Select
                value={values.secretsBackend}
                ariaLabel="凭据后端"
                onChange={(v) => patch({ secretsBackend: v as SecretsBackend })}
                options={ALL_SECRETS_BACKENDS
                  .map((b) => [b, SECRETS_BACKEND_LABEL[b]] as [string, string])}
                style={{ width: '100%' }}
              />
            </label>
            <TextField
              label="Secret Manager 项目"
              value={values.secretsProject}
              onChange={(v) => patch({ secretsProject: v })}
              mono
            />
            <TextField
              label="Secret Manager 前缀"
              value={values.secretsPrefix}
              onChange={(v) => patch({ secretsPrefix: v })}
              mono
            />
            <TextField
              label="本地凭据目录"
              value={values.secretsDir}
              onChange={(v) => patch({ secretsDir: v })}
              mono
            />
          </FormGrid>
        </Card>
      </Section>

      <HostKeysSection
        fingerprint={current.gitVerifyHostKeysFingerprint}
        value={values.hostKeysInput}
        onChange={(v) => patch({ hostKeysInput: v })}
      />

      <Section
        title="即将写入的改动"
        description="保存会整体替换这一行设置，并写一条前后值完整的审计记录。按下保存之前先在这里看一遍自己在改什么。"
      >
        {built.ok
          ? <DiffTable rows={rows} />
          : <ErrorNotice>{built.error}</ErrorNotice>}
      </Section>

      {error && <ErrorNotice>{error}</ErrorNotice>}

      <Button
        type="submit"
        disabled={busy || !built.ok || rows.length === 0}
        variant="primary" className="mt-3"
      >
        {busy ? '保存中…' : '保存设置'}
      </Button>
    </form>
  )
}

/**
 * SSH 信任锚区。
 *
 * 标题与说明直接写明它是什么，不放在折叠的帮助文本里：改这一项的人是在
 * 决定平台愿意和哪台 SSH 服务器说话（design doc §1.3）。一个只写着
 * 「host keys」的输入框会被当成一项连接参数。
 *
 * 只显示指纹。原文既没有从服务端回来过（PlatformSettingView 里没有那个
 * 字段），也不会被填进输入框——回显一份 SSH 公钥集合没有任何操作上的
 * 用处，只是给它多开一条会被截图、被贴进工单的出口（规范 §19、§20）。
 */
function HostKeysSection({ fingerprint, value, onChange }: {
  fingerprint: string
  value: string
  onChange: (v: string) => void
}) {
  return (
    <Section
      title="SSH 信任锚（Git host key）"
      description="这是平台连接策略仓库时的信任锚：它决定平台愿意和哪一台 SSH 服务器说话。换掉它就等于换掉平台信任的那台服务器——一份被替换的 host key 会让平台接受一个中间人，而连接看起来一切正常。"
    >
      <Card className="p-4 mb-3 mono text-sm break-all">
        <div>
          <span style={fieldLabelStyle}>当前指纹</span>
          {fingerprint ? (
            <div>
              {fingerprint}
            </div>
          ) : (
            // 空是一种必须被看见的状态：host key 为空时 gitverify.New 拒绝
            // 构造，于是所有 Git 校验都出不了结论。留白会被读成"正常"。
            <div style={{ fontSize: 'var(--text-sm)', color: 'var(--verdict-unknown)' }}>
              未配置 —— 平台当前无法完成任何 Git 仓库校验。
            </div>
          )}
        </div>

        <label className="block">
          <span style={fieldLabelStyle}>新的 known_hosts 原文（留空表示不修改）</span>
          <textarea
            className="ctl w-full"
            value={value}
            onChange={(e) => onChange(e.target.value)}
            rows={4}
            placeholder="gitlab.example.com ssh-ed25519 AAAA…"
          />
        </label>
        <SubHeading>
          这个框只用于写入，永远不会显示当前生效的 host key：服务端不回原文，
          能核对的只有上面那串指纹。保存之后请对照新指纹确认装上去的就是你粘的那一份。
          留空保存时这一项根本不会被提交，当前信任锚原样保留，其余改动照常生效；
          本页没有清空信任锚的操作。
        </SubHeading>
      </Card>
    </Section>
  )
}

/** 差异表。空表也要说清是"没有改动"，不能渲染成一张空表格。 */
function DiffTable({ rows }: { rows: SettingsDiffRow[] }) {
  const restart = restartRequiredLabels(rows)

  if (rows.length === 0) {
    return (
      <EmptyState
        message="当前表单与库里的设置一致，没有要写入的改动。"
        detail="改动任意一项之后，这里会逐项列出前后值。"
      />
    )
  }

  return (
    <>
      <TableCard>
        <thead>
          <tr>
            <th>项</th>
            <th>当前</th>
            <th>改为</th>
            <th>生效时机</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((r) => (
            <tr key={r.label}>
              <td>{r.label}</td>
              <td className="mono">{r.before}</td>
              <td className="mono">{r.after}</td>
              <td>{r.effect === 'AFTER_RESTART' ? '重启后生效' : '立刻生效'}</td>
            </tr>
          ))}
        </tbody>
      </TableCard>
      {restart.length > 0 && (
        <RestartNotice>
          {restart.join('、')}：{RESTART_NOTICE}
        </RestartNotice>
      )}
    </>
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
    <div className="mb-2 text-xs text-ink-muted">
      {children}
    </div>
  )
}

const fieldLabelStyle: CSSProperties = {
  display: 'block', marginBottom: 4, fontSize: 'var(--text-xs)', color: 'var(--text-muted)',
}

function TextField({ label, value, onChange, mono }: {
  label: string
  value: string
  onChange: (v: string) => void
  mono?: boolean
}) {
  return (
    <label className="block">
      <span style={fieldLabelStyle}>{label}</span>
      <input
        className="ctl w-full"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        style={{ width: '100%', fontFamily: mono ? 'var(--mono)' : undefined }}
      />
    </label>
  )
}

/**
 * 数字输入框。
 *
 * type="text" 而不是 type="number"：数字输入框在非法输入下会把 value 报
 * 成空串，于是"我打错了"与"我清空了"在数据层面无法区分，而清空一个超时
 * 的含义是关掉超时保护。校验统一由 buildSettingsWrite 做，界面只负责把
 * 用户打进去的字符原样交出去。
 */
function NumberField({ label, hint, afterRestart, value, onChange }: {
  label: string
  hint: string
  afterRestart: boolean
  value: string
  onChange: (v: string) => void
}) {
  return (
    <label className="block">
      <span style={fieldLabelStyle}>
        {label}
        {afterRestart && (
          <span style={{ marginLeft: 6, color: 'var(--verdict-unknown)' }}>· 重启后生效</span>
        )}
      </span>
      <input
        className="ctl w-full"
        value={value}
        inputMode="numeric"
        onChange={(e) => onChange(e.target.value)}
        style={{ width: '100%', fontFamily: 'var(--mono)' }}
      />
      <span className="mt-1 block text-xs text-ink-muted">
        {hint}
      </span>
    </label>
  )
}


function RestartNotice({ children }: { children: ReactNode }) {
  return (
    // **不用判定语义色。** 这里此前拿 UNKNOWN 的琥珀色表示「需要重启」——
    // 而 ALLOW / DENY / UNKNOWN 各自唯一，一旦被挪去表示别的东西，用户就
    // 再也无法从颜色读出判定结论（tokens.css 抬头）。要强调靠文案自己说。
    <p className="mt-3 rounded-card border border-line-strong bg-sunken px-3 py-2 text-sm text-ink-2">
      {children}
    </p>
  )
}


