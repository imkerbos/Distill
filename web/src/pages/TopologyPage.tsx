import { useState } from 'react'
import { api } from '../api/client'
import type { Topology, TopologyLevel } from '../api/types'
import { useResource } from '../api/useResource'
import TopologyGraph from '../components/TopologyGraph'

export default function TopologyPage({ cluster }: { cluster: string }) {
  const [level, setLevel] = useState<TopologyLevel>('namespace')
  // key 里带上 level：粒度切换要触发重新取数，且旧粒度的响应必须被丢弃，
  // 否则会出现"选了 workload、看到的是 namespace"这种界面与数据不符。
  const { data: topo, error, loading } = useResource(
    `${cluster}:${level}`,
    () => api.topology(cluster, level),
  )

  if (error) return <p style={{ color: 'var(--verdict-deny)' }}>{error}</p>
  if (loading || !topo) return <p style={{ color: 'var(--text-muted)' }}>加载中…</p>

  return (
    <div>
      <h2 style={{ marginTop: 0 }}>网络拓扑</h2>

      <label style={{ fontSize: 13, color: 'var(--text-muted)' }}>
        粒度{' '}
        <select
          value={level}
          onChange={(e) => setLevel(e.target.value as TopologyLevel)}
          style={{ marginRight: 'var(--space-3)' }}
        >
          <option value="namespace">Namespace</option>
          <option value="workload">Workload</option>
        </select>
      </label>

      {/*
        无法定位的流量必须显式说明。静默丢弃会让这一屏与数据质量页
        给出两个不同的总数，而运维无从判断哪个是完整的。
      */}
      {topo.unplaceableFlowCount > 0 && (
        <p style={{
          padding: 'var(--space-2) var(--space-3)', marginBottom: 'var(--space-3)',
          background: 'var(--verdict-unknown-bg)', color: 'var(--verdict-unknown)',
          borderRadius: 'var(--radius)', fontSize: 13,
        }}>
          另有 <strong>{topo.unplaceableFlowCount}</strong> 条流量因端点身份缺失无法定位到命名空间，
          未画在图上。它们仍计入数据质量页的总数。
        </p>
      )}

      <TopologyGraph topology={topo} />

      <DirectionTable topo={topo} />

      <div style={{ marginTop: 'var(--space-3)', fontSize: 12, color: 'var(--text-muted)', display: 'flex', gap: 'var(--space-4)', flexWrap: 'wrap' }}>
        <span><Swatch color="var(--verdict-allow)" /> 放行</span>
        <span><Swatch color="var(--verdict-deny)" /> 阻断</span>
        <span><Swatch color="var(--verdict-unknown)" /> 无法判定</span>
        <span><Swatch color="var(--degraded-stroke)" width={3} /> 粗线＝可信度降级</span>
        <span>虚线＝跨集群</span>
        <span>紫色描边节点＝在 mesh 中</span>
        <span>虚线圆＝其他集群的命名空间</span>
      </div>
    </div>
  )
}

/**
 * 按节点拆出入向与出向。
 *
 * NetworkPolicy 是有方向的：一个 namespace 可以对入向隔离、对出向完全开放，
 * 只看一张无向的图读不出这件事。表里同时给出"这条边是哪一侧判的"——
 * 一条 DENY 边该改源端的 egress 还是目的端的 ingress，只看边本身答不出来。
 */
function DirectionTable({ topo }: { topo: Topology }) {
  const ids = Array.from(
    new Set(topo.edges.flatMap((e) => [e.source, e.target])),
  ).sort()
  if (ids.length === 0) return null

  const short = (id: string) => id.split('/').slice(1).join('/')

  return (
    <section style={{ marginTop: 'var(--space-5)' }}>
      <h3 style={{ fontSize: 14, marginBottom: 'var(--space-2)' }}>入向 / 出向明细</h3>
      <table style={{ borderCollapse: 'collapse', fontSize: 13, minWidth: 520 }}>
        <thead>
          <tr style={{ textAlign: 'left', color: 'var(--text-muted)', fontSize: 12 }}>
            <th style={cell}>节点</th>
            <th style={cell}>入向</th>
            <th style={cell}>出向</th>
            <th style={cell}>判定方向</th>
          </tr>
        </thead>
        <tbody>
          {ids.map((id) => {
            const inbound = topo.edges.filter((e) => e.target === id)
            const outbound = topo.edges.filter((e) => e.source === id)
            const dirs = Array.from(
              new Set([...inbound, ...outbound].map((e) => e.decidedBy).filter(Boolean)),
            )
            return (
              <tr key={id}>
                <td style={cell}>{short(id)}</td>
                <td style={cell}>
                  {inbound.length} 条边 / {inbound.reduce((n, e) => n + e.flowCount, 0)} 条流量
                </td>
                <td style={cell}>
                  {outbound.length} 条边 / {outbound.reduce((n, e) => n + e.flowCount, 0)} 条流量
                </td>
                <td style={{ ...cell, color: 'var(--text-muted)' }}>
                  {dirs.length > 0 ? dirs.join('、') : '—'}
                </td>
              </tr>
            )
          })}
        </tbody>
      </table>
      <p style={{ fontSize: 12, color: 'var(--text-muted)' }}>
        判定方向指做出该判定的是哪一侧的策略：DENY 边为 INGRESS 时改目的端规则，
        为 EGRESS 时改源端规则。
      </p>
    </section>
  )
}

const cell: React.CSSProperties = {
  padding: 'var(--space-2)',
  borderBottom: '1px solid var(--border)',
  fontWeight: 400,
}

function Swatch({ color, width = 2 }: { color: string; width?: number }) {
  return <span style={{
    display: 'inline-block', width: 18, height: width,
    background: color, verticalAlign: 'middle', marginRight: 4,
  }} />
}
