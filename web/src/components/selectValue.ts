/**
 * 下拉框「全部」这一项的取值转换。
 *
 * Radix Select 把空串保留给「还没有选过」——一个 value 为空串的选项，它的
 * 触发器上什么都不显示。而本产品的筛选器一律用空串表示「全部」，于是
 * 「判定」与「可信度」两个筛选器在默认状态下渲染成两个空框：操作者看到的
 * 不是「没有筛选」，而是「这个控件没加载出来」，进而无法判断眼前这张表
 * 是不是被筛过。
 *
 * 转换收口在这里，调用方照旧用空串 —— 让每个页面自己记得绕开一个第三方
 * 库的约定，是这类缺陷会重新长出来的方式。
 *
 * 单独一个 .ts 而不是写进 radix.tsx：这样它能被直接测到，不必先有一套
 * DOM 测试设施。
 */

/**
 * ALL_VALUE 是「全部」在 Radix 内部的替身。
 *
 * 取一个不可能与真实枚举值相撞的串：这些下拉框里装的是 ALLOW / DEGRADED /
 * 集群 ID 这类取值，撞上的后果是选「全部」被当成选了某个具体值，而表格
 * 会安静地少显示一批行。
 */
export const ALL_VALUE = '__distill_all__'

/** 把本产品的取值翻成 Radix 能显示的取值。 */
export function toSelectValue(v: string): string {
  return v === '' ? ALL_VALUE : v
}

/** 把 Radix 交回来的取值翻回本产品的取值。 */
export function fromSelectValue(v: string): string {
  return v === ALL_VALUE ? '' : v
}
