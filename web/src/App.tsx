import { useState } from 'react'
import { BrowserRouter, Navigate, Route, Routes } from 'react-router-dom'
import AppShell from './components/AppShell'
import { SessionProvider, useSession } from './auth/SessionContext'
import AccountsPage, { OwnPasswordPage } from './pages/AccountsPage'
import ClustersPage from './pages/ClustersPage'
import CollectionPage from './pages/CollectionPage'
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
  // 会话未定时不能判定未登录：那会让刷新页面的已登录用户先闪一下登录页。
  // 这里刻意留空而不是画骨架 —— 骨架给的是"版面的形状"，而此刻连要画哪
  // 一屏都还不知道。
  if (loading) return <div />
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
        {/*
          资产采集是这块界面上唯一显示真实集群数据的一屏，因此接 cluster。
          这条路由**不按角色摘掉**，理由同 /accounts：摘掉它，只读账号敲进
          这个地址会被「* → /topology」悄悄弹走，那看起来像地址打错了。
          服务端对这条端点声明 accessAdmin，让页面自己把拒绝显示出来
          （规范 §34）。
        */}
        <Route path="/collection" element={<CollectionPage cluster={cluster} />} />
        {/* 策略仓库独立于集群存在，因此与设置页同理，不接 cluster。 */}
        <Route path="/git-repos" element={<GitReposPage />} />
        {/* 设置是平台自身的配置，不属于任何一个集群，因此不接 cluster。 */}
        <Route path="/settings" element={<SettingsPage />} />
        {/*
          账号与集群无关，因此同样不接 cluster。
          这条路由**不按角色摘掉**：摘掉它，只读账号敲进这个地址会被
          「* → /topology」悄悄弹走，那看起来像地址打错了。让页面自己说
          「服务端会拒绝这一页上的每一个请求」，说的是实情（规范 §34）。
        */}
        <Route path="/accounts" element={<AccountsPage />} />
        {/* 改自己的密码任何角色都能做，因此不在任何角色判断里。 */}
        <Route path="/me/password" element={<OwnPasswordPage />} />
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
