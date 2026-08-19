import { useState } from 'react'
import type { ReactNode } from 'react'

import { edgeCountLabel, showsGraph, trafficNotice } from './topologyView.ts'
import { api } from '../api/client'
import type { Topology, TopologyLevel } from '../api/types'
import { useResource } from '../api/useResource'
import DataSourceNotice from '../components/DataSourceNotice'
import TopologyGraph from '../components/TopologyGraph'
import { Card, Chip, Field, Notice, PageHeader, Section, Select, Skeleton, TableCard, Toolbar } from '../components/ui'

export default function TopologyPage({ cluster }: { cluster: string }) {
  const [level, setLevel] = useState<TopologyLevel>('namespace')
  // key 里带上 level：粒度切换要触发重新取数，且旧粒度的响应必须被丢弃，
  // 否则会出现"选了 workload、看到的是 namespace"这种界面与数据不符。
  const { data: topo, error, loading } = useResource(
    `${cluster}:${level}`,
    () => api.topology(cluster, level),
  )

  // 标题与数据来源一起提到早退分支之前，理由见 DataSourceNotice：来源标识
  // 必须与内容同屏，包括这一屏读不到数据的时候（design doc 2026-08-17 §2）。
  const head = (
    <>
      <PageHeader
        title="网络拓扑"
        description="集群内与跨集群的通信关系。NetworkPolicy 是有方向的，因此边上标注了做出判定的是哪一侧的策略。"
      />
      <DataSourceNotice />
    </>
  )

  if (error) return <div>{head}<p className="text-deny">{error}</p></div>
  if (loading || !topo) return <div>{head}<Skeleton /></div>

  return (
    <div>
      {head}

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
      {/*
        没有流量观测时不画那张图（topologyView.showsGraph）：一张没有边的图
        是一堆互不相连的点，看起来像一个"结构清晰、没有耦合"的集群 ——
        而事实是我们还没看过它的通信。节点仍然列出来，它们是真的。
      */}
      {trafficNotice(topo) !== null && (
        <Card style={{
          padding: 'var(--space-3)', marginBottom: 'var(--space-4)',
          borderLeft: '3px solid var(--verdict-unknown)',
        }}>
          <p className="m-0 text-sm">{trafficNotice(topo)}</p>
        </Card>
      )}

      <Card style={{ padding: 'var(--space-3)', display: 'flex', gap: 'var(--space-4)' }}>
        <div className="min-w-0 flex-1">
          {showsGraph(topo)
            ? <TopologyGraph topology={topo} />
            : <NodeList topo={topo} />}
        </div>

        {/* 图例与概览分成两块、各带小标题：此前是一列裸文字，读者要
            自己看出前四行讲颜色、后四行讲形状。 */}
        <aside className="w-[236px] shrink-0 border-l border-line pl-4 text-xs">
          <SideHeading>概览</SideHeading>
          <GraphSummary topo={topo} />

          <SideHeading className="mt-5">线的颜色＝判定</SideHeading>
          <ul className="m-0 flex list-none flex-col gap-[6px] p-0 text-ink-2">
            <li><Swatch color="var(--verdict-allow)" /> 放行</li>
            <li><Swatch color="var(--verdict-deny)" /> 阻断</li>
            <li><Swatch color="var(--verdict-unknown)" /> 无法判定</li>
          </ul>

          <SideHeading className="mt-4">线与点的形状</SideHeading>
          <ul className="m-0 flex list-none flex-col gap-[6px] p-0 text-ink-muted">
            <li><Swatch color="var(--degraded-stroke)" width={3} /> 粗线＝可信度降级</li>
            <li>虚线＝跨集群</li>
            <li>紫色描边＝在 mesh 中</li>
            <li>虚线圆＝其他集群的节点</li>
            <li>圆的大小＝Pod 数量</li>
          </ul>
        </aside>
      </Card>

      <div className="mt-5">
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
    <dl className="m-0 grid grid-cols-[1fr_auto] gap-x-2 gap-y-[6px]">
      <Item k="节点" v={`${topo.nodes.length - foreign}`} />
      <Item k="其他集群节点" v={`${foreign}`} />
      <Item k="边" v={edgeCountLabel(topo)} />
      <Item k="跨集群边" v={`${crossCluster}`} />
      <Item k="无策略节点" v={`${noPolicy}`} />
    </dl>
  )
}

function Item({ k, v }: { k: string; v: string }) {
  return (
    <>
      <dt className="text-ink-muted">{k}</dt>
      <dd className="m-0 font-semibold tabular-nums">{v}</dd>
    </>
  )
}

function Swatch({ color, width = 2 }: { color: string; width?: number }) {
  return <span
    className="mr-2 inline-block w-[18px] rounded-full align-middle"
    style={{ height: width, background: color }}
  />
}

/** 侧栏小标题。图例分块之后需要它，否则两块之间只有一段空白。 */
function SideHeading({ children, className = '' }: { children: ReactNode; className?: string }) {
  return (
    <div className={`mb-2 text-[11px] tracking-[0.06em] text-ink-muted uppercase ${className}`}>
      {children}
    </div>
  )
}

/**
 * 没有图可画时，把节点原样列出来。
 *
 * 列表而不是图：图的形状本身会传达"它们之间是这样连接的"，而这一刻我们
 * 对连接一无所知。列表只说它知道的那件事 —— 这些工作负载存在，其中哪些
 * 没有任何 NetworkPolicy 覆盖。
 */
function NodeList({ topo }: { topo: Topology }) {
  const own = topo.nodes.filter((n) => !n.foreign)
  if (own.length === 0) return null
  return (
    // 走统一的表格外壳，不自己拼一份：三个页面三种表格，读者会以为它们
    // 在讲不同性质的事（ui.tsx 抬头）。
    <TableCard>
      <thead>
        <tr>
          <th>{topo.level === 'workload' ? '工作负载' : '命名空间'}</th>
          <th className="num">Pod 数</th>
          <th>NetworkPolicy</th>
        </tr>
      </thead>
      <tbody>
        {own.map((n) => (
          <tr key={n.id}>
            <td>
              {/* namespace 这一栏已经按粒度带好了展示名：workload 粒度下
                  后端放的是 "namespace/workload"（collectstore.nodeIDOf）。
                  这里不再另拼一次 —— 拼的那一版读的是一个后端从未下发过的
                  字段，因此那个分支从来没有成立过。 */}
              {n.namespace}
            </td>
            <td className="num">{n.podCount}</td>
            <td>
              {n.hasPolicy ? '有' : <span className="text-deny">无</span>}
            </td>
          </tr>
        ))}
      </tbody>
    </TableCard>
  )
}
