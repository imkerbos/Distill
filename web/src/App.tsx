import { useState } from 'react'
import { BrowserRouter, Navigate, Route, Routes } from 'react-router-dom'
import AppShell from './components/AppShell'
import { SessionProvider, useSession } from './auth/SessionContext'
import LoginPage from './pages/LoginPage'

function Protected() {
  const { identity, loading } = useSession()
  const [cluster, setCluster] = useState('')

  // loading 期间不能判定未登录：那会让刷新页面的已登录用户先闪一下登录页。
  if (loading) return <div style={{ padding: 'var(--space-5)' }}>加载中…</div>
  if (!identity) return <Navigate to="/login" replace />

  return (
    <AppShell cluster={cluster} onClusterChange={setCluster}>
      <Routes>
        <Route path="/topology" element={<Placeholder name="网络拓扑" cluster={cluster} />} />
        <Route path="/flows" element={<Placeholder name="流量与判定" cluster={cluster} />} />
        <Route path="/quality" element={<Placeholder name="数据质量" cluster={cluster} />} />
        <Route path="*" element={<Navigate to="/topology" replace />} />
      </Routes>
    </AppShell>
  )
}

/** Placeholder 在 Task 5-7 被真实页面逐个替换。 */
function Placeholder({ name, cluster }: { name: string; cluster: string }) {
  return <div><h2>{name}</h2><p style={{ color: 'var(--text-muted)' }}>集群：{cluster || '（未选择）'}</p></div>
}

function Root() {
  const { identity, loading } = useSession()
  return (
    <Routes>
      <Route
        path="/login"
        element={loading ? <div /> : identity ? <Navigate to="/topology" replace /> : <LoginPage />}
      />
      <Route path="*" element={<Protected />} />
    </Routes>
  )
}

export default function App() {
  return (
    <BrowserRouter>
      <SessionProvider>
        <Root />
      </SessionProvider>
    </BrowserRouter>
  )
}
