import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

import {
  DISAGREEMENT_HELP, MAX_UNDER_PERMISSIVE_RATE,
  coverageView, humanSeconds,
  MAX_SAMPLES_PER_CLASS, reconcileView, sampleRows, SAMPLES_HELP,
  TREND_HELP, trendRows, type ReconcileView,
} from '../src/pages/reconcileView.ts'
import type {
  ReconciliationReport, ReconciliationSample, TrendPoint,
} from '../src/api/types'

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
    samples: [],
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

function sample(opts: Partial<ReconciliationSample> = {}): ReconciliationSample {
  return {
    subject: { namespace: 'payment', workload: 'api' },
    class: 'DISAGREE_UNDER_PERMISSIVE',
    source: 'prod/shop/web-1',
    dest: 'prod/payment/api-1',
    protocol: 'TCP',
    port: 8080,
    at: '2026-08-26T10:00:00Z',
    ...opts,
  }
}

// 证据行要一眼看出方向与连接。
test('分歧证据渲染方向与连接', () => {
  const [row] = sampleRows([sample()])
  assert.equal(row.subject, 'payment/api')
  assert.equal(row.blocking, true, 'UNDER_PERMISSIVE 是能造成阻断的那一类')
  assert.match(row.classLabel, /平台判 DENY/)
  assert.match(row.connection, /prod\/shop\/web-1 → prod\/payment\/api-1/)
  assert.match(row.connection, /TCP\/8080/)
})

// 高估方向不上"会阻断"的语义色：它让安全性变差、可用性无损，与低估不是一回事。
test('高估方向不标成会造成阻断', () => {
  const [row] = sampleRows([sample({ class: 'DISAGREE_OVER_PERMISSIVE' })])
  assert.equal(row.blocking, false)
  assert.match(row.classLabel, /集群拦下/)
})

// 没有 workload 归属标签时不能渲染成 "ns/" 断尾 —— 拿着它无法行动。
test('无归属标签的主体说清为什么', () => {
  const [row] = sampleRows([sample({ subject: { namespace: 'legacy', workload: '' } })])
  assert.match(row.subject, /没有 workload 归属标签/)
})

// **后端已经排好序，前端不得重排。**
//
// 一份在两处各排一次的清单迟早会有两种次序，而操作者拿界面上的第 3 条去对
// 后端日志时，对不上号比排序不好看严重得多。
test('证据保持后端给的次序', () => {
  const rows = sampleRows([
    sample({ port: 1 }), sample({ port: 2, class: 'DISAGREE_OVER_PERMISSIVE' }), sample({ port: 3 }),
  ])
  assert.deepEqual(rows.map(r => r.connection.match(/\/(\d+)$/)?.[1]), ['1', '2', '3'])
})

// 时间不合法时原样回显，不显示 Invalid Date。
test('不合法的时刻原样回显', () => {
  const [row] = sampleRows([sample({ at: '不是时间' })])
  assert.equal(row.atText, '不是时间')
})

// null 与空数组都当作"没有证据"。
test('没有证据时是空清单', () => {
  assert.deepEqual(sampleRows(null), [])
  assert.deepEqual(sampleRows([]), [])
})

// 说明必须交代"这是抽样"，并且条数与后端真的留下的条数一致。
//
// 一个只列了 5 条的清单，读的人会以为分歧只有 5 条；而如果界面说 5、后端留
// 10，他会以为自己看到了全部（同分歧阈值那条）。
test('证据说明写明这是抽样，条数与后端一致', () => {
  assert.match(SAMPLES_HELP, /抽样/)
  assert.match(SAMPLES_HELP, new RegExp(`至多留 ${MAX_SAMPLES_PER_CLASS} 条`))

  const backend = readFileSync(
    new URL('../../internal/reconcile/reconcile.go', import.meta.url), 'utf8')
  const m = backend.match(/MaxSamplesPerClass = (\d+)/)
  assert.ok(m, '后端没有 MaxSamplesPerClass 了，界面上那个数字失去了依据')
  assert.equal(Number(m[1]), MAX_SAMPLES_PER_CLASS,
    '界面说的条数与后端真的留下的条数不一致：读的人会以为自己看到了全部')
})

// 质量页必须真的渲染证据表，且排在主体表之后。
test('质量页渲染分歧证据，排在主体表之后', () => {
  const subjects = PAGE_SOURCE.indexOf('rv.subjects.map')
  const samples = PAGE_SOURCE.indexOf('rv.samples.map')
  assert.ok(samples > 0, '质量页没有渲染分歧证据')
  assert.ok(subjects < samples, '证据表要排在主体表之后：先看谁对不上，再看具体哪几条')
})

function point(opts: Partial<TrendPoint> = {}): TrendPoint {
  return {
    windowFrom: '2026-08-26T10:00:00Z',
    windowTo: '2026-08-26T10:01:00Z',
    computedAt: '2026-08-26T10:01:00Z',
    rate: 0.97,
    comparable: 100,
    under: 2,
    over: 1,
    platformUnknown: 5,
    sourceReports: true,
    ...opts,
  }
}

// **算不出一致率的那几轮必须画不出柱子，也不显示 0%。**
//
// 把"算不出"渲染成 0，那一行读起来是"那天全错了"，而事实是那天没有可比对
// 的连接。一条会说谎的曲线比没有曲线更糟。
test('算不出的一致率不渲染成零', () => {
  const [row] = trendRows([point({ rate: null, comparable: 0, under: 0, over: 0 })])
  assert.equal(row.rateText, '—')
  assert.equal(row.bar, null, '算不出时不能给柱高，否则会画出一根贴地的柱子')
  assert.notEqual(row.missingReason, '', '算不出必须说明为什么')
})

// 两种"算不出"的原因要分开：处置完全不同。
test('区分来源不报判定与这一轮没有可比对连接', () => {
  const [noSource] = trendRows([point({ rate: null, sourceReports: false })])
  const [noData] = trendRows([point({ rate: null, sourceReports: true })])
  assert.match(noSource.missingReason, /不报判定/)
  assert.match(noData.missingReason, /没有既被平台判出结论/)
  assert.notEqual(noSource.missingReason, noData.missingReason)
})

// 分母必须跟着走：基于 3 条的 100% 与基于 3 万条的 100% 不是一回事。
test('每一轮带上分母', () => {
  const [row] = trendRows([point({ rate: 1, comparable: 3, under: 0, over: 0 })])
  assert.equal(row.rateText, '100.0%')
  assert.equal(row.comparable, 3)
})

// 超过门禁阈值的那几轮要标出来，口径与主体表同一个常量。
test('低估率超阈的轮次被标出', () => {
  const [bad] = trendRows([point({ comparable: 100, under: 20 })])
  const [ok] = trendRows([point({ comparable: 100, under: 1 })])
  assert.equal(bad.blocked, true)
  assert.equal(ok.blocked, false)
})

// 不补齐缺失的时段：补齐要凭空造点，而造出来的点在图上与真实观测长得一样。
test('不补齐缺失时段', () => {
  const rows = trendRows([
    point({ windowFrom: '2026-08-26T10:00:00Z' }),
    // 中间隔了很久 —— 采集断过，那一段就该是空的。
    point({ windowFrom: '2026-08-01T10:00:00Z' }),
  ])
  assert.equal(rows.length, 2, '缺失的时段被凭空补上了点')
})

test('没有历史时是空清单', () => {
  assert.deepEqual(trendRows(null), [])
  assert.deepEqual(trendRows([]), [])
})

// 说明要写明"看走向不看绝对值"。
test('趋势说明写明读法', () => {
  assert.match(TREND_HELP, /走向/)
  assert.match(TREND_HELP, /算不出/)
})

// 质量页必须真的渲染走向，且排在证据之后。
test('质量页渲染一致率走向，排在证据之后', () => {
  const samples = PAGE_SOURCE.indexOf('rv.samples.map')
  const trend = PAGE_SOURCE.indexOf('trendData.map')
  assert.ok(trend > 0, '质量页没有渲染一致率走向')
  assert.ok(samples < trend, '走向要排在证据之后：先看这一轮，再看它在变好还是变坏')
})

// 跨度与实际观测必须并列，差额单独给。
//
// 一个集群 90 天前摄入过一次、之后停了 89 天：只报跨度读起来是"我们看了
// 三个月"，而真正被观测到的只有两分钟。
test('观测覆盖并列跨度与实际观测', () => {
  const v = coverageView({
    spanSeconds: 90 * 86400, coveredSeconds: 120, gapSeconds: 90 * 86400 - 120,
  })
  assert.equal(v.available, true)
  assert.match(v.spanText, /90 天/)
  assert.equal(v.coveredText, '2 分')
  assert.equal(v.alarming, true, '间隙占了跨度的绝大部分，必须报警')
  assert.match(v.alarm, /采集链路/, '要指向该去查的地方，而不是让人干等')
})

// 连续观测不报警。
//
// 没有这一条，一个恒报警的实现照样能让上面那条通过，而那等于把这个信号
// 变成永远为真、因而没有信息的一句话。
test('连续观测不报警', () => {
  const v = coverageView({ spanSeconds: 7 * 86400, coveredSeconds: 7 * 86400, gapSeconds: 0 })
  assert.equal(v.alarming, false)
  assert.equal(v.alarm, '')
  assert.equal(v.gapText, '0 秒')
})

// **一次成功摄入都没有时是 null，不是三个零。**
//
// 0/0/0 读起来是"观测过、但一秒都没覆盖到"，而事实是还没开始 —— 前者查
// 采集链路，后者去把采集器跑起来。
test('没有摄入时说清是还没开始', () => {
  const v = coverageView(null)
  assert.equal(v.available, false)
  assert.match(v.unavailableReason, /还没开始/)
  assert.equal(v.gapText, '—', '算不出时不能显示成 0')
})

// 时长写法与后端 humanDuration 一致：同一段时长在写回拒绝理由与这一屏上
// 必须是同一种写法，否则会被读成两个不同的数。
test('时长写法可读且与后端同口径', () => {
  assert.equal(humanSeconds(7 * 86400), '7 天')
  assert.equal(humanSeconds(86400 + 3600 * 5), '1 天 5 小时')
  assert.equal(humanSeconds(3600 + 120), '1 小时 2 分')
  assert.equal(humanSeconds(90), '1 分')
  assert.equal(humanSeconds(30), '30 秒')
  assert.equal(humanSeconds(-1), '—', '负数是数据有问题，不能显示成一个时长')
})

// 观测覆盖要排在走向之前：先知道看了多久，再读看出了什么。
test('质量页把观测覆盖排在走向之前', () => {
  const coverage = PAGE_SOURCE.indexOf('观测覆盖')
  const trend = PAGE_SOURCE.indexOf('一致率走向')
  assert.ok(coverage > 0, '质量页没有渲染观测覆盖')
  assert.ok(coverage < trend, '覆盖要排在走向之前：那些数字算在多长的观测上决定了它们值多少')
})
