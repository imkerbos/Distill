import { useEffect, useMemo, useRef, useState } from 'react'
import {
  forceCenter, forceLink, forceManyBody, forceSimulation, forceX, forceY,
  type SimulationLinkDatum, type SimulationNodeDatum,
} from 'd3-force'
import type { Topology, TopologyEdge, TopologyNode, Verdict } from '../api/types'

// 画布高度固定，宽度随容器实测。
//
// 之前用固定坐标系 + viewBox 缩放：布局在一个想象的画布里算好，再整体
// 塞进真实容器，两者尺寸永远对不上 —— 宽屏下图缩成中间一小团，四周
// 大片空白，而空白并不表示那里没有东西。现在直接按容器像素布局。
const H = 500
const PAD = 56

/**
 * 连线长度按可用尺寸与节点数推导。
 *
 * 关键是方向：节点**越少**，连线应当**越长** —— 少数几个节点摊在大画布里
 * 本就该拉开，而不是挤成一团。先前用 sqrt(面积/n) 的密度公式方向刚好相反，
 * 节点少时反而算出更短的连线，图就缩在中央一小块。
 */
function layoutScale(w: number, n: number) {
  const usable = Math.min(w, H * 1.6)
  const distance = Math.max(110, Math.min(300, (usable / (1 + Math.sqrt(Math.max(n, 1)))) * 1.1))
  return { distance, charge: -distance * 2.4 }
}

/**
 * 把模拟结果铺满画布。
 *
 * 关键是 x / y **独立**缩放。等比缩放会取两个方向的较小倍率，而绘图区
 * 接近 2:1、力导布局天然接近 1:1 —— 于是高度永远是约束边，宽度再宽也
 * 用不上，图被锁在中间一小块。SVG 宽度是 100% 没错，撑满的是画布，
 * 不是画布里的图形。
 *
 * 独立缩放会改变连线的视觉长度，但拓扑图要读的是"谁连着谁"，
 * 长度本身不携带信息（流量大小走线宽，判定走颜色）。为免拉伸到失真，
 * 两个方向的倍率差被限制在 MAX_SKEW 之内。
 */
const MAX_SKEW = 1.7

function fitToCanvas(nodes: SimNode[], W: number) {
  if (nodes.length === 0) return
  const xs = nodes.map((n) => n.x ?? 0)
  const ys = nodes.map((n) => n.y ?? 0)
  const minX = Math.min(...xs), maxX = Math.max(...xs)
  const minY = Math.min(...ys), maxY = Math.max(...ys)
  const spanX = Math.max(maxX - minX, 1)
  const spanY = Math.max(maxY - minY, 1)

  let sx = (W - PAD * 2) / spanX
  let sy = (H - PAD * 2) / spanY
  // 收敛到彼此的 MAX_SKEW 倍以内，避免一个方向被拉得过分。
  const lo = Math.min(sx, sy)
  sx = Math.min(sx, lo * MAX_SKEW)
  sy = Math.min(sy, lo * MAX_SKEW)

  const offX = (W - spanX * sx) / 2 - minX * sx
  const offY = (H - spanY * sy) / 2 - minY * sy
  for (const n of nodes) {
    n.x = (n.x ?? 0) * sx + offX
    n.y = (n.y ?? 0) * sy + offY
  }
}

/**
 * 节点半径按 Pod 数量分级，让"哪里承载得多"在图上直接可读。
 *
 * **外部节点不参与分级**：它的 podCount 在后端就是 Go 零值占位，
 * 不是测量结果（本集群的快照看不到别的集群）。按 0 去画会得到一个
 * 最小的圆点，读者会把"看不见"读成"这里几乎没有 Pod"。
 */
function nodeRadius(n: SimNode): number {
  if (n.foreign) return 8
  const c = n.podCount
  if (c >= 8) return 17
  if (c >= 4) return 14
  if (c >= 2) return 12
  return 10
}

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
function seedPositions(nodes: SimNode[], W: number) {
  nodes.forEach((n, i) => {
    let h = 0
    for (const ch of n.id) h = (h * 31 + ch.charCodeAt(0)) | 0
    const angle = ((Math.abs(h) % 360) + i * 37) * (Math.PI / 180)
    n.x = W / 2 + Math.cos(angle) * (W / 5)
    n.y = H / 2 + Math.sin(angle) * (H / 4)
  })
}

export default function TopologyGraph({
  topology, onSelectEdge,
}: { topology: Topology; onSelectEdge?: (e: TopologyEdge) => void }) {
  const [, forceRender] = useState(0)
  const boxRef = useRef<HTMLDivElement>(null)
  // 容器实测宽度。首帧为 0，测到之后再跑模拟 —— 用一个猜出来的宽度
  // 先画一遍会让图在加载完成时跳一下。
  const [width, setWidth] = useState(0)

  useEffect(() => {
    const el = boxRef.current
    if (!el) return
    const ro = new ResizeObserver(([entry]) => {
      setWidth(Math.round(entry.contentRect.width))
    })
    ro.observe(el)
    return () => ro.disconnect()
  }, [])
  const nodesRef = useRef<SimNode[]>([])
  const linksRef = useRef<SimLink[]>([])

  const key = useMemo(
    () => topology.nodes.map((n) => n.id).join('|') + '#' + topology.edges.length,
    [topology],
  )

  useEffect(() => {
    if (width === 0) return
    const nodes: SimNode[] = topology.nodes.map((n) => ({ ...n }))
    const byID = new Map(nodes.map((n) => [n.id, n]))
    const edgesWithMissingEndpoint = topology.edges.filter(
      (e) => !byID.has(e.source) || !byID.has(e.target),
    )
    if (edgesWithMissingEndpoint.length > 0) {
      // 后端现在保证每条边引用的端点都有对应节点（哪怕是外部占位节点）。
      // 这里还是漏了，说明后端退化了 —— 静默丢弃只会让图看起来更干净，
      // 掩盖了这个信号，所以必须喊出来。
      console.warn(
        '[TopologyGraph] dropping edges with missing endpoint node:',
        edgesWithMissingEndpoint.map((e) => `${e.source} → ${e.target}`),
      )
    }
    const links: SimLink[] = topology.edges
      .filter((e) => byID.has(e.source) && byID.has(e.target))
      .map((e) => ({ source: byID.get(e.source)!, target: byID.get(e.target)!, edge: e }))

    seedPositions(nodes, width)

    const { distance, charge } = layoutScale(width, nodes.length)
    const sim = forceSimulation(nodes)
      .force('link', forceLink<SimNode, SimLink>(links).id((d) => d.id).distance(distance))
      .force('charge', forceManyBody().strength(charge))
      .force('center', forceCenter(width / 2, H / 2))
      // 各向异性定心：容器是矮而宽的，力导布局默认给出近似圆形的团。
      // 纵向拉得更紧、横向更松，布局的形状才会去贴容器的比例，
      // 而不是在一个宽画布里堆成一个高瘦的团、两侧空着。
      .force('x', forceX(width / 2).strength(0.05))
      .force('y', forceY(H / 2).strength(0.11))
      .stop()

    // 同步跑固定轮数而非动画：结果确定，且没有节点缓缓漂移的过程 ——
    // 动画在这里只会让人以为图还在变。
    //
    // 每次 topology 变化都整体重新播种、重新模拟：对今天"每个集群一份
    // 静态快照"的数据形态是对的。如果将来拓扑变成原地增量更新（而不是
    // 整份替换），这里的"可复现即可建立空间记忆"就不再成立 —— 增量数据
    // 到来时全图会跟着重新洗牌，等于没有增量稳定性，需要另外设计。
    // 迭代轮数偏多：各向异性定心会把节点推开，收敛不足时连线交叉明显更多。
    // 同步跑完再渲染，多花的是一次性的毫秒级计算，换来的是稳定可读的图。
    sim.tick(500)

    // 力导布局的坐标范围不受画布约束，节点会越出边界被容器裁掉 ——
    // 一个被裁掉一半的节点，读者无从知道它是否还有别的连线。
    // 模拟结束后按包围盒整体平移缩放，保证全部节点连同标签落在画布内。
    fitToCanvas(nodes, width)

    nodesRef.current = nodes
    linksRef.current = links
    forceRender((v) => v + 1)
  }, [key, topology, width])

  return (
    <div ref={boxRef} style={{ width: '100%' }}>
      <svg width={width || '100%'} height={H} style={{ display: 'block' }}>
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
            cx={n.x} cy={n.y} r={nodeRadius(n)}
            /*
              无策略的节点用下沉底色填充，而不是换成语义色 ——
              这是资产状态不是判定结论。填充差异配合下方的"无策略"
              文字，两处冗余表达同一件事，避免只靠颜色传递信息。
            */
            fill={n.foreign ? 'var(--bg)'
              : !n.hasPolicy ? 'var(--surface-sunken)' : 'var(--surface)'}
            stroke={n.inMesh ? 'var(--degraded-stroke)' : 'var(--border-strong)'}
            strokeWidth={n.inMesh ? 2 : 1.25}
            strokeDasharray={n.foreign ? '3 3' : undefined}
          />
          <text
            x={n.x} y={(n.y ?? 0) + nodeRadius(n) + 14} textAnchor="middle"
            fontSize={12} fill={n.foreign ? 'var(--text-muted)' : 'var(--text)'}
          >{n.namespace}</text>
          {/*
            "无策略"不用 DENY 的红：语义色是判定结论的专属载体（spec §17.1）。
            这是资产状态 —— 一个没有策略的 namespace 里流量可能全部 ALLOW，
            染成红色会让人读成"这里被阻断了"。
          */}
          {!n.hasPolicy && !n.foreign && (
            <text x={n.x} y={(n.y ?? 0) + nodeRadius(n) + 28} textAnchor="middle" fontSize={10}
              fill="var(--text-secondary)" fontWeight={600}>
              无策略
            </text>
          )}
          <title>
            {/*
              foreign 节点的 podCount / hasPolicy 在后端就是 Go 零值占位，
              不是测量结果 —— 这个集群的快照根本看不到另一个集群的 Pod
              和策略。把零值当结论显示（"0 个 Pod"「没有任何 NetworkPolicy」）
              是这块 UI 能说出的最吓人的两句话，用在看不见的地方比什么
              都不说更危险，所以这里不显示计数，只说明看不见。
            */}
            {n.foreign
              ? `${n.id}\n其他集群的命名空间。本集群的快照看不到它的 Pod 与策略，`
                + `这里不显示计数 —— 缺失不等于零。\n本集群的 NetworkPolicy 管不到它。`
              : `${n.id}\n${n.podCount} 个 Pod`
                + (n.inMesh ? '\n在 service mesh 中：L4 身份被 sidecar 遮蔽' : '')
                + (n.hasPolicy ? '' : '\n没有任何 NetworkPolicy')
                + (n.unmanagedPodCount > 0 ? `\n${n.unmanagedPodCount} 个 hostNetwork Pod 不受管控` : '')}
          </title>
        </g>
      ))}
      </svg>
    </div>
  )
}
