import { useEffect, useMemo, useRef, useState } from 'react'
import {
  forceCenter, forceLink, forceManyBody, forceSimulation,
  type SimulationLinkDatum, type SimulationNodeDatum,
} from 'd3-force'
import type { Topology, TopologyEdge, TopologyNode, Verdict } from '../api/types'

const W = 900
const H = 520

const EDGE_COLOR: Record<Verdict, string> = {
  ALLOW: 'var(--verdict-allow)',
  DENY: 'var(--verdict-deny)',
  UNKNOWN: 'var(--verdict-unknown)',
}

type SimNode = TopologyNode & SimulationNodeDatum
type SimLink = SimulationLinkDatum<SimNode> & { edge: TopologyEdge }

/**
 * 固定初始位置，让力导向布局可复现。
 *
 * d3-force 默认用随机初值，同一份数据每次刷新会得到不同的图。
 * 用户无法建立空间记忆，也无法截图对照 —— 对一个要建立信任感的
 * 界面来说，图形每次都变本身就是一种不可信。这里按节点 id 的哈希
 * 铺在一个圆上作为确定的初值。
 */
function seedPositions(nodes: SimNode[]) {
  nodes.forEach((n, i) => {
    let h = 0
    for (const ch of n.id) h = (h * 31 + ch.charCodeAt(0)) | 0
    const angle = ((Math.abs(h) % 360) + i * 37) * (Math.PI / 180)
    n.x = W / 2 + Math.cos(angle) * 180
    n.y = H / 2 + Math.sin(angle) * 140
  })
}

export default function TopologyGraph({
  topology, onSelectEdge,
}: { topology: Topology; onSelectEdge?: (e: TopologyEdge) => void }) {
  const [, forceRender] = useState(0)
  const nodesRef = useRef<SimNode[]>([])
  const linksRef = useRef<SimLink[]>([])

  const key = useMemo(
    () => topology.nodes.map((n) => n.id).join('|') + '#' + topology.edges.length,
    [topology],
  )

  useEffect(() => {
    const nodes: SimNode[] = topology.nodes.map((n) => ({ ...n }))
    const byID = new Map(nodes.map((n) => [n.id, n]))
    const links: SimLink[] = topology.edges
      .filter((e) => byID.has(e.source) && byID.has(e.target))
      .map((e) => ({ source: byID.get(e.source)!, target: byID.get(e.target)!, edge: e }))

    seedPositions(nodes)

    const sim = forceSimulation(nodes)
      .force('link', forceLink<SimNode, SimLink>(links).id((d) => d.id).distance(170))
      .force('charge', forceManyBody().strength(-420))
      .force('center', forceCenter(W / 2, H / 2))
      .stop()

    // 同步跑固定轮数而非动画：结果确定，且没有节点缓缓漂移的过程 ——
    // 动画在这里只会让人以为图还在变。
    sim.tick(300)

    nodesRef.current = nodes
    linksRef.current = links
    forceRender((v) => v + 1)
  }, [key, topology])

  return (
    <svg width={W} height={H} style={{
      background: 'var(--surface)', border: '1px solid var(--border)',
      borderRadius: 'var(--radius)',
    }}>
      {linksRef.current.map((l, i) => {
        const s = l.source as SimNode
        const t = l.target as SimNode
        const e = l.edge
        return (
          <g key={i} onClick={() => onSelectEdge?.(e)} style={{ cursor: onSelectEdge ? 'pointer' : 'default' }}>
            <line
              x1={s.x} y1={s.y} x2={t.x} y2={t.y}
              stroke={EDGE_COLOR[e.verdict]}
              strokeWidth={e.confidence === 'DEGRADED' ? 3 : 1.5}
              strokeDasharray={e.crossCluster ? '6 4' : undefined}
              opacity={0.85}
            />
            <title>
              {`${e.source} → ${e.target}\n${e.verdict}${e.confidence === 'DEGRADED' ? '（降级）' : ''}`
                + `\n${e.flowCount} 条流量  端口 ${e.ports.join(', ')}`
                + (e.crossCluster ? '\n跨集群' : '') + (e.unmanaged ? '\n含不受管控端点' : '')}
            </title>
          </g>
        )
      })}

      {nodesRef.current.map((n) => (
        <g key={n.id}>
          <circle
            cx={n.x} cy={n.y} r={n.foreign ? 8 : 12}
            fill={n.foreign ? 'var(--bg)' : 'var(--surface)'}
            stroke={n.inMesh ? 'var(--degraded-stroke)' : 'var(--border)'}
            strokeWidth={n.inMesh ? 2 : 1}
            strokeDasharray={n.foreign ? '3 3' : undefined}
          />
          <text
            x={n.x} y={(n.y ?? 0) + 26} textAnchor="middle"
            fontSize={12} fill={n.foreign ? 'var(--text-muted)' : 'var(--text)'}
          >{n.namespace}</text>
          {!n.hasPolicy && !n.foreign && (
            <text x={n.x} y={(n.y ?? 0) + 40} textAnchor="middle" fontSize={10} fill="var(--verdict-deny)">
              无策略
            </text>
          )}
          <title>
            {`${n.id}\n${n.podCount} 个 Pod`
              + (n.inMesh ? '\n在 service mesh 中：L4 身份被 sidecar 遮蔽' : '')
              + (n.hasPolicy ? '' : '\n没有任何 NetworkPolicy')
              + (n.unmanagedPodCount > 0 ? `\n${n.unmanagedPodCount} 个 hostNetwork Pod 不受管控` : '')
              + (n.foreign ? '\n其他集群的命名空间，本集群策略管不到它' : '')}
          </title>
        </g>
      ))}
    </svg>
  )
}
