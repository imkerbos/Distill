import { useEffect, useRef, useState, type ReactNode } from 'react'
import { NavLink, useNavigate } from 'react-router-dom'
import { api } from '../api/client'
import { Select } from './ui'
import type { RegisteredCluster } from '../api/types'
import { useSession } from '../auth/SessionContext'

interface Props {
  cluster: string
  onClusterChange: (id: string) => void
  children: ReactNode
}

export default function AppShell({ cluster, onClusterChange, children }: Props) {
  const { identity, logout } = useSession()
  const [clusters, setClusters] = useState<RegisteredCluster[]>([])
  const [clustersError, setClustersError] = useState(false)
  const navigate = useNavigate()

  // 只在挂载时取一次集群列表：切换集群不会改变"有哪些集群"这个事实，
  // 把 cluster 放进依赖数组会让每次手动选择都重新拉一遍列表，纯浪费。
  // 用 ref 读最新的 cluster，避免在只运行一次的 effect 里闭包住挂载时的旧值。
  const clusterRef = useRef(cluster)
  clusterRef.current = cluster

  useEffect(() => {
    api.clusters().then((cs) => {
      setClusters(cs)
      setClustersError(false)
      if (!clusterRef.current && cs.length > 0) onClusterChange(cs[0].id)
    }).catch(() => setClustersError(true))
  }, [onClusterChange])

  return (
    <div style={{ display: 'grid', gridTemplateColumns: '220px 1fr', minHeight: '100vh' }}>
      <nav style={{
        borderRight: '1px solid var(--border)', background: 'var(--surface)',
        padding: 'var(--space-4) var(--space-3)', display: 'flex', flexDirection: 'column',
      }}>
        <div style={{
          fontWeight: 600, fontSize: 'var(--text-lg)', letterSpacing: '-0.01em',
          marginBottom: 'var(--space-1)',
        }}>
          Distill
        </div>
        <div style={{
          fontSize: 'var(--text-xs)', color: 'var(--text-muted)',
          marginBottom: 'var(--space-4)',
        }}>
          NetworkPolicy 可见性
        </div>

        <label style={{ display: 'block', marginBottom: 'var(--space-4)' }}>
          <span style={{ display: 'block', fontSize: 12, color: 'var(--text-muted)', marginBottom: 4 }}>
            集群
          </span>
          <Select
            value={cluster}
            ariaLabel="集群"
            onChange={onClusterChange}
            options={clusters.map((c) => [c.id, c.id] as [string, string])}
            style={{ width: '100%' }}
          />
          {clustersError && (
            <span style={{
              display: 'block', marginTop: 4, fontSize: 12, color: 'var(--verdict-deny)',
            }}>
              集群列表加载失败
            </span>
          )}
        </label>

        {[
          { to: '/topology', label: '网络拓扑' },
          { to: '/flows', label: '流量与判定' },
          { to: '/security', label: '安全发现' },
          { to: '/policy', label: '候选策略' },
          { to: '/quality', label: '数据质量' },
          { to: '/clusters', label: '集群管理' },
          { to: '/git-repos', label: '策略仓库' },
          { to: '/settings', label: '平台设置' },
        ].map((item) => (
          <NavLink
            key={item.to}
            to={item.to}
            style={({ isActive }) => ({
              padding: '8px 10px 8px 12px', marginBottom: 2,
              fontSize: 'var(--text-base)',
              borderRadius: 'var(--radius-sm)', textDecoration: 'none',
              color: isActive ? 'var(--text)' : 'var(--text-muted)',
              background: isActive ? 'var(--surface-sunken)' : 'transparent',
              fontWeight: isActive ? 600 : 400,
              // 左侧色条：只靠底色变化的 active 态在浅色系里几乎读不出来，
              // 使用者会不确定自己正在看哪一屏。
              borderLeft: isActive
                ? '3px solid var(--accent)'
                : '3px solid transparent',
            })}
          >
            {item.label}
          </NavLink>
        ))}

        <div style={{ marginTop: 'auto', fontSize: 12, color: 'var(--text-muted)' }}>
          <div style={{ marginBottom: 'var(--space-2)' }}>{identity?.username}</div>
          <button
            onClick={async () => { await logout(); navigate('/login') }}
            style={{
              padding: '4px 8px', fontSize: 12, background: 'transparent',
              border: '1px solid var(--border)', borderRadius: 'var(--radius)',
              cursor: 'pointer', color: 'var(--text-muted)',
            }}
          >登出</button>
        </div>
      </nav>

      {/*
        限制正文宽度：宽屏下表格列会被拉到一两千像素，同一行的源与目的
        相隔太远，读者无法把它们连成一条记录。
      */}
      <div style={{ padding: 'var(--space-5) var(--space-4)', overflow: 'auto' }}>
        <div style={{ maxWidth: 'var(--content-max)' }}>{children}</div>
      </div>
    </div>
  )
}
