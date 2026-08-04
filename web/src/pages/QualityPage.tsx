import { api } from '../api/client'
import { useResource } from '../api/useResource'

const REASON_LABEL: Record<string, string> = {
  POLICY_MALFORMED: '策略无法解析',
  SNAPSHOT_MISSING: '资产快照缺失',
  IP_AMBIGUOUS: 'IP 复用不可区分',
  CLUSTER_AMBIGUOUS: '跨集群网段重叠',
  IDENTITY_LOST_MESH: 'sidecar 遮蔽身份',
  CCNP_PRESENT: '存在 Cilium 策略',
  NAT_TRANSLATED: '地址被转换',
  EXTERNAL_NO_IDENTITY: '公网流量无主体',
  NAMED_PORT_UNRESOLVED: '命名端口无法解析',
  LOG_SAMPLED_OUT: '日志采样丢失',
  UNSPECIFIED: '未记录原因',
}

const pct = (v: number) => `${(v * 100).toFixed(1)}%`

export default function QualityPage({ cluster }: { cluster: string }) {
  const { data: q, error, loading } = useResource(cluster, () => api.quality(cluster))

  if (error) return <p style={{ color: 'var(--verdict-deny)' }}>{error}</p>
  if (loading || !q) return <p style={{ color: 'var(--text-muted)' }}>加载中…</p>

  return (
    <div>
      <h2 style={{ marginTop: 0 }}>数据质量</h2>

      {/*
        覆盖率与"不知道"必须同屏。单独展示一个好看的覆盖率数字，
        会让人以为剩下的部分都是安全的，而实际上其中相当一部分
        是平台根本没能判定的流量。
      */}
      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(180px, 1fr))', gap: 'var(--space-3)' }}>
        <Stat label="策略覆盖率" value={pct(q.policyCoverage)}
          note={`${q.nakedPodCount} 个 Pod 无任何策略`} />
        <Stat label="可信判定占比" value={pct(q.trustedRate)} />
        <Stat label="无法判定" value={`${q.unknownCount} 条`} tone="unknown"
          note={pct(q.unknownRate)} />
        <Stat label="可信度降级" value={pct(q.degradedRate)} tone="degraded" />
        <Stat label="跨集群敞口" value={`${q.crossClusterCount} 条`}
          note="当前版本不做管控" />
        <Stat label="不受管控 Pod" value={`${q.unmanagedPodCount} 个`}
          note="hostNetwork，已排除出覆盖率" />
      </div>

      <section style={{ marginTop: 'var(--space-5)' }}>
        <h3 style={{ fontSize: 14, marginBottom: 'var(--space-2)' }}>无法判定的构成</h3>
        <p style={{ fontSize: 13, color: 'var(--text-muted)', marginTop: 0 }}>
          只报一个比例无法告诉你该去修哪个子系统。下面是这 {q.unknownCount} 条的具体成因。
        </p>

        {Object.keys(q.unknownComposition).length === 0 ? (
          <p style={{ fontSize: 13 }}>本集群没有无法判定的流量。</p>
        ) : (
          <table style={{ borderCollapse: 'collapse', fontSize: 13, minWidth: 420 }}>
            <tbody>
              {Object.entries(q.unknownComposition)
                .sort((a, b) => b[1] - a[1])
                .map(([reason, count]) => (
                  <tr key={reason} style={{ borderTop: '1px solid var(--border)' }}>
                    <td style={{ padding: '8px', width: 220 }}>{REASON_LABEL[reason] ?? reason}</td>
                    <td style={{ padding: '8px', fontFamily: 'var(--mono)', fontSize: 11, color: 'var(--text-muted)' }}>{reason}</td>
                    <td style={{ padding: '8px', textAlign: 'right', fontWeight: 500 }}>{count} 条</td>
                  </tr>
                ))}
            </tbody>
          </table>
        )}
      </section>

      <p style={{ marginTop: 'var(--space-5)', fontSize: 12, color: 'var(--text-muted)' }}>
        本集群共 {q.totalFlows} 条流量参与判定。跨集群流量在其涉及的两个集群中都会计入，
        因此各集群的总数之和会大于全局流量数。
      </p>
    </div>
  )
}

function Stat({ label, value, note, tone }: {
  label: string; value: string; note?: string; tone?: 'unknown' | 'degraded'
}) {
  const color =
    tone === 'unknown' ? 'var(--verdict-unknown)'
    : tone === 'degraded' ? 'var(--degraded-stroke)'
    : 'var(--text)'
  return (
    <div style={{
      padding: 'var(--space-3)', background: 'var(--surface)',
      border: '1px solid var(--border)', borderRadius: 'var(--radius)',
      borderLeft: tone ? `3px solid ${color}` : '1px solid var(--border)',
    }}>
      <div style={{ fontSize: 12, color: 'var(--text-muted)' }}>{label}</div>
      <div style={{ fontSize: 22, fontWeight: 600, color, lineHeight: 1.3 }}>{value}</div>
      {note && <div style={{ fontSize: 12, color: 'var(--text-muted)' }}>{note}</div>}
    </div>
  )
}
