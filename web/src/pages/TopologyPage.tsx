import { useState } from 'react'
import { api } from '../api/client'
import type { Topology, TopologyLevel } from '../api/types'
import { useResource } from '../api/useResource'
import TopologyGraph from '../components/TopologyGraph'
import { Card, Chip, Field, Notice, PageHeader, Section, TableCard, Toolbar } from '../components/ui'

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
      <PageHeader
        title="网络拓扑"
        description="集群内与跨集群的通信关系。NetworkPolicy 是有方向的，因此边上标注了做出判定的是哪一侧的策略。"
      />

      <Toolbar>
        <Field label="粒度">
          <select
            value={level}
            onChange={(e) => setLevel(e.target.value as TopologyLevel)}
            style={{
              padding: '4px 8px', border: '1px solid var(--border)',
              borderRadius: 'var(--radius-sm)', background: 'var(--surface)',
              fontSize: 'var(--text-sm)',
            }}
          >
            <option value="namespace">Namespace</option>
            <option value="workload">Workload</option>
          </select>
        </Field>
      </Toolbar>

      {/*
        无法定位的流量必须显式说明。静默丢弃会让这一屏与数据质量页
        给出两个不同的总数，而运维无从判断哪个是完整的。
      */}
      {topo.unplaceableFlowCount > 0 && (
        <Notice>
          另有 <strong>{topo.unplaceableFlowCount}</strong> 条流量因端点身份缺失无法定位到节点，
          未画在图上。它们仍计入数据质量页的总数。
        </Notice>
      )}

      <Card style={{ padding: 'var(--space-3)' }}>
        <TopologyGraph topology={topo} />

        <div style={{
          marginTop: 'var(--space-3)', fontSize: 'var(--text-xs)', color: 'var(--text-muted)',
          display: 'flex', gap: 'var(--space-4)', flexWrap: 'wrap',
        }}>
          <span><Swatch color="var(--verdict-allow)" /> 放行</span>
          <span><Swatch color="var(--verdict-deny)" /> 阻断</span>
          <span><Swatch color="var(--verdict-unknown)" /> 无法判定</span>
          <span><Swatch color="var(--degraded-stroke)" width={3} /> 粗线＝可信度降级</span>
          <span>虚线＝跨集群</span>
          <span>紫色描边节点＝在 mesh 中</span>
          <span>虚线圆＝其他集群的节点</span>
        </div>
      </Card>

      <div style={{ marginTop: 'var(--space-5)' }}>
        <DirectionTable topo={topo} />
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
    <Section
      title="入向 / 出向明细"
      description="NetworkPolicy 是有方向的：一个 namespace 可以对入向隔离、对出向完全开放，只看一张无向的图读不出这件事。判定方向指做出该判定的是哪一侧的策略 —— DENY 边为 INGRESS 时改目的端规则，为 EGRESS 时改源端规则。"
    >
      <TableCard>
        <thead>
          <tr>
            <th>节点</th>
            <th className="num">入向</th>
            <th className="num">出向</th>
            <th>判定方向</th>
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
                <td className="mono">{short(id)}</td>
                <td className="num">
                  {inbound.length} 边 / {inbound.reduce((n, e) => n + e.flowCount, 0)} 流
                </td>
                <td className="num">
                  {outbound.length} 边 / {outbound.reduce((n, e) => n + e.flowCount, 0)} 流
                </td>
                <td>{dirs.length > 0 ? dirs.map((d) => <Chip key={d}>{d}</Chip>) : '—'}</td>
              </tr>
            )
          })}
        </tbody>
      </TableCard>
    </Section>
  )
}


function Swatch({ color, width = 2 }: { color: string; width?: number }) {
  return <span style={{
    display: 'inline-block', width: 18, height: width,
    background: color, verticalAlign: 'middle', marginRight: 4,
  }} />
}
