import type { ButtonHTMLAttributes, CSSProperties, ReactNode } from 'react'
import { Dropdown } from './radix'

/*
 * 通用组件层。
 *
 * 存在的理由：页面此前各自手写表头样式、卡片边框、区块标题，三种表格长得
 * 不一样，读者会以为它们在讲不同性质的事。规整的组件系统本身就是专业感的
 * 来源之一（spec §17.1 手法三）。
 *
 * **样式走 Tailwind，色板与尺度仍然只来自 tokens.css。** theme.css 里每一个
 * 值都是 var(--…)，一个字面量都没有 —— 语义色是硬约束（ALLOW / DENY /
 * UNKNOWN 各自唯一，全站不得挪作他用），在这里重新定义一份色阶等于给同一
 * 个概念开第二个定义，而两份会漂：漂的症状是某个页面的 ALLOW 绿与别处不是
 * 同一个绿，于是颜色不再读得出判定。
 *
 * 这里刻意不做主题化与变体爆炸 —— 组件越少、越固定，界面越像一个长期使用
 * 的系统，而不是一次演示。
 */

/** 页面标题与一句说明。说明解释这一屏回答什么问题，不是装饰。 */
export function PageHeader({ title, description }: { title: string; description?: ReactNode }) {
  return (
    <header className="mb-5 m-0 text-2xl tracking-[-0.02em] text-ink font-title mt-2 mb-0 max-w-[640px] text-sm leading-relaxed text-ink-muted">
      <h1
      >
        {title}
      </h1>
      {description && (
        <p>
          {description}
        </p>
      )}
    </header>
  )
}

/** 卡片：靠边框分层，不靠投影。投影一重就显得"演示用"。 */
export function Card({ children, style, className = '' }: {
  children: ReactNode
  style?: CSSProperties
  className?: string
}) {
  return (
    <div
      className={`rounded-card border border-line bg-surface shadow-card ${className}`}
      style={style}
    >
      {children}
    </div>
  )
}

/**
 * 区块：标题 + 说明 + 卡片化的内容。
 *
 * meta 放在标题右侧，用于条数、时间范围这类"这块内容的边界"信息 ——
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
    <section className="mb-6">
      {/* 标题下一条细线：此前标题与正文只差 2px 字号，"区块"读不出是区块。
          分隔线比放大字号便宜 —— 把标题放大到扎眼会让这一屏显得在推销结论，
          而这个平台的结论靠依据可查建立信任，不靠视觉音量。 */}
      <div className="mb-3 border-b border-line pb-2 flex flex-wrap items-baseline justify-between gap-2 m-0 text-lg tracking-[-0.01em] text-ink font-section text-xs tabular-nums text-ink-muted mt-2 mb-0 max-w-[720px] text-sm leading-relaxed">
        <div>
          <h2
          >
            {title}
          </h2>
          {meta && <span>{meta}</span>}
        </div>
        {description && (
          <p>
            {description}
          </p>
        )}
      </div>
      {children}
    </section>
  )
}

/** 表格外壳。表格是本产品的主要信息载体，样式集中在这里，不由各页面各写一份。 */
export function TableCard({ children }: { children: ReactNode }) {
  return (
    <Card className="overflow-hidden">
      <table className="dt">{children}</table>
    </Card>
  )
}

/** 会滚动的表格外壳，表头由 StickyHead 钉住。 */
export function ScrollTableCard({ children, maxHeight = 420 }: {
  children: ReactNode
  maxHeight?: number
}) {
  return (
    <Card className="overflow-hidden">
      <div className="overflow-auto" style={{ maxHeight }}>
        <table className="dt">{children}</table>
      </div>
    </Card>
  )
}

/** 钉住的表头。长表格滚下去之后，列的含义不能跟着消失。 */
export function StickyHead({ children }: { children: ReactNode }) {
  return <thead className="sticky top-0 z-10 bg-sunken">{children}</thead>
}

/** 标签。strong 用于需要被一眼认出的那一个，其余保持克制。 */
export function Chip({ children, strong = false }: { children: ReactNode; strong?: boolean }) {
  return (
    <span
      className={[
        'inline-block rounded-chip px-2 py-[2px] text-xs whitespace-nowrap',
        strong
          ? 'border border-line-strong bg-sunken font-medium text-ink'
          : 'border border-line text-ink-muted',
      ].join(' ')}
    >
      {children}
    </span>
  )
}

/**
 * 空态。**message 说"是什么空了"，detail 说"为什么"** ——
 * 一个只说"暂无数据"的空态，与一次查询失败长得一模一样。
 */
export function EmptyState({ message, detail }: { message: string; detail: ReactNode }) {
  return (
    <Card className="p-4">
      <p className="m-0 text-sm text-ink-2">{message}</p>
      <p className="mt-2 mb-0 text-xs text-ink-muted">{detail}</p>
    </Card>
  )
}

/**
 * 指标块。tone 只接受三个语义值，且**只用于判定语义** ——
 * 拿 unknown 的琥珀色表示"跨 namespace"之类，用户就再也无法从颜色读出结论。
 */
export function StatTile({
  label, value, note, tone, size,
}: {
  label: string
  value: string
  note?: ReactNode
  tone?: 'unknown' | 'degraded' | 'deny'
  size?: 'lg'
}) {
  // **语义用左侧描边表达，数字本身保持满对比度。**
  // 把数字染成语义色会让"这个数不太可信"读成"这个数不重要"，而 DEGRADED
  // 的结论仍然成立，只是不可信 —— 它必须与正常结论同等显著（tokens.css）。
  const accent =
    tone === 'unknown' ? 'var(--verdict-unknown)'
      : tone === 'degraded' ? 'var(--degraded-stroke)'
        : tone === 'deny' ? 'var(--verdict-deny)'
          : undefined
  return (
    <Card style={{
      padding: 'var(--space-3)',
      borderLeft: accent ? `3px solid ${accent}` : undefined,
    }}>
      <div className="text-xs text-ink-muted">{label}</div>
      <div
        className={`mt-1 font-semibold tabular-nums ${size === 'lg' ? 'text-2xl' : 'text-xl'}`}
        style={{ color: accent ?? 'var(--text)' }}
      >
        {value}
      </div>
      {note && <div className="mt-1 text-xs text-ink-muted">{note}</div>}
    </Card>
  )
}

/** 工具条：筛选与操作入口横排。 */
export function Toolbar({ children }: { children: ReactNode }) {
  return <div className="mb-3 flex flex-wrap items-center gap-3">{children}</div>
}

/** 带标签的表单项。 */
export function Field({ label, children }: { label: string; children: ReactNode }) {
  return (
    <label className="inline-flex items-center gap-2 text-sm text-ink-muted">
      {label}
      {children}
    </label>
  )
}

/**
 * 下拉框。
 *
 * 转发到 Radix 的无头实现（components/radix 的 Dropdown）：原生 `<select>`
 * 在 macOS 与 Windows 上外观差异很大，与卡片、表格的质感对不上，界面会显得
 * 是几段拼起来的。
 *
 * 保留这个名字与签名，是为了调用方不必跟着改 —— 换实现不该变成一次跨十个
 * 页面的改动。
 */
export function Select({ value, onChange, options, ariaLabel, style }: {
  value: string
  onChange: (v: string) => void
  options: [string, string][]
  ariaLabel?: string
  style?: CSSProperties
}) {
  return (
    <span style={style} className="inline-block">
      <Dropdown
        value={value} onChange={onChange} options={options}
        ariaLabel={ariaLabel} className="w-full"
      />
    </span>
  )
}

/**
 * 提示条。
 *
 * **不分级别**（没有 info / warn / error 三色）：这一屏上唯一该用颜色说话的
 * 是判定语义色。给提示条也上色会让画面出现第二套颜色语言，而读者分不清
 * 哪一套在讲判定。要强调的内容靠文案自己说。
 */
export function Notice({ children }: { children: ReactNode }) {
  return (
    <div className="mb-3 rounded-card border border-line bg-sunken px-3 py-2 text-sm text-ink-2">
      {children}
    </div>
  )
}

/**
 * 按钮。
 *
 * **一个定义，不是四个。** 在这之前 ClustersPage / GitReposPage /
 * AccountsPage / SettingsPage 各自抄了一份同样的 buttonStyle 常量 —— 四份
 * 会各自漂，而漂的症状是同一个动作在不同页面上长得不一样，读者会以为它们
 * 的分量不同（同 ui.tsx 抬头那条：三种表格会让人以为在讲三件事）。
 *
 * 只有两个变体：主操作与次操作。**没有 danger 变体** —— 这一屏上唯一该用
 * 颜色说话的是判定语义色，给按钮也上红会让画面出现第二套颜色语言，而读者
 * 分不清哪一套在讲判定。危险动作靠文案与二次确认表达，不靠颜色。
 */
export function Button({
  children, variant = 'primary', className = '', ...rest
}: {
  children: ReactNode
  variant?: 'primary' | 'secondary'
} & ButtonHTMLAttributes<HTMLButtonElement>) {
  const look = variant === 'primary'
    ? 'border-transparent bg-accent text-[var(--text-on-dark)]'
    : 'border-line-strong bg-surface text-ink'
  return (
    <button
      className={[
        'inline-flex items-center gap-2 rounded-chip border px-3 py-[6px]',
        'text-sm font-medium disabled:cursor-not-allowed disabled:opacity-50',
        'focus-visible:outline-2 focus-visible:outline-accent',
        look, className,
      ].join(' ')}
      {...rest}
    >
      {children}
    </button>
  )
}

/**
 * 加载骨架。
 *
 * 换掉一行「加载中…」不是为了好看：一行文字与一个失败的空屏在视觉上没有
 * 区别，而这个平台上"这一屏是空的"与"这一屏还没答上来"是两件必须分得开的
 * 事（同 EmptyState 的纪律）。骨架给出的是**版面的形状**，因此它一眼就能
 * 与"查过了，确实没有"区分开。
 *
 * 不做闪光动画：动得越花，越像在掩饰慢。这里只用一个低幅度的呼吸。
 */
export function Skeleton({ rows = 3 }: { rows?: number }) {
  return (
    <Card className="p-3">
      <div className="flex flex-col gap-2" aria-busy="true" aria-live="polite">
        <span className="sr-only">加载中</span>
        {Array.from({ length: rows }, (_, i) => (
          <div
            key={i}
            className="h-3 animate-pulse rounded-chip bg-sunken"
            // 参差的宽度让它读起来像一段内容，而不是一个进度条 ——
            // 进度条会让人以为有确定的完成度，而这里没有。
            style={{ width: `${[92, 74, 83, 66, 88][i % 5]}%` }}
          />
        ))}
      </div>
    </Card>
  )
}

/**
 * 失败提示。
 *
 * 抽成组件而不是每处各写一遍：此前六处各自拼 `background: var(--verdict-deny-bg)`
 * 加内边距，而失败提示是**唯一允许借用判定色的非判定场景** —— 借得越随意，
 * 这条例外越站不住。收在一处，例外就只有一个位置，改主意时也只改一处。
 *
 * `role="alert"` 不是可选的：一次表单提交失败若只是视觉上出现，读屏器用户
 * 会以为什么都没发生，然后再点一次。
 */
export function ErrorNotice({ children }: { children: ReactNode }) {
  return (
    <p
      role="alert"
      className="mt-3 mb-0 rounded-card px-3 py-2 text-sm"
      style={{ background: 'var(--verdict-deny-bg)', color: 'var(--verdict-deny)' }}
    >
      {children}
    </p>
  )
}
