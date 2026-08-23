import { useState, type FormEvent } from 'react'
import { ApiError } from '../api/client'
import { useSession } from '../auth/SessionContext'
import { Button } from '../components/ui'

export default function LoginPage() {
  const { login } = useSession()
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  async function submit(e: FormEvent) {
    e.preventDefault()
    setError('')
    setBusy(true)
    try {
      await login(username, password)
    } catch (err) {
      // 后端对"用户不存在"与"密码错误"返回同一个 code 与同一句文案，
      // 前端原样展示即可 —— 自行区分会重新打开账号枚举的口子。
      setError(err instanceof ApiError ? err.msg : '登录失败，请稍后重试')
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="grid min-h-screen place-items-center bg-bg p-4">
      <div className="w-[380px]">
        {/*
          品牌与说明放在卡片外：登录框本身只承担"填两个字段"这一件事。
          把介绍塞进卡片会让第一屏显得在推销，而这是一个内部平台 ——
          它需要的是让人一眼确认"进对地方了"。
        */}
        <div className="mb-5 flex items-center gap-3">
          <BrandGlyph />
          <div>
            <h1 className="m-0 text-2xl font-title tracking-[-0.02em] text-ink">Distill</h1>
            <p className="m-0 text-sm text-ink-muted">GKE NetworkPolicy 可见性与安全平台</p>
          </div>
        </div>

        <form
          onSubmit={submit}
          className="rounded-card border border-line bg-surface p-5 shadow-card"
        >
          <label className="mb-4 block">
            <span className="mb-1 block text-sm text-ink-2">用户名</span>
            <input
              className="ctl w-full"
              value={username} onChange={(e) => setUsername(e.target.value)}
              autoComplete="username" autoFocus required
            />
          </label>

          <label className="mb-4 block">
            <span className="mb-1 block text-sm text-ink-2">密码</span>
            <input
              className="ctl w-full"
              type="password" value={password} onChange={(e) => setPassword(e.target.value)}
              autoComplete="current-password" required
            />
          </label>

          {/* 失败原因不分「用户不存在」与「密码错误」：后端返回同一个码与
              同一句文案，前端原样展示 —— 自行区分会重新打开账号枚举的口子。 */}
          {error && (
            <p
              role="alert"
              className="mt-0 mb-3 rounded-card px-3 py-2 text-sm"
              style={{ background: 'var(--verdict-deny-bg)', color: 'var(--verdict-deny)' }}
            >
              {error}
            </p>
          )}

          <Button type="submit" disabled={busy} className="w-full justify-center py-[9px]">
            {busy ? '登录中…' : '登录'}
          </Button>
        </form>

        {/*
          平台边界写在登录页：使用者在看到任何数字之前就该知道这些数字
          意味着什么。一个不下发策略的只读平台，与一个能改集群的平台，
          使用者面对它们的心态完全不同。

          不再说「demo 数据集/不连接真实集群」——那已经不是实话（平台上有
          真实采集的集群）。留下的是仍然为真、且真正要让人放心的那一半：
          读取用于分析，从不写集群。
        */}
        <p className="mt-4 flex items-start gap-2 text-xs leading-relaxed text-ink-muted">
          <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2"
            strokeLinecap="round" strokeLinejoin="round" className="mt-[2px] shrink-0" aria-hidden>
            <path d="M20 13c0 5-3.5 7.5-7.66 8.95a1 1 0 0 1-.67-.01C7.5 20.5 4 18 4 13V6a1 1 0 0 1 1-1c2 0 4.5-1.2 6.24-2.72a1.17 1.17 0 0 1 1.52 0C14.51 3.81 17 5 19 5a1 1 0 0 1 1 1z" />
          </svg>
          <span>只读平台：读取集群资产与流量用于分析，从不下发或修改任何策略。</span>
        </p>
      </div>
    </div>
  )
}

/** 登录页品牌字形：方形徽标里一道蒸馏的收束线，呼应 “Distill”。 */
function BrandGlyph() {
  return (
    <span
      className="flex h-11 w-11 shrink-0 items-center justify-center rounded-card border border-line bg-surface text-ink shadow-card"
      aria-hidden
    >
      <svg width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2"
        strokeLinecap="round" strokeLinejoin="round">
        <path d="M6 3h12" />
        <path d="M8 3v5.5L4.5 15A3.5 3.5 0 0 0 7.6 20h8.8a3.5 3.5 0 0 0 3.1-5L16 8.5V3" />
        <path d="M9 14h6" />
      </svg>
    </span>
  )
}
