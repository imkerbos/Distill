import { useState } from 'react'
import { api } from '../api/client'
import type { Topology, TopologyLevel } from '../api/types'
import { useResource } from '../api/useResource'
import TopologyGraph from '../components/TopologyGraph'
import { Card, Chip, Field, Notice, PageHeader, Section, Select, TableCard, Toolbar } from '../components/ui'

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
          <Select
            value={level}
            ariaLabel="拓扑粒度"
            onChange={(v) => setLevel(v as TopologyLevel)}
            options={[['namespace', 'Namespace'], ['workload', 'Workload']]}
          />
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

      {/*
        图与侧栏并排：七八个节点的图并不需要一千多像素宽，硬把连线拉长
        只会显得刻意。多出来的宽度拿来放图例与概览 —— 横排的图例在宽屏上
        会拉成一长条，反而不好读。
      */}
      <Card style={{ padding: 'var(--space-3)', display: 'flex', gap: 'var(--space-4)' }}>
        <div style={{ flex: 1, minWidth: 0 }}>
          <TopologyGraph topology={topo} />
        </div>

        <aside style={{
          width: 216, flexShrink: 0, borderLeft: '1px solid var(--border)',
          paddingLeft: 'var(--space-3)', fontSize: 'var(--text-xs)',
        }}>
          <GraphSummary topo={topo} />

          <div style={{
            marginTop: 'var(--space-4)', color: 'var(--text-muted)',
            display: 'flex', flexDirection: 'column', gap: 6,
          }}>
            <span><Swatch color="var(--verdict-allow)" /> 放行</span>
            <span><Swatch color="var(--verdict-deny)" /> 阻断</span>
            <span><Swatch color="var(--verdict-unknown)" /> 无法判定</span>
            <span><Swatch color="var(--degraded-stroke)" width={3} /> 粗线＝可信度降级</span>
            <span>虚线＝跨集群</span>
            <span>紫色描边节点＝在 mesh 中</span>
            <span>虚线圆＝其他集群的节点</span>
            <span>节点大小＝Pod 数量</span>
          </div>
        </aside>
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


/**
 * 图的规模概览。
 *
 * 放在图旁边而不是图下方：读者判断"这张图值不值得细看"靠的是这几个数，
 * 而不是先把图看完。跨集群与无策略单列，它们是这一屏真正要传达的敞口。
 */
function GraphSummary({ topo }: { topo: Topology }) {
  const crossCluster = topo.edges.filter((e) => e.crossCluster).length
  const noPolicy = topo.nodes.filter((n) => !n.foreign && !n.hasPolicy).length
  const foreign = topo.nodes.filter((n) => n.foreign).length

  return (
    <dl style={{
      margin: 0, display: 'grid', gridTemplateColumns: '1fr auto',
      rowGap: 6, columnGap: 'var(--space-2)',
    }}>
      <Item k="节点" v={`${topo.nodes.length - foreign}`} />
      <Item k="其他集群节点" v={`${foreign}`} />
      <Item k="边" v={`${topo.edges.length}`} />
      <Item k="跨集群边" v={`${crossCluster}`} />
      <Item k="无策略节点" v={`${noPolicy}`} />
    </dl>
  )
}

function Item({ k, v }: { k: string; v: string }) {
  return (
    <>
      <dt style={{ color: 'var(--text-muted)' }}>{k}</dt>
      <dd style={{ margin: 0, fontWeight: 600, fontVariantNumeric: 'tabular-nums' }}>{v}</dd>
    </>
  )
}

function Swatch({ color, width = 2 }: { color: string; width?: number }) {
  return <span style={{
    display: 'inline-block', width: 18, height: width,
    background: color, verticalAlign: 'middle', marginRight: 4,
  }} />
}
