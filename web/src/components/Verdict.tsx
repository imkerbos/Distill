import type { Confidence, Verdict } from '../api/types'

const COLORS: Record<Verdict, { fg: string; bg: string }> = {
  ALLOW: { fg: 'var(--verdict-allow)', bg: 'var(--verdict-allow-bg)' },
  DENY: { fg: 'var(--verdict-deny)', bg: 'var(--verdict-deny-bg)' },
  UNKNOWN: { fg: 'var(--verdict-unknown)', bg: 'var(--verdict-unknown-bg)' },
}

const LABELS: Record<Verdict, string> = {
  ALLOW: '放行', DENY: '阻断', UNKNOWN: '无法判定',
}

/**
 * VerdictBadge 是判定结论的唯一展示出口。
 *
 * DEGRADED 用描边而非灰化表达：它的结论仍然成立，只是不可信，
 * 必须与正常结论同等显著。把它调淡等同于隐瞒平台的能力边界。
 */
export function VerdictBadge({ verdict, confidence }: { verdict: Verdict; confidence?: Confidence }) {
  const c = COLORS[verdict]
  const degraded = confidence === 'DEGRADED'
  return (
    <span
      title={degraded ? '判定可信度降级：源身份被 sidecar 遮蔽或集群存在 Cilium 策略' : undefined}
      style={{
        display: 'inline-flex', alignItems: 'center', gap: 4,
        padding: '2px 8px', fontSize: 12, fontWeight: 500,
        color: c.fg, background: c.bg, borderRadius: 999,
        border: degraded
          ? `var(--degraded-stroke-width) solid var(--degraded-stroke)`
          : '1px solid transparent',
      }}
    >
      {LABELS[verdict]}
      {degraded && <span style={{ fontSize: 11, opacity: 0.9 }}>· 降级</span>}
    </span>
  )
}

/** UnmanagedMark 标出"NetworkPolicy 根本管不到"，它与"策略放行"不是一回事。 */
export function UnmanagedMark() {
  return (
    <span
      title="该端点使用宿主网络，NetworkPolicy 对其不生效 —— 这不是被策略放行"
      style={{
        padding: '2px 6px', fontSize: 11, color: 'var(--text-muted)',
        border: '1px dashed var(--border)', borderRadius: 999,
      }}
    >不受管控</span>
  )
}

/** CrossClusterMark 标出跨集群流量。V4 只做可见性、不做 enforce，这是已知敞口。 */
export function CrossClusterMark() {
  return (
    <span
      title="跨集群流量：当前版本只做可见性，不做策略管控，这是显式的已知敞口"
      style={{
        padding: '2px 6px', fontSize: 11, color: 'var(--text-muted)',
        border: '1px solid var(--border)', borderRadius: 999,
      }}
    >跨集群</span>
  )
}
