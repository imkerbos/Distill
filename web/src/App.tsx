import { useState } from 'react'
import { BrowserRouter, Navigate, Route, Routes } from 'react-router-dom'
import AppShell from './components/AppShell'
import { SessionProvider, useSession } from './auth/SessionContext'
import ClustersPage from './pages/ClustersPage'
import FlowsPage from './pages/FlowsPage'
import GitReposPage from './pages/GitReposPage'
import LoginPage from './pages/LoginPage'
import PolicyPage from './pages/PolicyPage'
import QualityPage from './pages/QualityPage'
import SecurityPage from './pages/SecurityPage'
import SettingsPage from './pages/SettingsPage'
import TopologyPage from './pages/TopologyPage'

function Protected() {
  const { identity, loading } = useSession()
  const [cluster, setCluster] = useState('')

  // loading 期间不能判定未登录：那会让刷新页面的已登录用户先闪一下登录页。
  if (loading) return <div style={{ padding: 'var(--space-5)' }}>加载中…</div>
  if (!identity) return <Navigate to="/login" replace />

  return (
    <AppShell cluster={cluster} onClusterChange={setCluster}>
      <Routes>
        <Route path="/topology" element={<TopologyPage cluster={cluster} />} />
        <Route path="/flows" element={<FlowsPage cluster={cluster} />} />
        <Route path="/quality" element={<QualityPage cluster={cluster} />} />
        <Route path="/security" element={<SecurityPage cluster={cluster} />} />
        <Route path="/policy" element={<PolicyPage cluster={cluster} />} />
        <Route path="/clusters" element={<ClustersPage />} />
        {/* 策略仓库独立于集群存在，因此与设置页同理，不接 cluster。 */}
        <Route path="/git-repos" element={<GitReposPage />} />
        {/* 设置是平台自身的配置，不属于任何一个集群，因此不接 cluster。 */}
        <Route path="/settings" element={<SettingsPage />} />
        <Route path="*" element={<Navigate to="/topology" replace />} />
      </Routes>
    </AppShell>
  )
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
