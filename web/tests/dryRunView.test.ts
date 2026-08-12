import test from 'node:test'
import assert from 'node:assert/strict'

import { dryRunView, wouldOpenEmptyDetail } from '../src/pages/dryRunView.ts'
import type { ChangedFlow, PredictionReport } from '../src/api/types.ts'

function flow(id: string): ChangedFlow {
  return {
    flowId: id, sourceLabel: 'batch/worker', destLabel: 'payment/api',
    protocol: 'TCP', port: 3306, current: 'DENY', predicted: 'ALLOW',
    unknownReason: '', confidence: 'TRUSTED', crossCluster: false, unmanaged: false,
  }
}

function report(opts: {
  broke: string[]
  opened: string[]
  unknown?: Record<string, number>
  trusted?: number
}): PredictionReport {
  return {
    changes: {
      WOULD_BREAK: opts.broke.map(flow),
      WOULD_OPEN: opts.opened.map(flow),
      UNCHANGED: [],
      UNKNOWN: [],
    },
    counts: {
      WOULD_BREAK: opts.broke.length, WOULD_OPEN: opts.opened.length,
      UNCHANGED: 0, UNKNOWN: 0,
    },
    unknownComposition: opts.unknown ?? {},
    trustedCount: opts.trusted ?? 0,
    degradedCount: 0, unratedCount: 0, crossClusterCount: 0, unmanagedCount: 0,
    totalEvaluated: opts.broke.length + opts.opened.length,
  } as PredictionReport
}

/**
 * 这是 spec §5 的那条「不能让它成立的阅读」：tile 显示人工版本、
 * 明细区显示默认版本时，页面会在写着 `0 → 3  +3` 的 tile 正下方
 * 印出「WOULD_OPEN 为 0 是一个真实的 0」——一句关于敞口的假话，
 * 出现在操作者决定要不要接受自己那几次确认的代价的那一屏上。
 *
 * 断言的是「明细区的 report 就是 overridden 这个对象本身」，而不是
 * 「某个计数等于 3」：后者在两套预测碰巧同值时也会通过，而这里要
 * 钉死的是同源，不是同值。
 */
test('明细区与被强调的 tile 数字同源：有人工决定时用 overridden', () => {
  const prediction = report({ broke: ['a', 'b', 'c'], opened: [] })
  const overridden = report({ broke: ['a'], opened: ['x', 'y', 'z'] })

  const view = dryRunView(prediction, overridden, 1)

  assert.equal(view.detail.report, overridden,
    '明细区必须渲染 overridden 那一套，否则清单与上方的箭头右值互相矛盾')
  assert.equal(view.detail.report.changes.WOULD_OPEN.length, 3)
  assert.equal(view.detail.report.changes.WOULD_BREAK.length, 1)
})

/**
 * tile 箭头右端与明细区不是两次各自的选择，而是同一个 report 的两种呈现。
 *
 * 这条断言的是对象同一性（`===`），不是数值相等：C1 的实际形态正是
 * 「78 与 81 都各自算得对，只是不来自同一套预测」，数值相等的断言在
 * 两套预测碰巧同值的 fixture 上一律通过，什么也守不住。
 */
test('被强调的那一列与明细区是同一个 report', () => {
  const prediction = report({ broke: ['a', 'b', 'c'], opened: [] })
  const overridden = report({ broke: ['a'], opened: ['x'] })

  for (const overrideCount of [0, 1, 7]) {
    const view = dryRunView(prediction, overridden, overrideCount)
    assert.equal(view.emphasized, view.detail.report.counts,
      `overrideCount=${overrideCount}：tile 右端的计数必须就是明细区那套预测的计数`)
    assert.equal(view.baseline, prediction.counts,
      `overrideCount=${overrideCount}：tile 左端恒为默认推荐`)
  }
})

/**
 * 空态那句话是一个断言，不是装饰：它声称这个 0 是算出来的。它必须
 * 描述它下面那张表用的那套预测——否则 overridden 放开了 3 条时，
 * 页面照样印「WOULD_OPEN 为 0 是一个真实的 0」。
 */
test('WOULD_OPEN 的空态文案与它描述的那套预测同源', () => {
  const prediction = report({ broke: [], opened: [] })
  const overridden = report({ broke: [], opened: ['x', 'y', 'z'] })

  const view = dryRunView(prediction, overridden, 1)

  // 有覆盖时这句话只会随非空的 WOULD_OPEN 表一起消失（表非空就不渲染
  // 空态），因此这里断言的是它的口径：它说的是"应用人工决定之后"。
  assert.equal(view.detail.report.changes.WOULD_OPEN.length, 3,
    '这套 fixture 下 overridden 有 3 条敞口，空态根本不该出现')
  assert.match(wouldOpenEmptyDetail(view.detail), /应用人工决定之后/)
  assert.doesNotMatch(view.detail.basis, /^以下明细来自默认推荐/)
})

/** 没有人工决定时两套预测恒等，展示默认推荐，文案也回到默认口径。 */
test('无人工决定时用默认推荐', () => {
  const prediction = report({ broke: ['a'], opened: [] })
  const overridden = report({ broke: ['a'], opened: [] })

  const view = dryRunView(prediction, overridden, 0)

  assert.equal(view.showDelta, false)
  assert.equal(view.detail.report, prediction)
  assert.match(wouldOpenEmptyDetail(view.detail), /WOULD_OPEN 为 0 是一个真实的 0。$/)
  assert.doesNotMatch(view.detail.emptyDetail, /人工决定/)
})
