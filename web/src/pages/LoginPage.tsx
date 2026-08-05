import { useState, type FormEvent } from 'react'
import { ApiError } from '../api/client'
import { useSession } from '../auth/SessionContext'

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
    <div style={{
      minHeight: '100vh', display: 'grid', placeItems: 'center',
      background: 'var(--bg)', padding: 'var(--space-4)',
    }}>
      <div style={{ width: 380 }}>
        {/*
          品牌与说明放在卡片外：登录框本身只承担"填两个字段"这一件事。
          把介绍塞进卡片会让第一屏显得在推销，而这是一个内部平台 ——
          它需要的是让人一眼确认"进对地方了"。
        */}
        <div style={{ marginBottom: 'var(--space-4)' }}>
          <h1 style={{
            margin: 0, fontSize: 'var(--text-2xl)', fontWeight: 600, letterSpacing: '-0.01em',
          }}>
            Distill
          </h1>
          <p style={{
            margin: 'var(--space-1) 0 0', color: 'var(--text-muted)', fontSize: 'var(--text-sm)',
          }}>
            GKE NetworkPolicy 可见性与安全平台
          </p>
        </div>

      <form onSubmit={submit} style={{
        padding: 'var(--space-4)', background: 'var(--surface)',
        border: '1px solid var(--border)', borderRadius: 'var(--radius)',
        boxShadow: 'var(--shadow-card)',
      }}>

        <label style={{ display: 'block', marginBottom: 'var(--space-3)' }}>
          <span style={{
            display: 'block', marginBottom: 'var(--space-1)',
            fontSize: 'var(--text-sm)', color: 'var(--text-secondary)',
          }}>用户名</span>
          <input
            value={username} onChange={(e) => setUsername(e.target.value)}
            autoComplete="username" autoFocus required
            style={inputStyle}
          />
        </label>

        <label style={{ display: 'block', marginBottom: 'var(--space-4)' }}>
          <span style={{
            display: 'block', marginBottom: 'var(--space-1)',
            fontSize: 'var(--text-sm)', color: 'var(--text-secondary)',
          }}>密码</span>
          <input
            type="password" value={password} onChange={(e) => setPassword(e.target.value)}
            autoComplete="current-password" required
            style={inputStyle}
          />
        </label>

        {error && (
          <p role="alert" style={{
            margin: '0 0 var(--space-3)', padding: 'var(--space-2)',
            background: 'var(--verdict-deny-bg)', color: 'var(--verdict-deny)',
            borderRadius: 'var(--radius)', fontSize: 13,
          }}>{error}</p>
        )}

        <button type="submit" disabled={busy} style={{
          width: '100%', padding: '10px', fontSize: 'var(--text-base)', fontWeight: 500,
          color: 'var(--text-on-dark)', background: busy ? 'var(--text-muted)' : 'var(--accent)',
          border: 'none', borderRadius: 'var(--radius-sm)',
          cursor: busy ? 'default' : 'pointer',
        }}>
          {busy ? '登录中…' : '登录'}
        </button>
      </form>

      {/*
        平台边界写在登录页：使用者在看到任何数字之前就该知道这些数字
        意味着什么。一个不下发策略的只读平台，与一个能改集群的平台，
        使用者面对它们的心态完全不同。
      */}
      <p style={{
        margin: 'var(--space-3) 0 0', fontSize: 'var(--text-xs)',
        color: 'var(--text-muted)', lineHeight: 1.7,
      }}>
        当前为 demo 数据集，不连接真实集群，不下发任何策略。
      </p>
      </div>
    </div>
  )
}

const inputStyle: React.CSSProperties = {
  width: '100%', padding: '9px 11px', fontSize: 'var(--text-base)',
  border: '1px solid var(--border-strong)', borderRadius: 'var(--radius-sm)',
  background: 'var(--surface)', color: 'var(--text)',
}
