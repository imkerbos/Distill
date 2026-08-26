import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

import {
  DISAGREEMENT_HELP, MAX_UNDER_PERMISSIVE_RATE,
  reconcileView, type ReconcileView,
} from '../src/pages/reconcileView.ts'
import type { ReconciliationReport } from '../src/api/types'

const PAGE_SOURCE = readFileSync(new URL('../src/pages/QualityPage.tsx', import.meta.url), 'utf8')

function report(opts: Partial<ReconciliationReport> = {}): ReconciliationReport {
  return {
    cluster: 'prod-asia-1',
    window: { from: '2026-08-26T00:00:00Z', to: '2026-08-26T00:15:00Z' },
    sourceReportsVerdicts: true,
    report: {
      total: 10,
      overall: {
        AGREE: 8, SOURCE_SILENT: 0, PLATFORM_UNKNOWN: 0,
        DISAGREE_OVER_PERMISSIVE: 1, DISAGREE_UNDER_PERMISSIVE: 1,
      },
      bySubject: [],
    },
    ...opts,
  }
}

/**
 * **来源不报判定时，一致率必须显示成"对不了账"，不是 0%。**
 *
 * NODE_CONNTRACK 接入与合成数据集恒为此。显示 0% 会被读成"平台全错"，
 * 显示 100% 会被读成"平台全对"，两者都是编的 —— 事实是这条接入方式
 * 拿不到 ground truth。
 */
test('来源不报判定时不给一致率数字', () => {
  const v = reconcileView(report({
    sourceReportsVerdicts: false,
    report: {
      total: 5, bySubject: [],
      overall: {
        AGREE: 0, SOURCE_SILENT: 5, PLATFORM_UNKNOWN: 0,
        DISAGREE_OVER_PERMISSIVE: 0, DISAGREE_UNDER_PERMISSIVE: 0,
      },
    },
  }))
  assert.equal(v.available, false)
  assert.match(v.unavailableReason, /对不了账|不报判定/)
  assert.equal(v.rateText, '—')
})

/**
 * 有可比对连接但一条都没有时，同样答不出来。
 *
 * 分母为零而显示 100%，是这一屏最容易犯的错：它会让一个什么都没比过的
 * 集群看起来最可信。
 */
test('没有可比对连接时不给一致率数字', () => {
  const v = reconcileView(report({
    report: {
      total: 7, bySubject: [],
      overall: {
        AGREE: 0, SOURCE_SILENT: 0, PLATFORM_UNKNOWN: 7,
        DISAGREE_OVER_PERMISSIVE: 0, DISAGREE_UNDER_PERMISSIVE: 0,
      },
    },
  }))
  assert.equal(v.available, false)
  assert.equal(v.rateText, '—')
})

/** 一致率的分母只含可比对的三类，与后端同一口径。 */
test('一致率按可比对连接算', () => {
  const v = reconcileView(report())
  assert.equal(v.available, true)
  assert.equal(v.rateText, '80.0%')
  assert.equal(v.comparable, 10)
})

/**
 * **两个方向的分歧必须分开显示，且低估那一档要显著。**
 *
 * 低估放行面（平台判 DENY、集群实际放行）是唯一能绕过 dry-run 造成阻断的
 * 分歧；高估只会多生成一条规则。合成一个"分歧率"会让前者被后者稀释。
 */
test('两个方向分开显示，低估那一档更重', () => {
  const v = reconcileView(report())
  assert.equal(v.under.count, 1)
  assert.equal(v.over.count, 1)
  assert.notEqual(v.under.tone, v.over.tone, '两个方向被渲染成同一档轻重')
  assert.match(v.under.help, /阻断|dry-run/)
})

/** 超过门禁阈值的 workload 要被标出来 —— 它们的推荐推不出去。 */
test('超阈的 workload 被标记为会被门禁拦下', () => {
  const v = reconcileView(report({
    report: {
      total: 40, overall: {
        AGREE: 36, SOURCE_SILENT: 0, PLATFORM_UNKNOWN: 0,
        DISAGREE_OVER_PERMISSIVE: 0, DISAGREE_UNDER_PERMISSIVE: 4,
      },
      bySubject: [
        {
          subject: { namespace: 'payment', workload: 'api' },
          counts: {
            AGREE: 6, SOURCE_SILENT: 0, PLATFORM_UNKNOWN: 0,
            DISAGREE_OVER_PERMISSIVE: 0, DISAGREE_UNDER_PERMISSIVE: 4,
          },
        },
        {
          subject: { namespace: 'shop', workload: 'web' },
          counts: {
            AGREE: 30, SOURCE_SILENT: 0, PLATFORM_UNKNOWN: 0,
            DISAGREE_OVER_PERMISSIVE: 0, DISAGREE_UNDER_PERMISSIVE: 0,
          },
        },
      ],
    },
  }))
  const rows = v.subjects
  assert.equal(rows.length, 2)
  // 有问题的排在最前：这一屏的读者要先看到会被拦的那些。
  assert.equal(rows[0].label, 'payment/api')
  assert.equal(rows[0].blocked, true, `低估率 40% 超过阈值 ${MAX_UNDER_PERMISSIVE_RATE}`)
  assert.equal(rows[1].blocked, false)
})

/** 阈值与后端那个常量必须是同一个数，否则界面说的"会被拦"与实际拦的不是一回事。 */
test('前端阈值与后端常量一致', () => {
  const goSource = readFileSync(
    new URL('../../internal/httpapi/writeback_handler.go', import.meta.url), 'utf8')
  const m = goSource.match(/maxUnderPermissiveRate = ([\d.]+)/)
  assert.notEqual(m, null, '后端那个常量改名了')
  assert.equal(Number(m![1]), MAX_UNDER_PERMISSIVE_RATE,
    '前端标注"会被门禁拦"的阈值与后端实际拦的阈值对不上')
})

/** 页面必须真的渲染它 —— 纯逻辑算得再对，调用点没了就等于没做。 */
test('质量页渲染了一致率与两类分歧', () => {
  assert.match(PAGE_SOURCE, /reconcileView\(/, '质量页没有调用 reconcileView')
  for (const needle of ['rateText', 'under', 'over', 'subjects']) {
    assert.equal(PAGE_SOURCE.includes(needle), true, `页面没有渲染 ${needle}`)
  }
  // 文案本身住在视图层（DISAGREEMENT_HELP），页面渲染的是它 —— 断言的是
  // **调用点**："那段解释真的到屏幕上了"。把字面量抄进页面反而会让两处分家。
  assert.match(PAGE_SOURCE, /\{rv\.under\.help\}/,
    '低估那一档的说明没有渲染出来 —— 一个没有解释的数字没人知道该怎么办')
  assert.match(PAGE_SOURCE, /\{rv\.over\.help\}/, '高估那一档的说明没有渲染出来')
  assert.notEqual(DISAGREEMENT_HELP.under.trim(), '', '视图层的说明是空的')
})

export type { ReconcileView }
