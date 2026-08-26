import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { join } from 'node:path'

import { ALL_DRIFT_RESULTS, driftView, ALL_CLUSTER_DRIFT_RESULTS, clusterDriftView } from '../src/pages/driftView.ts'
import type { DriftResult } from '../src/api/types.ts'

const PAGE_SOURCE = readFileSync(
  join(import.meta.dirname, '..', 'src', 'pages', 'ClustersPage.tsx'), 'utf8')

test('五种结论各有各的说法，一个都不塌进别的', () => {
  const seen = new Set<string>()
  for (const r of ALL_DRIFT_RESULTS) {
    const v = driftView(r)
    assert.ok(v.label, `${r} 没有标签`)
    assert.notEqual(v.label, r, `${r} 的"标签"就是它自己`)
    assert.ok(v.action, `${r} 没有说该做什么`)
    assert.ok(!seen.has(v.label), `${r} 与另一个结论共用同一句话`)
    seen.add(v.label)
  }
})

test('UNKNOWN 绝不能读成一致', () => {
  // 一次网络抖动读成"一致"，操作者就以为下发的东西还在，而它可能早被人
  // 删了（design doc 2026-08-18-drift-detection §3）。
  const unknown = driftView('UNKNOWN')
  const inSync = driftView('IN_SYNC')
  assert.notEqual(unknown.tone, inSync.tone, 'UNKNOWN 与 IN_SYNC 的呈现一样')
  assert.match(unknown.label, /没能|无法|不知道/)
})

test('ANCHOR_MISSING 说的是历史被改写，不是内容变了', () => {
  // 与 DRIFTED 分开：那条历史里我们那次提交连同它的审计线索一起没了，
  // 处置是去查谁 force push 了，不是重推一次。
  const v = driftView('ANCHOR_MISSING')
  assert.match(v.label + v.action, /历史|force|改写/)
  assert.notEqual(v.action, driftView('DRIFTED').action)
})

test('未登记的取值按不知道处理，不按一致', () => {
  // 与 dataSourceView 同一条纪律：读不懂的取值不能当成最让人安心的那个。
  const v = driftView('SOMETHING' as DriftResult)
  assert.equal(v.tone, driftView('UNKNOWN').tone)
})

test('页面渲染了漂移结论', () => {
  assert.ok(PAGE_SOURCE.includes('driftView'),
    'ClustersPage 没有渲染漂移结论：锚点又一次没有消费方')
  assert.ok(!/\{false\s*&&/.test(PAGE_SOURCE), 'ClustersPage 里有被写死为 false 的渲染分支')
})

/* ---------------------------------------------------------------------- */
/* 集群漂移：GitOps 到底有没有把仓库里那份落下去                             */
/* ---------------------------------------------------------------------- */

/**
 * 四个结论各有各的处置，且**未登记的取值按 UNKNOWN 处理**。
 *
 * 与 driftView 同一条纪律：一个读不懂的取值不能当成最让人安心的那个 ——
 * CONVERGED 会让操作者以为下发已经生效。
 */
test('每个集群漂移结论都有说法，未登记取值落到 UNKNOWN', () => {
  for (const r of ALL_CLUSTER_DRIFT_RESULTS) {
    const v = clusterDriftView(r)
    assert.notEqual(v.label.trim(), '', `${r} 没有说法`)
    assert.notEqual(v.action.trim(), '', `${r} 没有处置`)
  }
  const unknown = clusterDriftView(undefined)
  assert.deepEqual(unknown, clusterDriftView('UNKNOWN'))
  // @ts-expect-error 故意传一个后端将来才会有的取值
  assert.deepEqual(clusterDriftView('SOMETHING_NEW'), clusterDriftView('UNKNOWN'))
})

/**
 * 只有 CONVERGED 是"绿"的。
 *
 * PENDING（仓库有集群没有）与 CLUSTER_AHEAD（集群有仓库没有）都是要人去看的
 * 状态；UNKNOWN 更不能是绿的 —— 那是"平台没看全"。
 */
test('只有 CONVERGED 是 ok 档', () => {
  for (const r of ALL_CLUSTER_DRIFT_RESULTS) {
    const tone = clusterDriftView(r).tone
    if (r === 'CONVERGED') assert.equal(tone, 'ok')
    else assert.notEqual(tone, 'ok', `${r} 被标成了"没问题"`)
  }
})
