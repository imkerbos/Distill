import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { join } from 'node:path'

import {
  FLOW_NEVER_INGESTED_CODE, flowIngestView, isNeverIngestedError, missingEvidence,
} from '../src/pages/flowIngestView.ts'
import type { IngestSummary } from '../src/api/types.ts'

function summary(over: Partial<IngestSummary> = {}): IngestSummary {
  return {
    clusterId: 'c', runId: 'r-1', source: 'HUBBLE',
    startedAt: '2026-08-18T00:00:00Z', finishedAt: '2026-08-18T00:01:00Z',
    status: 'OK', errorReason: '',
    window: { from: '2026-08-18T00:00:00Z', to: '2026-08-18T00:01:00Z' },
    covered: { from: '', to: '' },
    connections: 12,
    sampleRate: 0, sampleRateKnown: false,
    dropped: 0, droppedReported: false,
    coveredKnown: false,
    completeness: 'UNKNOWN',
    ...over,
  }
}

// **「从未摄入过」与「摄入过、没有连接」必须是两句不同的话。**
//
// 两者在界面上长得一模一样，而处置完全相反：前者要去部署采集器，
// 后者什么都不用做 —— 那是一句关于集群的话。
test('从未摄入过说的是"从未"，不是"没有流量"', () => {
  const v = flowIngestView(null)
  assert.equal(v.code, 'NEVER')
  assert.match(v.headline, /从未|还没有过/)
  assert.doesNotMatch(v.headline, /^没有流量$/)
  assert.match(v.action, /部署|开启/)
})

test('摄入过但零条连接，是一句关于集群的话', () => {
  const v = flowIngestView(summary({ connections: 0 }))
  assert.equal(v.code, 'EMPTY')
  assert.notEqual(v.headline, flowIngestView(null).headline)
  assert.doesNotMatch(v.action, /部署采集器/,
    '把"看过了、没有连接"说成"去装采集器"，会让人去做一件不需要做的事')
})

test('失败要给出原因与处置', () => {
  const v = flowIngestView(summary({ status: 'FAILED', errorReason: 'UNREACHABLE' }))
  assert.equal(v.code, 'FAILED')
  assert.match(v.headline, /失败/)
  assert.match(v.action, /连不上|网络|地址/)
})

test('成功且有连接时不报警', () => {
  const v = flowIngestView(summary({ connections: 42 }))
  assert.equal(v.code, 'OK')
})

/* ---------------------------------------------------------------------- */
/* 完整度到不了 COMPLETE 时，要说得出缺哪几项证据                            */
/* ---------------------------------------------------------------------- */

test('缺哪几项证据要逐项说出来', () => {
  const miss = missingEvidence(summary())
  assert.ok(miss.length >= 2, `只报了 ${miss.length} 项，而三项证据一项都没有`)
  assert.ok(miss.some((m) => /覆盖/.test(m)), '没说覆盖窗口缺席')
  assert.ok(miss.some((m) => /采样/.test(m)), '没说采样率缺席')
})

test('证据齐了就不再报缺 —— 上一条不是靠"永远报缺"做到的', () => {
  const miss = missingEvidence(summary({
    coveredKnown: true, sampleRateKnown: true, sampleRate: 1,
    droppedReported: true, completeness: 'COMPLETE',
  }))
  assert.deepEqual(miss, [])
})

test('页面真的接上了这一屏', () => {
  const page = readFileSync(
    join(import.meta.dirname, '..', 'src', 'pages', 'CollectionPage.tsx'), 'utf8')
    .replace(/\/\*[\s\S]*?\*\//g, '').replace(/^\s*\/\/.*$/gm, '')
  assert.match(page, /flowIngestView\(/,
    '资产采集页没有接上流量摄入 —— 而"哪里去采集流量"正是界面上答不出的那句话')
})

// **「从未摄入过」在流量页上必须与在采集页上说同一句话。**
//
// 两屏对同一个状态给两套指示，操作者会按先看到的那一屏行动；而这个状态的
// 正确处置只有一个 —— 去部署采集器或开流量日志。
test('从未摄入过的判定按业务码，不按文案里碰运气的关键词', () => {
  assert.equal(FLOW_NEVER_INGESTED_CODE, 20009)
  assert.equal(isNeverIngestedError({ code: 20009 }), true)

  // 20005 是「采过资产，但这次问到的窗口没有可用数据」，处置不同：
  // 那一条让人去补那次采集，这一条让人去接上流量来源。
  assert.equal(isNeverIngestedError({ code: 20005 }), false)
  assert.equal(isNeverIngestedError({ code: 20004 }), false)
  assert.equal(isNeverIngestedError(new Error('boom')), false)
  assert.equal(isNeverIngestedError(null), false)
})

// 这一条钉住的是「别再用文案关键词判状态」。
//
// 原先 CollectionPage 用 /从来没有过|从未|20009/ 去匹配错误字符串，而后端那
// 句话是「这个集群**还没有过**任何一次流量摄入」—— 三个关键词一个都不中。
// 它一直是绿的，因为落回的 view(null) 恰好也是对的那一句；哪天它不再恰好，
// 屏幕上就会出现一句没人写过的话。
test('后端那句真实文案里，一个凭关键词的匹配都不该被依赖', () => {
  const real = '这个集群还没有过任何一次流量摄入：采集器尚未部署，或流量日志尚未开启'
  assert.doesNotMatch(real, /从来没有过|从未|20009/)
})

// 断言的是**渲染出来的形状**，不是"这几个标识符在文件里出现过"。
//
// 第一版这条守卫写成 `assert.match(src, /flowIngestView/)`，而把整个分支删掉、
// 只留一行没人用的 import，它照样是绿的 —— 一条只证明了 import 语句还在的守卫。
// 下面三条各自钉住一个必须存在的表达式，删掉分支时三条一起红。
test('流量页把「从未摄入过」渲染成指示，而不是一行红字', () => {
  const src = readFileSync(join(import.meta.dirname, '../src/pages/FlowsPage.tsx'), 'utf8')
  const stripped = src.replace(/\/\*[\s\S]*?\*\//g, '').replace(/^\s*\/\/.*$/gm, '')

  // 一、判据是业务码，取自那次失败本身。
  assert.match(stripped, /neverIngested\s*=\s*isNeverIngestedError\(\s*cause\s*\)/,
    'FlowsPage 必须按业务码判「从未摄入过」，而不是猜错误文案里的关键词')

  // 二、红字那一行必须被它挡住。挡不住，操作者读到的是"这一屏坏了"。
  assert.match(stripped, /error\s*&&\s*!neverIngested/,
    '「从未摄入过」不是读取故障，不能同时再渲染一行红色错误')

  // 三、那句话必须真的被渲染出来，且取自 flowIngestView —— 采集页说的是同一句。
  //     两屏对同一个状态给两套指示，人会按先看到的那一屏行动。
  const ingest = /(\w+)\s*=\s*flowIngestView\(\s*null\s*\)/.exec(stripped)
  assert.ok(ingest, 'FlowsPage 必须走 flowIngestView(null) 拿那句话，自己再写一句会与采集页分叉')
  const v = ingest[1]
  assert.match(stripped, new RegExp(`\\{\\s*${v}\\.headline\\s*\\}`), 'headline 没有被渲染')
  assert.match(stripped, new RegExp(`\\{\\s*${v}\\.action\\s*\\}`), 'action 没有被渲染')
})
