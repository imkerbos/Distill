import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { join } from 'node:path'

import { flowIngestView, missingEvidence } from '../src/pages/flowIngestView.ts'
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
