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
      background: 'var(--bg)',
    }}>
      <form onSubmit={submit} style={{
        width: 360, padding: 'var(--space-5)', background: 'var(--surface)',
        border: '1px solid var(--border)', borderRadius: 'var(--radius)',
      }}>
        <h1 style={{ margin: 0, fontSize: 20, fontWeight: 600 }}>Distill</h1>
        <p style={{ margin: '4px 0 var(--space-4)', color: 'var(--text-muted)', fontSize: 13 }}>
          网络策略可见性与安全平台
        </p>

        <label style={{ display: 'block', marginBottom: 'var(--space-3)' }}>
          <span style={{ display: 'block', marginBottom: 'var(--space-1)', fontSize: 13 }}>用户名</span>
          <input
            value={username} onChange={(e) => setUsername(e.target.value)}
            autoComplete="username" autoFocus required
            style={inputStyle}
          />
        </label>

        <label style={{ display: 'block', marginBottom: 'var(--space-4)' }}>
          <span style={{ display: 'block', marginBottom: 'var(--space-1)', fontSize: 13 }}>密码</span>
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
          width: '100%', padding: '10px', fontSize: 14, fontWeight: 500,
          color: 'var(--text-on-dark)', background: busy ? 'var(--text-muted)' : 'var(--text)',
          border: 'none', borderRadius: 'var(--radius)',
          cursor: busy ? 'default' : 'pointer',
        }}>
          {busy ? '登录中…' : '登录'}
        </button>
      </form>
    </div>
  )
}

const inputStyle: React.CSSProperties = {
  width: '100%', padding: '8px 10px', fontSize: 14,
  border: '1px solid var(--border)', borderRadius: 'var(--radius)',
  background: 'var(--surface)', color: 'var(--text)',
}
