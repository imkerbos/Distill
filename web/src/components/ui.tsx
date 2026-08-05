import type { CSSProperties, ReactNode } from 'react'

/*
 * 通用组件层。
 *
 * 存在的理由：四个页面此前各自手写表头样式、卡片边框、区块标题，三种表格
 * 长得不一样，读者会以为它们在讲不同性质的事。规整的组件系统本身就是
 * 专业感的来源之一（spec §17.1 手法三）。
 *
 * 这里刻意不做主题化与变体爆炸 —— 组件越少、越固定，界面越像一个
 * 长期使用的系统，而不是一次演示。
 */

/** 页面标题与一句说明。说明解释这一屏回答什么问题，不是装饰。 */
export function PageHeader({ title, description }: { title: string; description?: ReactNode }) {
  return (
    <header style={{ marginBottom: 'var(--space-4)' }}>
      <h1 style={{
        margin: 0, fontSize: 'var(--text-2xl)', fontWeight: 600, letterSpacing: '-0.01em',
      }}>
        {title}
      </h1>
      {description && (
        <p style={{
          margin: 'var(--space-2) 0 0', fontSize: 'var(--text-sm)',
          color: 'var(--text-muted)', maxWidth: '640px',
        }}>
          {description}
        </p>
      )}
    </header>
  )
}

/** 卡片：靠边框分层，不靠投影。投影一重就显得"演示用"。 */
export function Card({ children, style }: { children: ReactNode; style?: CSSProperties }) {
  return (
    <div style={{
      background: 'var(--surface)',
      border: '1px solid var(--border)',
      borderRadius: 'var(--radius)',
      boxShadow: 'var(--shadow-card)',
      ...style,
    }}>
      {children}
    </div>
  )
}

/**
 * 区块：标题 + 说明 + 卡片化的内容。
 *
 * meta 放在标题右侧，用于条数、时间范围这类"这块内容的边界"信息——
 * 它们必须与内容同屏且固定位置，不能散落在正文里被读者漏掉。
 */
export function Section({
  title, description, meta, children,
}: {
  title: string
  description?: ReactNode
  meta?: ReactNode
  children: ReactNode
}) {
  return (
    <section style={{ marginBottom: 'var(--space-5)' }}>
      <div style={{
        display: 'flex', alignItems: 'baseline', justifyContent: 'space-between',
        gap: 'var(--space-3)', flexWrap: 'wrap', marginBottom: 'var(--space-2)',
      }}>
        <h2 style={{ margin: 0, fontSize: 'var(--text-lg)', fontWeight: 600 }}>{title}</h2>
        {meta && (
          <span style={{ fontSize: 'var(--text-sm)', color: 'var(--text-muted)' }}>{meta}</span>
        )}
      </div>
      {description && (
        <p style={{
          margin: '0 0 var(--space-3)', fontSize: 'var(--text-sm)',
          color: 'var(--text-muted)', maxWidth: '720px',
        }}>
          {description}
        </p>
      )}
      {children}
    </section>
  )
}

/** 表格容器：窄屏下横向滚动由容器承担，页面本身不横向滚动。 */
export function TableCard({ children }: { children: ReactNode }) {
  return (
    <Card style={{ overflowX: 'auto' }}>
      <table className="dt">{children}</table>
    </Card>
  )
}

/**
 * 中性标签。
 *
 * 刻意不接受语义色：位置、方向、粒度这类元信息一旦借用 ALLOW/DENY/UNKNOWN
 * 的颜色，读者就会把"跨 namespace"读成"无法判定"。语义色只归 VerdictBadge。
 */
export function Chip({ children, strong = false }: { children: ReactNode; strong?: boolean }) {
  return (
    <span style={{
      display: 'inline-block',
      padding: '2px 8px',
      borderRadius: 'var(--radius-sm)',
      fontSize: 'var(--text-xs)',
      whiteSpace: 'nowrap',
      background: strong ? 'var(--accent)' : 'var(--surface-sunken)',
      color: strong ? 'var(--text-on-dark)' : 'var(--text-secondary)',
      border: strong ? '1px solid var(--accent)' : '1px solid var(--border)',
    }}>
      {children}
    </span>
  )
}

/**
 * 空状态。
 *
 * detail 是必填的设计约定：一句"没有数据"无法区分"查过了、确实没有"
 * 与"根本没查"。这个平台的每块屏都必须能说清自己没告诉你什么。
 */
export function EmptyState({ message, detail }: { message: string; detail: ReactNode }) {
  return (
    <Card style={{ padding: 'var(--space-4)' }}>
      <p style={{ margin: 0, fontSize: 'var(--text-sm)' }}>{message}</p>
      <p style={{
        margin: 'var(--space-2) 0 0', fontSize: 'var(--text-xs)', color: 'var(--text-muted)',
      }}>
        {detail}
      </p>
    </Card>
  )
}

/**
 * 指标卡。
 *
 * tone 只接受语义值：UNKNOWN 与 DEGRADED 必须与正常指标同等显著
 * （spec §17.1），因此用左侧色条而非灰字弱化 —— 弱化它们等同于
 * 隐瞒平台的能力边界。
 */
export function StatTile({
  label, value, note, tone,
}: {
  label: string
  value: string
  note?: ReactNode
  tone?: 'unknown' | 'degraded'
}) {
  const accent =
    tone === 'unknown' ? 'var(--verdict-unknown)'
      : tone === 'degraded' ? 'var(--degraded-stroke)'
        : undefined
  return (
    <Card style={{
      padding: 'var(--space-3)',
      borderLeft: accent ? `3px solid ${accent}` : undefined,
    }}>
      <div style={{ fontSize: 'var(--text-xs)', color: 'var(--text-muted)' }}>{label}</div>
      <div style={{
        fontSize: 'var(--text-xl)', fontWeight: 600, marginTop: 'var(--space-1)',
        color: accent ?? 'var(--text)', fontVariantNumeric: 'tabular-nums',
      }}>
        {value}
      </div>
      {note && (
        <div style={{
          fontSize: 'var(--text-xs)', color: 'var(--text-muted)', marginTop: 'var(--space-1)',
        }}>
          {note}
        </div>
      )}
    </Card>
  )
}

/** 筛选栏：控件横排，标签在控件左侧，保持各页一致。 */
export function Toolbar({ children }: { children: ReactNode }) {
  return (
    <div style={{
      display: 'flex', gap: 'var(--space-3)', alignItems: 'center',
      flexWrap: 'wrap', marginBottom: 'var(--space-3)',
    }}>
      {children}
    </div>
  )
}

export function Field({ label, children }: { label: string; children: ReactNode }) {
  return (
    <label style={{
      display: 'inline-flex', alignItems: 'center', gap: 'var(--space-2)',
      fontSize: 'var(--text-sm)', color: 'var(--text-muted)',
    }}>
      {label}
      {children}
    </label>
  )
}

/**
 * 下拉选择。
 *
 * 统一外观后各页共用一个实现 —— 之前集群选择器、判定筛选、粒度切换
 * 三处各写一套内联样式，尺寸与圆角都不一致，界面看起来是拼出来的。
 */
export function Select({ value, onChange, options, ariaLabel, style }: {
  value: string
  onChange: (v: string) => void
  options: [string, string][]
  ariaLabel?: string
  style?: CSSProperties
}) {
  return (
    <select
      className="ctl"
      aria-label={ariaLabel}
      value={value}
      onChange={(e) => onChange(e.target.value)}
      style={style}
    >
      {options.map(([v, label]) => <option key={v} value={v}>{label}</option>)}
    </select>
  )
}

/** 提示条：用于"有一部分内容没能展示"这类必须被看到的说明。 */
export function Notice({ children }: { children: ReactNode }) {
  return (
    <div style={{
      padding: 'var(--space-3)',
      marginBottom: 'var(--space-4)',
      background: 'var(--verdict-unknown-bg)',
      border: '1px solid var(--verdict-unknown)',
      borderRadius: 'var(--radius)',
      color: 'var(--verdict-unknown)',
      fontSize: 'var(--text-sm)',
    }}>
      {children}
    </div>
  )
}
