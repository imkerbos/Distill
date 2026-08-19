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
        <div className="mb-4">
          <h1
            className="m-0 text-2xl tracking-[-0.02em] text-ink"
            style={{ fontWeight: 'var(--weight-title)' }}
          >
            Distill
          </h1>
          <p className="mt-1 mb-0 text-sm text-ink-muted">
            GKE NetworkPolicy 可见性与安全平台
          </p>
        </div>

        <form
          onSubmit={submit}
          className="rounded-card border border-line bg-surface p-4 shadow-card"
        >
          <label className="mb-3 block">
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
        */}
        <p className="mt-3 text-xs leading-relaxed text-ink-muted">
          当前为 demo 数据集，不连接真实集群，不下发任何策略。
        </p>
      </div>
    </div>
  )
}
