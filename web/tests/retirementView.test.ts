import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

import { RETIREMENT_HELP, retirementView } from '../src/pages/retirementView.ts'
import type { RetirementCandidate, RetirementReport } from '../src/api/types'

const PAGE = readFileSync(new URL('../src/pages/PolicyPage.tsx', import.meta.url), 'utf8')

function candidate(opts: Partial<RetirementCandidate> = {}): RetirementCandidate {
  return {
    namespace: 'argocd', name: 'redis-np',
    retirable: true, wouldBreak: 0, coveredBy: 3,
    ...opts,
  }
}

function report(opts: Partial<RetirementReport> = {}): RetirementReport {
  return {
    cluster: 'kind-local-e2e',
    window: { from: '2026-08-26T00:00:00Z', to: '2026-08-26T01:00:00Z' },
    eligible: true, ineligibleReason: '', truncated: false,
    candidates: [candidate()],
    ...opts,
  }
}

// **给不出建议时不列任何候选。**
//
// 观测不足时算出来的"删掉没影响"只说明那段时间没看见，不说明没有 ——
// 而这份清单指向的动作是删掉正在生效的策略。
test('给不出建议时清单为空且说明原因', () => {
  const v = retirementView(report({
    eligible: false, ineligibleReason: '观测还没覆盖一轮业务周期。',
    candidates: [candidate()],
  }))
  assert.equal(v.available, false)
  assert.equal(v.rows.length, 0, '拒绝给建议却仍然列出了候选')
  assert.match(v.unavailableReason, /业务周期/)
})

// 还撑着流量的排在最前：它们才是操作者要看的。
test('还撑着流量的排在最前', () => {
  const v = retirementView(report({
    candidates: [
      candidate({ name: 'covered-a' }),
      candidate({ name: 'holding', retirable: false, wouldBreak: 12 }),
      candidate({ name: 'covered-b' }),
    ],
  }))
  assert.equal(v.rows[0].label, 'argocd/holding')
  assert.equal(v.rows[0].holding, true)
  assert.match(v.rows[0].detail, /12 条连接会从通变成不通/)
})

// **coveredBy 为 0 时必须点破那个"没影响"多半是假的。**
//
// 该命名空间没有任何主体进候选集，"删掉没连接会断"很可能只是因为那些主体
// 在这段窗口里没有流量 —— 照着它删就会断。
test('没有主体接手时点破那个零', () => {
  const v = retirementView(report({ candidates: [candidate({ coveredBy: 0 })] }))
  assert.match(v.rows[0].detail, /很可能只是因为那些主体在这段窗口里没有流量/)
})

// 有主体接手时给出正面理由，而不只是"删了看起来没事"。
test('有主体接手时给出正面理由', () => {
  const v = retirementView(report({ candidates: [candidate({ coveredBy: 5 })] }))
  assert.match(v.rows[0].detail, /5 个主体在候选集里/)
})

// **截断的清单不得被读成完整的。**
test('截断时说明清单不完整', () => {
  const v = retirementView(report({ truncated: true }))
  assert.match(v.truncationNote, /不完整/)
  assert.match(v.truncationNote, /根本没被算过/)
})

// 一条都没有是一个结论，不是空白。
test('没有旧策略时给出结论', () => {
  const v = retirementView(report({ candidates: [] }))
  assert.notEqual(v.emptyNote, '')
  assert.match(v.emptyNote, /没有需要接管的旧策略/)
})

// null 不抛。
test('没有数据时不渲染', () => {
  const v = retirementView(null)
  assert.equal(v.available, false)
  assert.equal(v.rows.length, 0)
})

// **抬头必须同时说清三件事**，少一件都会让人做错事。
test('抬头说清平台不删、结论不能叠加、窗口外看不见', () => {
  assert.match(RETIREMENT_HELP, /平台不会删除/)
  assert.match(RETIREMENT_HELP, /单独退休它/)
  assert.match(RETIREMENT_HELP, /一起删就断了/)
  assert.match(RETIREMENT_HELP, /这段观测里/)
})

// 界面上不得出现删除入口：平台对被管集群没有写权限。
test('接管区块没有删除入口', () => {
  const start = PAGE.indexOf('function RetirementSection')
  assert.ok(start > 0, '页面没有接管区块')
  const section = PAGE.slice(start)
  assert.doesNotMatch(section.slice(0, 2000), /onClick|<button/,
    '接管区块里出现了可点击的动作 —— 平台对被管集群没有策略写权限')
})

// 取不到时整节不渲染，而不是显示"加载失败"。
//
// 这个端点按管理员鉴权，而 viewer 同样要能看候选策略与 dry-run；
// 一个写着"加载失败"的空区块会让他以为平台坏了。
test('取不到时整节不渲染', () => {
  assert.match(PAGE, /if \(error !== null \|\| data == null\) return null/)
})
