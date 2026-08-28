import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

import {
  UNATTACHED_BASELINE_HELP, UNATTACHED_BASELINE_NONE,
  unattachedBaselineNote, unattachedBaselineRows,
} from '../src/pages/unattachedBaselineView.ts'
import type { UnattachedBaselineReason } from '../src/api/types'

const PAGE = readFileSync(new URL('../src/pages/PolicyPage.tsx', import.meta.url), 'utf8')

// 两种成因各有各的处置，不能给同一句话。
test('每一类挂不上主体的原因都给出处置', () => {
  const reasons: UnattachedBaselineReason[] = ['NO_SELECTOR', 'NO_SUCH_WORKLOAD']
  const texts = reasons.map(reason => {
    const [row] = unattachedBaselineRows([{
      kind: 'EXPOSED_INGRESS', namespace: 'istio-system', name: 'gw', reason,
    }])
    return row.reason
  })
  assert.equal(new Set(texts).size, 2, '两类原因给了重复的说明，读的人不知道该做什么')
  for (const t of texts) {
    assert.ok(t.length > 20, `说明太短，无法据此行动：${t}`)
  }
})

// 主体写法带 namespace 与类型，否则同名 Service 分不清、也看不出是哪类 Baseline。
test('挂不上的规则带命名空间与类型', () => {
  const [row] = unattachedBaselineRows([{
    kind: 'EXPOSED_INGRESS', namespace: 'istio-system', name: 'gw', reason: 'NO_SELECTOR',
  }])
  assert.equal(row.label, 'istio-system/gw')
  assert.equal(row.kind, 'EXPOSED_INGRESS')
})

// 未知分类不静默留空：空白读起来像"这一栏坏了"。
test('未认出的原因也给一句话', () => {
  const [row] = unattachedBaselineRows([{
    kind: 'EXPOSED_INGRESS', namespace: 'n', name: 'x', reason: 'FUTURE' as UnattachedBaselineReason,
  }])
  assert.notEqual(row.reason, '')
})

test('没有挂不上的规则时是空清单', () => {
  assert.deepEqual(unattachedBaselineRows(null), [])
  assert.deepEqual(unattachedBaselineRows([]), [])
  assert.deepEqual(unattachedBaselineRows(undefined), [])
})

// 抬头要说清后果：没进候选集意味着这个入口现在什么放行都没有。
test('抬头说清没挂上的后果', () => {
  assert.match(UNATTACHED_BASELINE_HELP, /什么放行都没有/)
  assert.match(UNATTACHED_BASELINE_HELP, /dry-run/)
  assert.notEqual(UNATTACHED_BASELINE_NONE, '')
})

// null 与 [] 是两件事：前者是"服务端没回答"，后者是"算过，都挂上了"。
// 折成同一句话，"服务端没回答"就会被读成"都挂上了"——这个字段描述的
// 恰恰是集群里可能正在悄悄断掉的入口，这个方向的误读后果最重。
test('null 与空清单给出不同的说明', () => {
  const nullNote = unattachedBaselineNote(null)
  const emptyNote = unattachedBaselineNote([])
  assert.notEqual(nullNote, '')
  assert.equal(emptyNote, '')
  assert.notEqual(nullNote, emptyNote)
  assert.notEqual(nullNote, UNATTACHED_BASELINE_NONE, 'null 不该被读成"都挂上了"——那是一句服务端从没说过的话')
  assert.match(nullNote, /没有返回|缺席/, 'null 要说清是服务端没回答，不是给了一个空清单')
})

// **零条 / null 时区块也要在**：一个不显示的区块与"全都挂上了"在屏幕上
// 长得一样，而操作者没有第二个地方能发现这个入口没有任何放行。
test('挂不上的 Baseline 区块无条件渲染、且不折叠 null', () => {
  assert.match(PAGE, /<UnattachedBaselineSection items=\{pv\.unattachedBaselines\} \/>/)
  assert.doesNotMatch(
    PAGE,
    /<UnattachedBaselineSection items=\{pv\.unattachedBaselines \?\? \[\]\} \/>/,
    'items 被 `?? []` 收口了：null（服务端没回答）会被悄悄读成"都挂上了"',
  )
  assert.doesNotMatch(
    PAGE,
    /\{rows\.length > 0 && \(\s*<Section\s*\n\s*title="没有挂上的对外暴露"/,
    '区块被条件渲染了：零条时它会消失，而那与"全都挂上了"长得一样',
  )
})
