import * as Collapsible from '@radix-ui/react-collapsible'
import * as RadixTabs from '@radix-ui/react-tabs'
import * as RadixTooltip from '@radix-ui/react-tooltip'
import type { ReactNode } from 'react'

/*
 * 建在 Radix 上的三个原语。
 *
 * 用无头组件而不是自己写：焦点管理、键盘导航、aria-* 这些自己写会写漏，
 * 而写漏的症状是键盘用户根本用不了这一屏 —— 那种缺陷不会在截图里出现。
 *
 * 只引三个，不引一整套（CLAUDE.md「依赖引入时机」）：需要时再加。
 */

/**
 * 折叠块。**这一屏信息密度的解法。**
 *
 * 这个平台的每条判定旁边都挂着"为什么" —— 那些解释是必须存在的（一个没有
 * 依据的结论在这里比没有结论更糟），但它们不该与结论抢同一份注意力。
 * 结论常驻，依据折起来。
 *
 * **默认折起，但摘要必须自带结论。** 一个写着"详情"的折叠块等于把内容藏了，
 * 而藏起来的依据与不存在的依据对读者是同一件事。
 */
export function Disclosure({ summary, children, defaultOpen = false }: {
  summary: ReactNode
  children: ReactNode
  defaultOpen?: boolean
}) {
  return (
    <Collapsible.Root defaultOpen={defaultOpen} className="rounded-card border border-line bg-sunken">
      <Collapsible.Trigger
        className="group flex w-full items-start gap-2 px-3 py-2 text-left text-sm text-ink-2
                   hover:bg-surface focus-visible:outline-2 focus-visible:outline-accent"
      >
        <span
          aria-hidden
          className="mt-[2px] shrink-0 text-ink-muted transition-transform
                     group-data-[state=open]:rotate-90"
        >
          ▸
        </span>
        <span className="flex-1">{summary}</span>
      </Collapsible.Trigger>
      <Collapsible.Content className="border-t border-line px-3 py-2 text-sm text-ink-2">
        {children}
      </Collapsible.Content>
    </Collapsible.Root>
  )
}

/**
 * 分段切换。
 *
 * 用 Tabs 而不是两个 button：Radix 给的是 roving tabindex 与左右键导航，
 * 而 aria-pressed 的一对按钮在读屏器里读不出"这是一组互斥选项"。
 *
 * **切换是一次重新取数，不是本地过滤** —— 调用方据此决定要不要发请求。
 * 这里只负责说清楚"当前选中的是哪个"。
 */
export function Segmented<T extends string>({ value, onChange, options, ariaLabel }: {
  value: T
  onChange: (v: T) => void
  options: ReadonlyArray<{ value: T; label: ReactNode }>
  ariaLabel: string
}) {
  return (
    <RadixTabs.Root value={value} onValueChange={(v) => onChange(v as T)}>
      <RadixTabs.List
        aria-label={ariaLabel}
        className="inline-flex rounded-chip border border-line-strong bg-sunken p-[2px]"
      >
        {options.map((o) => (
          <RadixTabs.Trigger
            key={o.value}
            value={o.value}
            className="rounded-chip px-3 py-1 text-sm text-ink-muted
                       data-[state=active]:bg-surface data-[state=active]:text-ink
                       data-[state=active]:shadow-card
                       focus-visible:outline-2 focus-visible:outline-accent"
          >
            {o.label}
          </RadixTabs.Trigger>
        ))}
      </RadixTabs.List>
    </RadixTabs.Root>
  )
}

/**
 * 悬浮说明。
 *
 * **只放"锦上添花"的补充**，不放结论所依赖的任何东西：悬浮层在触屏上摸不到、
 * 在打印与截图里不存在，而这个平台的结论会被贴进工单。
 */
export function Hint({ label, children }: { label: ReactNode; children: ReactNode }) {
  return (
    <RadixTooltip.Provider delayDuration={200}>
      <RadixTooltip.Root>
        <RadixTooltip.Trigger asChild>
          <span className="cursor-help underline decoration-dotted underline-offset-2">{label}</span>
        </RadixTooltip.Trigger>
        <RadixTooltip.Portal>
          <RadixTooltip.Content
            sideOffset={6}
            className="max-w-[320px] rounded-chip border border-line-strong bg-surface
                       px-2 py-1 text-xs text-ink-2 shadow-card"
          >
            {children}
            <RadixTooltip.Arrow className="fill-[var(--surface)]" />
          </RadixTooltip.Content>
        </RadixTooltip.Portal>
      </RadixTooltip.Root>
    </RadixTooltip.Provider>
  )
}
