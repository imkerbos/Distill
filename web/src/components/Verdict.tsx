import type { CSSProperties } from 'react'
import type { Confidence, Verdict } from '../api/types'
// 类型来自 pages/verifyView：那里是两层校验结论的共用词汇表，只有类型与
// 纯函数，没有组件。徽章反过来放在这里，是因为「用什么颜色画一个结论」
// 这件事必须全平台一处——仓库页与集群页各画一套的第一个后果，就是其中
// 一页把「未校验」画成了灰字，而它恰恰是那一格里最需要被看见的事实。
import type { VerifyOutcomeView, VerifyStatusView, VerifyTone } from '../pages/verifyView'

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

/**
 * 校验结论的样式：三种语气三种画法。
 *
 * 借用判定语义色（本该只归 VerdictBadge）是有意为之：这里陈述的正是
 * 「这个仓库/这条路径可不可信」，是判断结论而不是元信息。未校验用描边而非
 * 灰化——灰掉等于把「没查过」弱化成一句次要提示，而它恰恰是最需要被看见
 * 的事实（同 VerdictBadge 处理 DEGRADED 的纪律）。
 */
const VERIFY_TONE_STYLE: Record<VerifyTone, CSSProperties> = {
  ok: {
    color: 'var(--verdict-allow)', background: 'var(--verdict-allow-bg)',
    border: '1px solid var(--verdict-allow)',
  },
  bad: {
    color: 'var(--verdict-deny)', background: 'var(--verdict-deny-bg)',
    border: '1px solid var(--verdict-deny)',
  },
  unverified: {
    color: 'var(--verdict-unknown)', background: 'transparent',
    border: 'var(--degraded-stroke-width) solid var(--verdict-unknown)',
  },
}

/**
 * 一条校验结论的徽章。仓库级与路径级共用，两层的区别在文案里，不在画法里。
 *
 * 只接受已经折算好的 VerifyStatusView，不接受裸枚举：折算（含「界面不认识
 * 的取值收窄成未校验」这条纪律）在纯函数里，那里才测得到。
 */
export function VerifyBadge({ view }: { view: VerifyStatusView }) {
  return (
    <span style={{
      display: 'inline-flex', alignItems: 'center', padding: '2px 8px',
      fontSize: 'var(--text-xs)', fontWeight: 500, borderRadius: 999,
      ...VERIFY_TONE_STYLE[view.tone],
    }}>
      {view.label}
    </span>
  )
}

/**
 * 一次校验请求的回执。
 *
 * 与徽章分开显示，且措辞上分得开：徽章说的是「库里现在记着什么」，这句
 * 说的是「刚才那一下发生了什么」。合成一个会逼出一个选择——要么把一次
 * 没发生的校验渲染成崭新的「未校验」（刷新后自己变回旧结论），要么干脆
 * 什么都不说（操作者以为按钮坏了）。两个都不行，所以分成两处。
 */
export function VerifyOutcomeNote({ outcome }: { outcome: VerifyOutcomeView }) {
  return (
    <p role="status" style={{
      margin: 'var(--space-1) 0 0', fontSize: 'var(--text-xs)',
      color: outcome.happened ? 'var(--text-secondary)' : 'var(--verdict-unknown)',
    }}>
      {outcome.message}
    </p>
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
