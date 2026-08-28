import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

import {
  EXPOSURE_WIDENING_HELP, EXPOSURE_WIDENING_NONE,
  exposureWideningNote, exposureWideningRows,
} from '../src/pages/exposureWideningView.ts'
import type { ExposureWidening } from '../src/api/types'

const PAGE = readFileSync(new URL('../src/pages/PolicyPage.tsx', import.meta.url), 'utf8')

/** UAT 的 devops/zk-0-lb：selector 点名 zookeeper-0，规则覆盖三个 Pod。 */
const zk: ExposureWidening = {
  namespace: 'devops', service: 'zk-0-lb', workload: 'zookeeper',
  selectedPods: 1, workloadPods: 3, extraPods: 2,
}

// 三个数各有各的含义，一行里都要答得出来：点名了几个、覆盖几个、多几个。
test('放宽的那条报出点名数、覆盖数与差额', () => {
  const [row] = exposureWideningRows([zk])
  assert.equal(row.label, 'devops/zk-0-lb')
  assert.equal(row.workload, 'zookeeper')
  assert.equal(row.coverage, '1 / 3')
  assert.equal(row.extraPods, 2)
  assert.match(row.note, /2/)
})

// **extraPods 为 0 也要留在表里**（spec §4）：把无损的与真的放宽了的混在
// 一起，操作者分不出哪几条值得回到 Pod 粒度看；而过滤掉无损的那些，会让
// "算过、恰好无损"与"根本没算"在屏幕上长得一样。
test('无损的那条也留在表里，并说清它无损', () => {
  const lossless: ExposureWidening = {
    namespace: 'shop', service: 'web-lb', workload: 'web',
    selectedPods: 3, workloadPods: 3, extraPods: 0,
  }
  const rows = exposureWideningRows([lossless])
  assert.equal(rows.length, 1, 'extraPods 为 0 的那条被过滤掉了')
  assert.equal(rows[0].coverage, '3 / 3')
  assert.match(rows[0].note, /无损/)
})

// 两种结局给的不是同一句话，否则读的人分不出该不该去改 Service。
test('放宽了与没放宽给出不同的说明', () => {
  const [widened] = exposureWideningRows([zk])
  const [lossless] = exposureWideningRows([{ ...zk, selectedPods: 3, extraPods: 0 }])
  assert.notEqual(widened.note, lossless.note)
})

// 同名 Service 在不同 namespace 里必须分得开。
test('行键含 namespace 与 workload', () => {
  const rows = exposureWideningRows([zk, { ...zk, namespace: 'other' }])
  assert.equal(new Set(rows.map(r => r.key)).size, 2)
})

test('没有暴露规则时是空清单', () => {
  assert.deepEqual(exposureWideningRows(null), [])
  assert.deepEqual(exposureWideningRows([]), [])
  assert.deepEqual(exposureWideningRows(undefined), [])
})

// null 与 [] 是两件事：前者是"服务端没回答"，后者是"算过，一条都没放宽"。
test('null 与空清单给出不同的说明', () => {
  const nullNote = exposureWideningNote(null)
  assert.notEqual(nullNote, '')
  assert.equal(exposureWideningNote([]), '')
  assert.notEqual(nullNote, EXPOSURE_WIDENING_NONE)
  assert.match(nullNote, /没有返回|缺席/)
})

// 抬头要说清这次放宽从哪来，否则会与折叠粒度那一栏（widening）混在一起。
test('抬头说清放宽的来源', () => {
  assert.match(EXPOSURE_WIDENING_HELP, /selector/)
  assert.match(EXPOSURE_WIDENING_HELP, /workload/)
  assert.notEqual(EXPOSURE_WIDENING_NONE, '')
})

// **这一节必须真的挂在页面上。** 五轮修复把这个字段送上了生产路径，它到达
// JSON 之后又在 TypeScript 这堵墙前消失了一轮 —— excess property check 管不到
// 解析出来的服务端响应，类型层面不会有任何信号。
test('放宽区块无条件渲染、且不折叠 null', () => {
  assert.match(PAGE, /<ExposureWideningSection items=\{pv\.exposureWidenings\} \/>/)
  assert.doesNotMatch(
    PAGE,
    /<ExposureWideningSection items=\{pv\.exposureWidenings \?\? \[\]\} \/>/,
    'items 被 `?? []` 收口了：null（服务端没回答）会被悄悄读成"一条都没放宽"',
  )
})
