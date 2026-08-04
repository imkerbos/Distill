import { useEffect, useState, type ReactNode } from 'react'
import { NavLink, useNavigate } from 'react-router-dom'
import { api } from '../api/client'
import type { ClusterSummary } from '../api/types'
import { useSession } from '../auth/SessionContext'

interface Props {
  cluster: string
  onClusterChange: (id: string) => void
  children: ReactNode
}

export default function AppShell({ cluster, onClusterChange, children }: Props) {
  const { identity, logout } = useSession()
  const [clusters, setClusters] = useState<ClusterSummary[]>([])
  const navigate = useNavigate()

  useEffect(() => {
    api.clusters().then((cs) => {
      setClusters(cs)
      if (!cluster && cs.length > 0) onClusterChange(cs[0].id)
    }).catch(() => setClusters([]))
  }, [cluster, onClusterChange])

  return (
    <div style={{ display: 'grid', gridTemplateColumns: '220px 1fr', minHeight: '100vh' }}>
      <nav style={{
        borderRight: '1px solid var(--border)', background: 'var(--surface)',
        padding: 'var(--space-4) var(--space-3)', display: 'flex', flexDirection: 'column',
      }}>
        <div style={{ fontWeight: 600, fontSize: 16, marginBottom: 'var(--space-4)' }}>Distill</div>

        <label style={{ display: 'block', marginBottom: 'var(--space-4)' }}>
          <span style={{ display: 'block', fontSize: 12, color: 'var(--text-muted)', marginBottom: 4 }}>
            集群
          </span>
          <select
            value={cluster}
            onChange={(e) => onClusterChange(e.target.value)}
            style={{
              width: '100%', padding: '6px 8px', fontSize: 13,
              border: '1px solid var(--border)', borderRadius: 'var(--radius)',
              background: 'var(--surface)',
            }}
          >
            {clusters.map((c) => (
              <option key={c.id} value={c.id}>{c.id}</option>
            ))}
          </select>
        </label>

        {[
          { to: '/topology', label: '网络拓扑' },
          { to: '/flows', label: '流量与判定' },
          { to: '/quality', label: '数据质量' },
        ].map((item) => (
          <NavLink
            key={item.to}
            to={item.to}
            style={({ isActive }) => ({
              padding: '8px 10px', marginBottom: 2, fontSize: 14,
              borderRadius: 'var(--radius)', textDecoration: 'none',
              color: isActive ? 'var(--text)' : 'var(--text-muted)',
              background: isActive ? 'var(--bg)' : 'transparent',
              fontWeight: isActive ? 500 : 400,
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

      <div style={{ padding: 'var(--space-4)', overflow: 'auto' }}>{children}</div>
    </div>
  )
}
