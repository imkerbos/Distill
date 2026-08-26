import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

import {
  IMPORTED_BASIS, isImported, UNATTACHED_HELP, UNATTACHED_NONE, unattachedRows,
} from '../src/pages/importedView.ts'
import type { UnattachedReason } from '../src/api/types'

const PAGE = readFileSync(new URL('../src/pages/PolicyPage.tsx', import.meta.url), 'utf8')

// 导入来源必须与学习来源分得开。
//
// 导入规则的 flowCount 恒为 0，而那不是"没有流量"——把它落回 LEARNED 那一支，
// 一条人工补上的月结批处理规则会显示成"没人用、可以收紧"。
test('导入来源可区分', () => {
  assert.equal(isImported({ origin: 'IMPORTED' }), true)
  assert.equal(isImported({ origin: 'LEARNED' }), false)
  assert.equal(isImported({ origin: 'BASELINE' }), false)
})

// 依据栏必须解释那个 0 是怎么回事。
test('依据说明解释流量条数为零', () => {
  assert.match(IMPORTED_BASIS, /流量条数恒为 0/)
  assert.match(IMPORTED_BASIS, /不在观测窗口里/)
})

// 三种挂不上的原因各有各的处置，不能给同一句话。
test('每一类挂不上的原因都给出处置', () => {
  const reasons: UnattachedReason[] = ['NO_WORKLOAD_LABEL', 'NO_RULES', 'NO_SUCH_WORKLOAD']
  const texts = reasons.map(reason => {
    const [row] = unattachedRows([{
      importId: reason, namespace: 'payment', name: 'x', reason,
    }])
    return row.reason
  })
  assert.equal(new Set(texts).size, 3, '三类原因给了重复的说明，读的人不知道该做什么')
  for (const t of texts) {
    assert.ok(t.length > 20, `说明太短，无法据此行动：${t}`)
  }
  // 「这个 workload 还没起来」不是错误，不该让人去改 YAML。
  assert.match(texts[2], /会自动进候选集/)
})

// 主体写法带 namespace，否则同名策略分不清。
test('挂不上的导入带命名空间', () => {
  const [row] = unattachedRows([{
    importId: 'i', namespace: 'payment', name: 'monthly', reason: 'NO_RULES',
  }])
  assert.equal(row.label, 'payment/monthly')
})

// 未知分类不静默留空：空白读起来像"这一栏坏了"。
test('未认出的原因也给一句话', () => {
  const [row] = unattachedRows([{
    importId: 'i', namespace: 'n', name: 'x', reason: 'FUTURE' as UnattachedReason,
  }])
  assert.notEqual(row.reason, '')
})

test('没有挂不上的导入时是空清单', () => {
  assert.deepEqual(unattachedRows(null), [])
  assert.deepEqual(unattachedRows([]), [])
})

// 抬头要说清后果：没进候选集就等于没补上。
test('抬头说清没挂上的后果', () => {
  assert.match(UNATTACHED_HELP, /不会被写回/)
  assert.match(UNATTACHED_HELP, /dry-run/)
  assert.notEqual(UNATTACHED_NONE, '')
})

// **零条时区块也要在**：一个不显示的区块与"全都挂上了"在屏幕上长得一样，
// 而操作者没有第二个地方能发现这条补充没生效。
test('挂不上的导入区块无条件渲染', () => {
  assert.match(PAGE, /<UnattachedImportSection items=\{pv\.unattachedImports \?\? \[\]\} \/>/)
  assert.doesNotMatch(
    PAGE,
    /\{rows\.length > 0 && \(\s*<Section\s*\n\s*title="没有挂上的人工导入"/,
    '区块被条件渲染了：零条时它会消失，而那与「全都挂上了」长得一样',
  )
})

// 徽标必须认出第三种来源，不能落回 LEARNED。
test('来源徽标认出人工导入', () => {
  assert.match(PAGE, /case 'IMPORTED':/)
  assert.match(PAGE, /人工导入<\/Chip>/)
})
