import { SessionProvider, useSession } from './auth/SessionContext'
import LoginPage from './pages/LoginPage'

function Shell() {
  const { identity, loading, logout } = useSession()

  if (loading) return <div style={{ padding: 'var(--space-5)' }}>加载中…</div>
  if (!identity) return <LoginPage />

  return (
    <main style={{ padding: 'var(--space-5)' }}>
      <p>已登录：{identity.username}</p>
      <button onClick={logout}>登出</button>
    </main>
  )
}

/** App 是应用根组件。Task 4 用真实路由替换 Shell 的占位内容。 */
export default function App() {
  return (
    <SessionProvider>
      <Shell />
    </SessionProvider>
  )
}
