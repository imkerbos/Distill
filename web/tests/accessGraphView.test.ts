import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { join } from 'node:path'

import { accessEdges, edgeStatus, tighteningNote } from '../src/pages/accessGraphView.ts'
import type { CandidatePolicy } from '../src/api/types.ts'

function policy(namespace: string, rules: Array<{
  direction: 'INGRESS' | 'EGRESS'; peers: string[]
}>): CandidatePolicy {
  return {
    cluster: 'c', namespace, granularity: 'NAMESPACE', workload: '',
    rules: rules.map((r, i) => ({
      fingerprint: `fp-${namespace}-${i}`, enabled: true,
      origin: 'BASELINE', direction: r.direction, peers: r.peers,
      ports: [], flowCount: 0,
    })) as CandidatePolicy['rules'],
  }
}

// 出向规则：主体是源，对端是目的。
test('出向规则读作「主体 → 对端」', () => {
  const edges = accessEdges([policy('app', [{ direction: 'EGRESS', peers: ['kube-system/kube-dns'] }])])
  assert.equal(edges.length, 1)
  assert.equal(edges[0].source, 'app')
  assert.equal(edges[0].target, 'kube-system/kube-dns')
})

// 入向规则方向相反 —— 把两者读成同一个方向，图上每条边都会指错。
test('入向规则读作「对端 → 主体」', () => {
  const edges = accessEdges([policy('payment', [{ direction: 'INGRESS', peers: ['monitoring/prometheus'] }])])
  assert.equal(edges.length, 1)
  assert.equal(edges[0].source, 'monitoring/prometheus')
  assert.equal(edges[0].target, 'payment')
})

test('同一对端出现多次只画一条边，并记下它由几条规则支撑', () => {
  const edges = accessEdges([
    policy('a', [{ direction: 'EGRESS', peers: ['kube-system'] }]),
    policy('b', [{ direction: 'EGRESS', peers: ['kube-system'] }]),
    policy('a', [{ direction: 'EGRESS', peers: ['kube-system'] }]),
  ])
  const ab = edges.filter((e) => e.source === 'a')
  assert.equal(ab.length, 1, '同一对 (源,目的) 画了多条边')
  assert.equal(ab[0].ruleCount, 2)
})

// 网段对端不是集群里的节点 —— 画成一样的形状，读者会去找一个叫
// 10.170.48.2/32 的 namespace。
test('网段对端要能与 namespace 对端区分开', () => {
  const edges = accessEdges([policy('app', [
    { direction: 'EGRESS', peers: ['10.170.48.2/32', 'kube-system/kube-dns'] },
  ])])
  const cidr = edges.find((e) => e.target === '10.170.48.2/32')
  const ns = edges.find((e) => e.target === 'kube-system/kube-dns')
  assert.equal(cidr?.targetIsCIDR, true, '网段对端没有被标出来')
  assert.equal(ns?.targetIsCIDR, false, 'namespace 对端被当成了网段')
})

/* ---------------------------------------------------------------------- */
/* 一条边落在哪几层，就是"该做什么"                                          */
/* ---------------------------------------------------------------------- */

test('推荐有、现存无 → 这是要补的那条策略', () => {
  const s = edgeStatus({ proposed: true, declared: false, observed: false }, true)
  assert.equal(s.code, 'MISSING_POLICY')
  assert.match(s.action, /补|加/)
})

test('推荐有、现存有 → 已覆盖，不必做什么', () => {
  assert.equal(edgeStatus({ proposed: true, declared: true, observed: false }, true).code, 'COVERED')
})

// 现存放行、观测到有流量、而我们没推荐 —— 下发那份推荐会打断它。
test('现存有、观测有、推荐无 → 学漏了，下发会打断', () => {
  const s = edgeStatus({ proposed: false, declared: true, observed: true }, true)
  assert.equal(s.code, 'WOULD_BREAK')
  assert.match(s.action, /打断|切断/)
})

/* ---------------------------------------------------------------------- */
/* 这一轮唯一危险的地方                                                     */
/* ---------------------------------------------------------------------- */

// **零观测时不得说"可收紧"。**
//
// 没有观测的集群上，「没人走这条」与「我们没看过」长得一模一样，而把后者
// 当成前者会推荐一次收紧，切断真实流量。
test('没有流量观测时，绝不给出可收紧的判断', () => {
  const s = edgeStatus({ proposed: false, declared: true, observed: false }, false)
  assert.notEqual(s.code, 'TIGHTENABLE',
    '零观测下把「允许但没人走」当成了结论，而那与「我们没看过」无法区分')
  assert.equal(s.code, 'UNOBSERVED')
  // 只禁**建议收紧**那个说法。一刀切地禁掉「收紧」二字会连"不要据此收紧"
  // 这句提醒一起禁掉 —— 而那正是这里该出现的话。
  assert.doesNotMatch(s.action, /可以(考虑)?收紧/,
    '零观测下给出了收紧建议')
  assert.match(s.action, /不要据此收紧/, '没有提醒读者不要收紧')
})

// 对照组：有观测时这个判断才成立 —— 少了它，一个"永远不说可收紧"的实现
// 也能通过上一条，而那等于把这个功能关掉。
test('有流量观测时，允许但没人走才读作可收紧', () => {
  const s = edgeStatus({ proposed: false, declared: true, observed: false }, true)
  assert.equal(s.code, 'TIGHTENABLE')
  assert.match(s.action, /收紧/)
})

test('整屏的限定语在零观测时必须出现', () => {
  const note = tighteningNote(false)
  assert.notEqual(note, '')
  assert.match(note, /没有.*观测|还没看/)
  const quiet = tighteningNote(true)
  assert.equal(quiet, '', '有观测时不该再出现这条限定语')
})

/* ---------------------------------------------------------------------- */
/* 页面接线                                                                 */
/* ---------------------------------------------------------------------- */

test('候选策略页真的把访问关系画出来了', () => {
  const page = readFileSync(
    join(import.meta.dirname, '..', 'src', 'pages', 'PolicyPage.tsx'), 'utf8')
  assert.match(page, /accessEdges\(/,
    '页面没有聚合访问关系 —— 那 728 条规则本身就是一张「谁访问谁」，'
    + '而逐条读表格读不出这件事')
  assert.match(page, /tighteningNote\(/,
    '零观测时的限定语没有接上；缺了它，读者会把「缺一条策略」读成完整结论')
})
