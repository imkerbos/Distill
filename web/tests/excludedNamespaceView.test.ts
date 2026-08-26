import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

import {
  EXCLUDED_NS_HELP, EXCLUDED_NS_NONE, excludedNamespaceRows,
} from '../src/pages/excludedNamespaceView.ts'
import type { NamespaceExclusionReason } from '../src/api/types'

const PAGE = readFileSync(new URL('../src/pages/PolicyPage.tsx', import.meta.url), 'utf8')

// 说明必须点出具体后果，而不只是"被排除了"。
//
// 一句笼统的"这些命名空间被排除了"会让人去找哪里配错了 —— 而这是一道
// 有意的保护，不需要任何动作。
test('排除说明点出具体后果', () => {
  const [row] = excludedNamespaceRows([
    { namespace: 'kube-system', reason: 'SYSTEM_NAMESPACE' },
  ])
  assert.equal(row.namespace, 'kube-system')
  assert.match(row.reason, /default-deny/)
  assert.match(row.reason, /失去 DNS/)
  // 必须给出口，否则这道保护读起来像一堵没有门的墙。
  assert.match(row.reason, /显式声明并写下理由/)
})

// 未知分类不静默留空。
test('未认出的原因也给一句话', () => {
  const [row] = excludedNamespaceRows([
    { namespace: 'x', reason: 'FUTURE' as NamespaceExclusionReason },
  ])
  assert.notEqual(row.reason, '')
})

test('没有排除时是空清单', () => {
  assert.deepEqual(excludedNamespaceRows(null), [])
  assert.deepEqual(excludedNamespaceRows([]), [])
})

// 抬头要说清"不生成策略 ≠ 看不见"。
//
// 排除只影响生成，不影响判定与对账。少了这句，操作者会以为这一片是盲区。
test('抬头说清排除不影响判定', () => {
  assert.match(EXCLUDED_NS_HELP, /有意不碰/)
  assert.match(EXCLUDED_NS_HELP, /照常参与判定与对账/)
  assert.notEqual(EXCLUDED_NS_NONE, '')
})

// **区块无条件渲染。**
//
// 一个不显示的区块与"平台碰了所有命名空间"在屏幕上长得一样，
// 而这道保护的价值恰恰在于它可被看见、可被审计。
test('保护区块无条件渲染', () => {
  assert.match(PAGE, /<ExcludedNamespaceSection items=\{pv\.excludedNamespaces \?\? \[\]\} \/>/)
})

// 整片排除要排在逐个 Pod 排除之前。
test('整片排除排在逐个排除之前', () => {
  const ns = PAGE.indexOf('<ExcludedNamespaceSection')
  const wl = PAGE.indexOf('<ExcludedWorkloadSection')
  assert.ok(ns > 0 && wl > 0, '两个区块都要在')
  assert.ok(ns < wl, '先说「这一片不碰」，再说「这一片里哪几个 Pod 有问题」')
})
