import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

import {
  evidenceKey, evidenceDetail, observedSpan,
  previewEvidenceNote, ruleEvidenceView,
} from '../src/pages/ruleEvidenceView.ts'
import type { RuleEvidence } from '../src/api/types'

const BACKEND_KEY = readFileSync(
  new URL('../../internal/snapshotstore/ruleevidence.go', import.meta.url), 'utf8')

function evidence(opts: Partial<RuleEvidence> = {}): RuleEvidence {
  return {
    firstSeen: '2026-08-01T00:00:00Z',
    lastSeen: '2026-08-08T00:00:00Z',
    windows: 20,
    completeWindows: 20,
    observations: 412,
    ...opts,
  }
}

// 键的形状必须与后端一致，否则每条规则都查不到自己的证据 —— 而查不到的
// 表现是"全部未记录"，看起来像功能没生效，不像一个键拼错了。
test('证据键与后端 EvidenceKey 同形', () => {
  assert.equal(evidenceKey('payment', 'api', 'abc'), 'payment/api/abc')
  assert.match(
    BACKEND_KEY,
    /return namespace \+ "\/" \+ workload \+ "\/" \+ fingerprint/,
    '后端换了分隔符而前端没跟上：每条规则都会查不到证据',
  )
})

// "没在记"与"记了但这条规则没出现过"必须分开。
//
// 前者是集群状态（演示集群、从未采集），后者是这条规则的性质。压成一个
// 空白格子，读的人会把"我们没在记"读成"证据为零"。
test('未记录证据与证据为零是两档', () => {
  assert.equal(ruleEvidenceView(null, 'payment', 'api', 'abc').strength, 'UNTRACKED')
  assert.equal(ruleEvidenceView({}, 'payment', 'api', 'abc').strength, 'NONE')
  assert.notEqual(
    ruleEvidenceView(null, 'payment', 'api', 'abc').label,
    ruleEvidenceView({}, 'payment', 'api', 'abc').label,
  )
  assert.ok(ruleEvidenceView(null, 'payment', 'api', 'abc').note !== '',
    'UNTRACKED 必须带一句说明，空白本身说明不了它的含义')
})

// 主体不同的同一条规则不得互相顶用。
//
// 指纹只覆盖规则内容 ——「egress 到 kube-dns:53」在每个 workload 上都是同一个
// 指纹。查证据时丢掉主体，一个 workload 会拿到另一个 workload 的证据。
test('证据按主体区分，不只按指纹', () => {
  const table = { [evidenceKey('payment', 'api', 'dns')]: evidence() }
  assert.equal(ruleEvidenceView(table, 'payment', 'api', 'dns').strength, 'ESTABLISHED')
  assert.equal(ruleEvidenceView(table, 'payment', 'worker', 'dns').strength, 'NONE')
})

// 分档随窗口数走，且弱证据排在前面。
test('证据分档与排序', () => {
  const at = (windows: number) =>
    ruleEvidenceView(
      { [evidenceKey('n', 'w', 'f')]: evidence({ windows, completeWindows: windows }) },
      'n', 'w', 'f')
  assert.equal(at(1).strength, 'THIN')
  assert.equal(at(3).strength, 'GROWING')
  assert.equal(at(12).strength, 'ESTABLISHED')

  const ranks = [
    ruleEvidenceView(null, 'n', 'w', 'f').rank,
    ruleEvidenceView({}, 'n', 'w', 'f').rank,
    at(1).rank, at(3).rank, at(12).rank,
  ]
  assert.deepEqual(ranks, [...ranks].sort((a, b) => a - b),
    '证据越弱 rank 必须越小：先看到的应当是最不该下发的那几条')
})

// windows 为 0 的行不得显示成一个有效档位。
//
// 这种行不该存在（记一次就是一个窗口），但真出现时把它读成 THIN 会让一条
// 没有任何窗口支撑的规则显得已经被观察过。
test('零窗口的证据按新出现处理', () => {
  const table = { [evidenceKey('n', 'w', 'f')]: evidence({ windows: 0, completeWindows: 0 }) }
  assert.equal(ruleEvidenceView(table, 'n', 'w', 'f').strength, 'NONE')
})

// 三个数并列，不合成一个证据分。
test('明细同时给出窗口数、跨度与累计次数', () => {
  const text = evidenceDetail(evidence({ windows: 7, completeWindows: 3, observations: 412 }))
  assert.match(text, /7 个窗口（其中 3 个完整）/)
  assert.match(text, /跨 7 天/)
  assert.match(text, /共 412 次/)
})

// 时间不合法时宁可不显示，也不显示负数或 NaN：一个显然错误的数字会被当成真的读。
test('时间跨度不合法时留空', () => {
  assert.equal(observedSpan(evidence({ firstSeen: '2026-08-08T00:00:00Z', lastSeen: '2026-08-01T00:00:00Z' })), '')
  assert.equal(observedSpan(evidence({ lastSeen: '不是时间' })), '')
  assert.equal(observedSpan(evidence({ firstSeen: '2026-08-01T00:00:00Z', lastSeen: '2026-08-01T00:30:00Z' })), '30 分钟')
  assert.equal(observedSpan(evidence({ firstSeen: '2026-08-01T00:00:00Z', lastSeen: '2026-08-01T06:00:00Z' })), '6 小时')
})

// "没在记"要在预览一级说一次，不在每条规则上重复四十遍。
test('未记录证据时预览一级给出提示', () => {
  assert.notEqual(previewEvidenceNote({ evidence: null }), '')
  assert.equal(previewEvidenceNote({ evidence: {} }), '')
})

const PAGE_SOURCE = readFileSync(new URL('../src/pages/PolicyPage.tsx', import.meta.url), 'utf8')

// 证据必须与流量条数分列，不能合并成一个数。
//
// 两个数说的是两件事：那一列是这一个窗口里的条数，这一列是我们看了多久。
// 合并之后，"观察了三周一直如此"与"刚才那一小时第一次出现"就再也分不开。
test('候选策略表把观测证据单列一栏', () => {
  assert.match(PAGE_SOURCE, /<th className="num">流量条数<\/th>\s*<th>观测证据<\/th>/)
  assert.match(PAGE_SOURCE, /<RuleEvidenceCell view=\{ev\} \/>/)
})

// evidence 必须原样传下去，不得在中途 ?? {}。
//
// 补一个空对象会把"这个集群没在记"变成"记了但都是零"，而这两句话对读的人
// 意味着完全不同的动作。
test('证据不在传递途中被填成空对象', () => {
  assert.doesNotMatch(PAGE_SOURCE, /evidence \?\? \{\}/)
  assert.match(PAGE_SOURCE, /evidence=\{pv\.evidence\}/)
})

// 证明不了完整的窗口，攒再多也不能升到最高档。
//
// 漏看的连接不会进候选集，覆盖它的规则于是缺席，而缺席的规则会被判
// 「无流量、可收紧」—— 把这种情况标成"证据充分"，正好在最危险的方向上
// 给了操作者信心。
test('完整度不足的证据不升到最高档', () => {
  const t = { [evidenceKey('n', 'w', 'f')]: evidence({ windows: 40, completeWindows: 0 }) }
  assert.equal(ruleEvidenceView(t, 'n', 'w', 'f').strength, 'GROWING')

  const full = { [evidenceKey('n', 'w', 'f')]: evidence({ windows: 40, completeWindows: 40 }) }
  assert.equal(ruleEvidenceView(full, 'n', 'w', 'f').strength, 'ESTABLISHED')
})

// 完整窗口为 0 时也要写出来，不能省。
test('完整窗口数恒显示，包括零', () => {
  assert.match(evidenceDetail(evidence({ windows: 5, completeWindows: 0 })), /其中 0 个完整/)
})
