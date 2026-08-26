import test from 'node:test'
import assert from 'node:assert/strict'

import { ALL_VALUE, fromSelectValue, toSelectValue } from '../src/components/selectValue.ts'

/**
 * 空串必须换个替身送进 Radix。
 *
 * Radix Select 把空串保留给「还没有选过」，于是本产品用空串表示的「全部」
 * 会渲染成一个空框。实测「判定」与「可信度」两个筛选器在默认状态下就是
 * 两个只有一个 ▾ 的框 —— 操作者读到的不是「没有筛选」，而是「这个控件
 * 没加载出来」，进而无法判断眼前这张表是不是被筛过。
 */
test('「全部」不以空串送进下拉框', () => {
  assert.notEqual(toSelectValue(''), '')
  assert.equal(toSelectValue(''), ALL_VALUE)
})

/** 往返必须无损，否则筛选器选完之后回不到原值。 */
test('取值往返无损', () => {
  for (const v of ['', 'ALLOW', 'DENY', 'UNKNOWN', 'DEGRADED', 'kind-local-e2e']) {
    assert.equal(fromSelectValue(toSelectValue(v)), v, `取值 ${JSON.stringify(v)} 往返之后变了`)
  }
})

/** 具体取值原样通过：替身只替空串。 */
test('具体取值不被改写', () => {
  assert.equal(toSelectValue('ALLOW'), 'ALLOW')
  assert.equal(fromSelectValue('ALLOW'), 'ALLOW')
})

/**
 * 替身不能撞上真实取值。
 *
 * 撞上的后果是选「全部」被当成选了某个具体值，而表格会安静地少显示一批行 ——
 * 一个不报错、只是少了几条的筛选器。
 */
test('替身不会与真实枚举值相撞', () => {
  for (const real of ['ALLOW', 'DENY', 'UNKNOWN', 'OK', 'DEGRADED', 'ALL', '全部', 'all']) {
    assert.notEqual(ALL_VALUE, real)
  }
})
