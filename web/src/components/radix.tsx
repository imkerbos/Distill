import * as RadixCheckbox from '@radix-ui/react-checkbox'
import * as Collapsible from '@radix-ui/react-collapsible'
import * as RadixDialog from '@radix-ui/react-dialog'
import * as RadixSelect from '@radix-ui/react-select'
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

/**
 * 右侧抽屉。
 *
 * **手写的浮层跑不掉这四样**：焦点陷阱、Escape 关闭、`role="dialog"`、
 * `aria-modal`。写漏的症状不会出现在截图里 —— 键盘用户打开它之后焦点还留在
 * 背景里，读屏器根本不知道弹出了东西。这正是当初选无头组件的理由，而在这
 * 之前这一处一直是自己摆的一个 `position: fixed` 盒子。
 *
 * `title` 不是可选的：Radix 要求每个 Dialog 有可访问名，没有名字的浮层在
 * 读屏器里只会被念成"对话框"。
 */
export function Drawer({ open, onClose, title, children }: {
  open: boolean
  onClose: () => void
  title: string
  children: ReactNode
}) {
  return (
    <RadixDialog.Root open={open} onOpenChange={(o) => { if (!o) onClose() }}>
      <RadixDialog.Portal>
        {/* 遮罩：抽屉直接压在表格上时，被盖住的那几列看起来像"数据缺了"。
            压暗背景让读者知道那是被临时遮挡，不是内容不全。 */}
        <RadixDialog.Overlay className="fixed inset-0 z-10 bg-[rgba(28,28,26,.28)]" />
        <RadixDialog.Content
          className="fixed inset-y-0 right-0 z-20 flex w-[480px] max-w-full flex-col
                     overflow-auto border-l border-line bg-bg shadow-[-4px_0_24px_rgba(28,28,26,.10)]"
        >
          <header className="sticky top-0 flex items-center justify-between border-b border-line
                             bg-surface px-4 py-3">
            <RadixDialog.Title
              className="m-0 text-lg text-ink"
              style={{ fontWeight: 'var(--weight-section)' }}
            >
              {title}
            </RadixDialog.Title>
            <RadixDialog.Close
              className="rounded-chip border border-line bg-surface px-3 py-1 text-sm text-ink-2
                         hover:border-line-strong hover:text-ink"
            >
              关闭
            </RadixDialog.Close>
          </header>
          {children}
        </RadixDialog.Content>
      </RadixDialog.Portal>
    </RadixDialog.Root>
  )
}

/**
 * 下拉框。
 *
 * 原生 `<select>` 在 macOS 与 Windows 上外观差异很大，与卡片、表格的质感
 * 对不上，界面会显得是几段拼起来的。这里换成无头实现 —— 键盘与无障碍行为
 * 由 Radix 保证，外观由我们决定。
 */
export function Dropdown({ value, onChange, options, ariaLabel, className = '' }: {
  value: string
  onChange: (v: string) => void
  options: ReadonlyArray<[string, string]>
  ariaLabel?: string
  className?: string
}) {
  return (
    <RadixSelect.Root value={value} onValueChange={onChange}>
      <RadixSelect.Trigger
        aria-label={ariaLabel}
        className={`ctl inline-flex items-center justify-between gap-2 ${className}`}
      >
        <RadixSelect.Value />
        <RadixSelect.Icon className="text-ink-muted">▾</RadixSelect.Icon>
      </RadixSelect.Trigger>
      <RadixSelect.Portal>
        <RadixSelect.Content
          position="popper" sideOffset={4}
          className="z-30 overflow-hidden rounded-card border border-line-strong bg-surface shadow-card"
        >
          <RadixSelect.Viewport className="p-1">
            {options.map(([v, label]) => (
              <RadixSelect.Item
                key={v} value={v}
                className="cursor-pointer rounded-chip px-2 py-1 text-sm text-ink outline-none
                           data-[highlighted]:bg-sunken data-[state=checked]:font-medium"
              >
                <RadixSelect.ItemText>{label}</RadixSelect.ItemText>
              </RadixSelect.Item>
            ))}
          </RadixSelect.Viewport>
        </RadixSelect.Content>
      </RadixSelect.Portal>
    </RadixSelect.Root>
  )
}

/**
 * 勾选框。
 *
 * 原生 checkbox 同样无法统一外观，而它常常与一段说明并排 —— 两者对不齐时
 * 整段读起来像没排版。这里由 Radix 承担状态与键盘，外观自己给。
 */
export function Checkbox({ checked, onChange, ariaLabel, className = '' }: {
  checked: boolean
  onChange: (v: boolean) => void
  ariaLabel?: string
  className?: string
}) {
  return (
    <RadixCheckbox.Root
      checked={checked}
      onCheckedChange={(v) => onChange(v === true)}
      aria-label={ariaLabel}
      className={`inline-flex size-4 shrink-0 items-center justify-center rounded-[4px]
                  border border-line-strong bg-surface
                  data-[state=checked]:border-brand data-[state=checked]:bg-brand ${className}`}
    >
      <RadixCheckbox.Indicator className="text-[10px] leading-none text-[var(--text-on-dark)]">
        ✓
      </RadixCheckbox.Indicator>
    </RadixCheckbox.Root>
  )
}
