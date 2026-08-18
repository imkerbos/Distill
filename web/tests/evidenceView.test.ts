import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { join } from 'node:path'

import { EVIDENCE_LABEL, evidenceNote } from '../src/pages/evidenceView.ts'
import type { EvidenceClass } from '../src/api/types.ts'

const PAGE_SOURCE = readFileSync(
  join(import.meta.dirname, '..', 'src', 'pages', 'PolicyPage.tsx'), 'utf8')

test('每一个证据类别都有中文标签', () => {
  // 裸枚举名（TRUSTED_ALLOW / INCOMPLETE_WINDOW）在屏幕上要读的人自己去猜，
  // 而猜错的方向是把"证据可能不全"读成一句技术噪声。
  const all: EvidenceClass[] = [
    'TRUSTED_ALLOW', 'TRUSTED_DENY', 'INTERNET_EGRESS', 'CROSS_CLUSTER', 'INCOMPLETE_WINDOW',
  ]
  for (const k of all) {
    assert.ok(EVIDENCE_LABEL[k], `${k} 没有标签`)
    assert.notEqual(EVIDENCE_LABEL[k], k, `${k} 的"标签"就是它自己`)
  }
})

test('证据不完整那一类必须说出它意味着什么', () => {
  // 这是唯一一个「规则本身没错、但可能不够」的类别：漏看的连接不会进候选，
  // 覆盖它的规则于是缺席，而缺席的规则会被判「无流量、可收紧」。
  const note = evidenceNote('INCOMPLETE_WINDOW')
  assert.ok(note, 'INCOMPLETE_WINDOW 没有说明')
  assert.match(note, /不完整|没看全|可能不全/)
  assert.match(note, /确认|启用/)
})

test('其余类别不加说明', () => {
  // 只给这一个加说明是刻意的：其余四类在这一轮之前就存在、语义没变，
  // 顺手给它们编一段解释会让"这一句是新的、要读"这件事被稀释掉。
  for (const k of ['TRUSTED_ALLOW', 'TRUSTED_DENY', 'INTERNET_EGRESS', 'CROSS_CLUSTER'] as EvidenceClass[]) {
    assert.equal(evidenceNote(k), '', `${k} 不该有说明`)
  }
})

test('页面用了标签与说明，不是直接渲染枚举名', () => {
  assert.ok(PAGE_SOURCE.includes('EVIDENCE_LABEL'),
    'PolicyPage 仍在直接渲染 rule.evidence：屏幕上是一个裸枚举名')
  assert.ok(PAGE_SOURCE.includes('evidenceNote'),
    'PolicyPage 没有调用 evidenceNote')

  // **调用了不等于渲染出来了。** 这里额外钉住那个条件与那个插值：
  // 把它包进 {false && …} 时上面两条仍然是绿的（实测），而项目没有 DOM
  // 测试设施，源码断言只能抓"从没被调用"与"条件被写死"这两种失效。
  assert.match(PAGE_SOURCE, /\{note !== ''\s*&&/,
    'PolicyPage 的说明不再由 note 决定是否渲染：条件被写死了')
  assert.match(PAGE_SOURCE, /\{note\}/,
    'PolicyPage 算出了 note 却没有把它插进 JSX')
  assert.ok(!/\{false\s*&&/.test(PAGE_SOURCE),
    'PolicyPage 里有被写死为 false 的渲染分支')
})
