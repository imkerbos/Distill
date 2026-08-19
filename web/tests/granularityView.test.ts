import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { join } from 'node:path'

import { granularityView, wideningNote } from '../src/pages/granularityView.ts'
import type { Granularity, Widening } from '../src/api/types.ts'

const src = (...parts: string[]) =>
  readFileSync(join(import.meta.dirname, '..', 'src', ...parts), 'utf8')

test('两个粒度各自说出自己的主体是什么', () => {
  const wl = granularityView('WORKLOAD')
  assert.equal(wl.code, 'WORKLOAD')
  assert.match(wl.subject, /workload/i)

  const ns = granularityView('NAMESPACE')
  assert.equal(ns.code, 'NAMESPACE')
  assert.match(ns.subject, /命名空间|namespace/i)
  assert.notEqual(wl.subject, ns.subject)
})

test('粒度缺席按 WORKLOAD 读 —— 那是更精确的那一侧', () => {
  for (const raw of [undefined, null, '' as unknown as Granularity, 'NAMESPACES' as Granularity]) {
    const got = granularityView(raw as Granularity | null | undefined)
    assert.equal(got.code, 'WORKLOAD',
      `${String(raw)} 被读成了 ${got.code}；未登记取值落到 NAMESPACE 会把一份`
      + '本该只选中一个 workload 的策略显示成选中整个命名空间')
  }
})

test('放宽为 0 时要说出「折叠无损」，不能只是不出话', () => {
  const lossless: Widening[] = [
    { namespace: 'a', workloads: 3, rules: 2, extraGrants: 0 },
    { namespace: 'b', workloads: 1, rules: 2, extraGrants: 0 },
  ]
  const note = wideningNote(lossless)
  assert.notEqual(note, '',
    '一份全 0 的放宽报告被读成"没什么可说的"，而它其实是一句很强的话：'
    + '这次折叠没有多放行任何东西')
  assert.match(note, /无损|没有.*放宽|不变/)
  assert.doesNotMatch(note, /放宽了/)
})

test('有放宽时必须点名是哪几个 namespace 与多出多少', () => {
  const note = wideningNote([
    { namespace: 'quiet', workloads: 2, rules: 3, extraGrants: 0 },
    { namespace: 'loud', workloads: 9, rules: 7, extraGrants: 12 },
  ])
  assert.match(note, /loud/, '放宽的那个 namespace 没有被点名')
  assert.match(note, /12/, '多出来的授权数没有说出来')
  assert.doesNotMatch(note, /quiet/,
    '无损的 namespace 混进了放宽名单，操作者会去看一个不需要看的地方')
})

test('服务端没回答放宽时，不得说成「没有放宽」', () => {
  const note = wideningNote(null)
  assert.notEqual(note, '')
  assert.match(note, /没有(说明|回答)|缺席/)
  // 只禁**肯定式**那个说法（零放宽那一支用的是「这次折叠**无损**」）。
  // 一刀切地禁掉「无损」二字会连"不得把它读作无损"这句提醒一起禁掉 ——
  // 那正是这里该出现的话。
  assert.doesNotMatch(note, /这次折叠\*\*无损\*\*/,
    'null 被读成"折叠无损"，而那是一句服务端从没说过的话 —— 与 dataSourceView '
    + '同一条纪律')
})

test('页面不自己拼粒度文案，也不把粒度当布尔', () => {
  const page = src('pages', 'PolicyPage.tsx')
  assert.doesNotMatch(page, /granularity\s*===\s*'namespace'/,
    '页面在拿小写字面量比对枚举 —— 后端回显的是大写，这条比较永远不成立')
  assert.match(page, /granularityView|wideningNote/,
    '页面没有走判定模块，粒度文案散在 tsx 里会各自漂')
})

/* ---------------------------------------------------------------------- */
/* 组件层：Radix + Tailwind 之后要守住的几条                                */
/* ---------------------------------------------------------------------- */

test('粒度切换用的是一组互斥选项，不是两个按钮', () => {
  const page = src('pages', 'PolicyPage.tsx')
  assert.match(page, /<Segmented/,
    '粒度切换没走 Segmented —— 一对 aria-pressed 的按钮在读屏器里读不出'
    + '"这是一组互斥选项"，键盘左右键也走不动')
  assert.doesNotMatch(page, /aria-pressed/,
    '页面里还留着手写的 aria-pressed 切换')
})

test('长解释折起来，但摘要必须自带结论', () => {
  const radix = src('components', 'radix.tsx')
  assert.match(radix, /summary/,
    'Disclosure 没有 summary —— 一个写着"详情"的折叠块等于把内容藏了，'
    + '而藏起来的依据与不存在的依据对读者是同一件事')
  assert.match(radix, /defaultOpen/,
    '折叠块不能只有一种状态：要紧的那几条得能默认展开')
})

test('语义色不进 Tailwind 的通用色阶', () => {
  const theme = src('theme.css')
  // 每一个 --color-* 都必须是 var(--…)，不得出现十六进制字面量 ——
  // 在这里重开一份色板等于给同一个概念开第二个定义，而两份会漂：
  // 漂的症状是某个页面的 ALLOW 绿与别处不是同一个绿。
  const literals = theme.match(/--color-[a-z-]+:\s*#[0-9a-fA-F]/g)
  assert.equal(literals, null,
    `theme.css 里有写死的颜色：${literals?.join(', ')}；色板只能来自 tokens.css`)
})
