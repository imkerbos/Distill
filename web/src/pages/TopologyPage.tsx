import { api } from '../api/client'
import { useResource } from '../api/useResource'
import TopologyGraph from '../components/TopologyGraph'

export default function TopologyPage({ cluster }: { cluster: string }) {
  const { data: topo, error, loading } = useResource(cluster, () => api.topology(cluster))

  if (error) return <p style={{ color: 'var(--verdict-deny)' }}>{error}</p>
  if (loading || !topo) return <p style={{ color: 'var(--text-muted)' }}>加载中…</p>

  return (
    <div>
      <h2 style={{ marginTop: 0 }}>网络拓扑</h2>

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

function Swatch({ color, width = 2 }: { color: string; width?: number }) {
  return <span style={{
    display: 'inline-block', width: 18, height: width,
    background: color, verticalAlign: 'middle', marginRight: 4,
  }} />
}
