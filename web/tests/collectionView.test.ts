import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { join } from 'node:path'

import {
  COLLECTION_FEEDS_NOTHING, COLLECTION_NO_RUN_CODE, COLLECTION_UNKNOWN_CLUSTER_CODE,
  FAILURE_REASON_ACTION, FAILURE_REASON_LABEL,
  collectionCoverageNote, collectionDuration, collectionRows, collectionSummaryView,
  RUN_ERROR_REASON_ACTION, RUN_ERROR_REASON_LABEL,
  collectionWarningRows, isNoRunError, isUnknownClusterError, runErrorNote, summaryStatusTone,
} from '../src/pages/collectionView.ts'
import type { CollectionResource, CollectionSummary } from '../src/api/types.ts'

const src = (...parts: string[]) =>
  readFileSync(join(import.meta.dirname, '..', 'src', ...parts), 'utf8')

const PAGE_SOURCE = src('pages', 'CollectionPage.tsx')
const CLIENT_SOURCE = src('api', 'client.ts')
const APP_SOURCE = src('App.tsx')
const SHELL_SOURCE = src('components', 'AppShell.tsx')

/** 一次部分成功的采集：一个真实的 0、一次 FORBIDDEN、一个正常计数。 */
function resources(): CollectionResource[] {
  return [
    { resource: 'NAMESPACE', count: 0 },
    { resource: 'NETWORKPOLICY', failureReason: 'FORBIDDEN' },
    { resource: 'POD', count: 42 },
  ]
}

function summary(over: Partial<CollectionSummary> = {}): CollectionSummary {
  return {
    clusterId: 'prod-asia-1',
    runId: 'run-1',
    startedAt: '2026-08-16T03:00:00Z',
    finishedAt: '2026-08-16T03:00:09Z',
    status: 'PARTIAL',
    resources: resources(),
    warnings: [{ kind: 'POD_IP_OUTSIDE_CLUSTER', count: 3 }],
    warningTotal: 3,
    ...over,
  }
}

/* ---------------------------------------------------------------------- */
/* 1. 失败与 0 必须分得开（spec §4.2）                                       */
/* ---------------------------------------------------------------------- */

/**
 * 一次采集失败的资源在表格里**没有数字**。
 *
 * 这是这一屏存在的一半理由：「NetworkPolicy = 0」与「NetworkPolicy 因权限
 * 不足根本没采到」在计数上长得一模一样，而后者被读成前者的后果是平台
 * 推荐一份 default-deny。断言 countText 是空串而不是"不等于某个值"——
 * 一个能拿到数字的字段迟早会被渲染出来。
 */
test('采集失败的资源不带任何条数，连 0 都不带', () => {
  const failed = collectionRows(resources()).find((r) => r.resource === 'NETWORKPOLICY')

  assert.ok(failed, 'NETWORKPOLICY 这一行消失了 —— 缺席会被读成「这个集群没有策略」')
  assert.equal(failed.observed, false)
  assert.equal(failed.countText, '',
    '没采到的资源带上了条数文案 —— 那个数字会被读成「这个集群没有任何策略」')
  assert.doesNotMatch(failed.countText, /\d/, '没采到的资源不该出现任何数字')
  assert.equal(failed.failureReason, 'FORBIDDEN')
  assert.equal(failed.failureLabel, FAILURE_REASON_LABEL.FORBIDDEN)
})

/**
 * FORBIDDEN 的处置必须说"重试没用"。
 *
 * 它是唯一一种「集群是好的、是我们没被授权」的失败，而重试恰恰是它最常
 * 被误配的处置方式（migrations/000009 上的同一段注释）。
 */
test('FORBIDDEN 的处置指向改 RBAC，而不是重试', () => {
  assert.match(FAILURE_REASON_ACTION.FORBIDDEN, /重试没有用/)
  assert.match(FAILURE_REASON_ACTION.FORBIDDEN, /权限/)
})

/**
 * 一个真实的 0 必须仍然是 0。
 *
 * 与上一条互为反面。只挡住失败那一半，很容易顺手把所有 0 都藏起来 ——
 * 而「这个集群确实一个 Namespace 都没有」是一个采到了的事实。
 */
test('采到 0 条仍然显示 0，且不带失败原因', () => {
  const zero = collectionRows(resources()).find((r) => r.resource === 'NAMESPACE')

  assert.ok(zero)
  assert.equal(zero.observed, true)
  assert.equal(zero.countText, '0', '一个真实的 0 被藏起来了')
  assert.equal(zero.failureReason, '', '一个采到了的资源被标成了失败')
  assert.equal(zero.failureLabel, '')
})

/**
 * ReplicaSet 的缺席既不是 0 也不是失败。
 *
 * 它只用于把 Pod 的 ownerRef 链解到顶层控制器，从来不是被观测的资产
 * （spec §4.2）。按枚举补齐会造出一条凭空的失败，把操作者引去查一个
 * 不存在的 RBAC 问题。
 */
test('不按枚举补齐：ReplicaSet 不会凭空出现一行', () => {
  const rows = collectionRows(resources())

  assert.equal(rows.length, 3, '行数与报文里的条数不一致 —— 有资源类型被补了出来')
  assert.equal(rows.find((r) => r.resource === 'REPLICASET'), undefined,
    'REPLICASET 被补出了一行 —— 它从不被观测，任何显示都是凭空造的')
})

/** 未收录的原因照原样显示，不丢弃、不留空。 */
test('未收录的失败原因照原样显示', () => {
  const rows = collectionRows([{ resource: 'SERVICE', failureReason: 'BRAND_NEW_REASON' }])

  assert.equal(rows[0].failureReason, 'BRAND_NEW_REASON')
  assert.equal(rows[0].failureLabel, 'BRAND_NEW_REASON',
    '没有标签的原因被显示成空白 —— 少显示一种成因等于把一类系统性问题藏起来')
})

/** 覆盖说明必须同时报出没采到的类数，不能只报成功那一半。 */
test('覆盖说明把没采到的类数一起报出来', () => {
  assert.match(collectionCoverageNote(collectionRows(resources())), /有 1 类没采到/)
  assert.equal(
    collectionCoverageNote(collectionRows([{ resource: 'POD', count: 1 }])),
    '1 类资源全部采到',
  )
})

/** PARTIAL 不能是中性色：视觉上与全绿一样就等于把它读成了成功。 */
test('PARTIAL 与未收录的状态都不按正常显示', () => {
  assert.equal(summaryStatusTone('OK'), undefined)
  assert.equal(summaryStatusTone('PARTIAL'), 'unknown')
  assert.equal(summaryStatusTone('FAILED'), 'deny')
  assert.equal(summaryStatusTone('SOMETHING_NEW'), 'unknown',
    '未收录的状态被当成正常 —— 一个新枚举值会静默地显示成全绿')
})

/* ---------------------------------------------------------------------- */
/* 2. 这一屏不参与任何结论，必须说出来（spec §5.2）                            */
/* ---------------------------------------------------------------------- */

/**
 * 那句话必须说清三件事：不参与结论、其余各屏跑的是合成数据、拼起来的后果。
 *
 * 只说「数据仅供参考」是不够的：它不告诉操作者 dry-run 的结论与这一屏
 * 没有关系，而那正是他会做出的错误联想。
 */
test('页面必须声明这些资产不参与任何结论', () => {
  assert.match(COLLECTION_FEEDS_NOTHING, /不参与平台的任何结论/)
  assert.match(COLLECTION_FEEDS_NOTHING, /合成数据集/)
  assert.match(COLLECTION_FEEDS_NOTHING, /dry-run/)
})

/**
 * 那句话必须真的被页面引用。
 *
 * **这条断言的力度有限，必须说清楚：** 它只能证明这个符号出现在页面
 * 源码里，证明不了它被渲染到了一个看得见的位置。前端没有 DOM 测试设施，
 * 本轮也不允许新增依赖，因此「包在一个恒假的条件里」这类缺陷这里抓不住 ——
 * 本项目此前正是这样绿着过一轮。
 */
test('资产采集页引用了那句声明，并把它放在提示条里', () => {
  assert.ok(PAGE_SOURCE.includes('COLLECTION_FEEDS_NOTHING'),
    'CollectionPage.tsx 不再引用 COLLECTION_FEEDS_NOTHING —— 常量测得再全，页面已经不说这句话了')
  assert.match(PAGE_SOURCE, /<Notice>\{COLLECTION_FEEDS_NOTHING\}<\/Notice>/,
    '那句声明不再无条件地渲染在提示条里')
})

/** 页面必须从纯逻辑层取派生值，不自己拼一份。 */
test('资产采集页的派生值来自 collectionView，不是页面自己算的', () => {
  assert.ok(PAGE_SOURCE.includes('collectionSummaryView'),
    'CollectionPage.tsx 不再走 collectionSummaryView —— 上面那些断言测的就不是页面在用的东西了')
})

/**
 * 页面源码里不得出现把缺席补成 0 的写法。
 *
 * 这是上面那条源码断言够不着的地方的一道补：`row.countText || 0` 或
 * `count ?? 0` 是一次改动里随手写得出来的表达式，而它的后果正是这一屏
 * 要防的那件事。
 */
test('页面源码里没有把缺席补成 0 的兜底写法', () => {
  for (const pattern of [/countText\s*\|\|/, /count\s*\?\?/, /count\s*\|\|/]) {
    assert.doesNotMatch(PAGE_SOURCE, pattern,
      `页面里出现了 ${pattern} —— 一个补出来的 0 会把「没被授权看」显示成「没有」`)
  }
})

/* ---------------------------------------------------------------------- */
/* 3. 三种"没有数字"必须分得开                                               */
/* ---------------------------------------------------------------------- */

test('20004 被识别成「从未采集过」，其余失败不是', () => {
  assert.equal(COLLECTION_NO_RUN_CODE, 20004)
  assert.equal(isNoRunError({ code: 20004 }), true)
  assert.equal(isNoRunError({ code: 50002 }), false, '依赖不可用不是「从未采集过」')
  assert.equal(isNoRunError({ code: 50001 }), false, '一次读取故障不能被伪装成「还没采过」')
  assert.equal(isNoRunError(new Error('boom')), false)
  assert.equal(isNoRunError(undefined), false)
})

/**
 * 「集群不存在」与「从未采集过」必须是两个码、两条判定。
 *
 * 后端读取端查的是 collection_run，对一个拼错的集群 ID 同样查不到行 ——
 * 两者共用一个码时，一次 URL 里的拼写错误会显示成「还没有采集记录」，
 * 于是操作者去查采集器为什么没跑，而真正的原因是他打错了字。
 *
 * 两个方向都断言：各自认得自己那个码，且**互不认对方的** ——
 * 少了后半条，两个都返回 true 的实现照样能过。
 */
test('20002 是「集群不存在」，与「从未采集过」互不相认', () => {
  assert.equal(COLLECTION_UNKNOWN_CLUSTER_CODE, 20002)
  assert.notEqual(COLLECTION_UNKNOWN_CLUSTER_CODE, COLLECTION_NO_RUN_CODE)

  assert.equal(isUnknownClusterError({ code: 20002 }), true)
  assert.equal(isUnknownClusterError({ code: 20004 }), false,
    '「还没采过」被当成了「集群不存在」')
  assert.equal(isNoRunError({ code: 20002 }), false,
    '一次集群 ID 拼写错误被显示成「还没有采集记录」')
  assert.equal(isUnknownClusterError(new Error('boom')), false)
  assert.equal(isUnknownClusterError(undefined), false)
})

/**
 * 「这一轮根本没开始」必须与「采到了零个资源」分开。
 *
 * 这是这条端点最容易骗人的一种状态：采集器被拉起、在读集群之前就失败
 * 退出。没有这条提示，页面上是一张空表 —— 与一次真的采到零资源的运行
 * 在界面上完全一样，而两者的下一步动作完全不同。
 */
test('没能开始的一轮讲清它不是「采到了零个资源」', () => {
  const note = runErrorNote('READ_ONLY_UNPROVEN')
  assert.match(note, /这一轮没有开始/)
  assert.match(note, /不是「采到了零个资源」/,
    '提示没有把「根本没看过」与「看了、什么都没有」分开')
  assert.match(note, /RBAC/, '没有告诉操作者下一步该查什么')

  // 正常的一轮必须**没有**这条提示：每一轮都顶着一句话，
  // 会让真正出事的那一轮看起来和平常一样。
  assert.equal(runErrorNote(undefined), '')
  assert.equal(runErrorNote(''), '')
})

/** 未收录的原因照原样显示，不丢弃、不留空。 */
test('不认识的「没能开始」原因照原样显示', () => {
  const note = runErrorNote('SOMETHING_NEW')
  assert.match(note, /SOMETHING_NEW/,
    '少显示一种成因等于把一类系统性问题藏起来')
})

/** 三种「没能开始」的原因各自有标签与处置，且处置方向不同。 */
test('每种「没能开始」的原因都有标签与处置', () => {
  for (const reason of ['CREDENTIAL_UNAVAILABLE', 'CLIENT_UNAVAILABLE', 'READ_ONLY_UNPROVEN']) {
    assert.ok(RUN_ERROR_REASON_LABEL[reason], `${reason} 没有标签`)
    assert.ok(RUN_ERROR_REASON_ACTION[reason], `${reason} 没有处置`)
  }
  // 与资源级失败不共用一张表：合成一张会让「NetworkPolicy 被拒」与
  // 「采集器连不上这个集群」落进同一句话。
  assert.notDeepEqual(RUN_ERROR_REASON_LABEL, FAILURE_REASON_LABEL)
})

/** 页面必须在任何数字之上渲染这条提示。 */
test('没能开始的提示排在数字之前', () => {
  assert.match(PAGE_SOURCE, /view\.errorNote \? <Notice>\{view\.errorNote\}<\/Notice> : null/,
    '页面没有渲染「这一轮没能开始」的提示')
  const noticeAt = PAGE_SOURCE.indexOf('view.errorNote')
  const tileAt = PAGE_SOURCE.indexOf('本次采集结果')
  assert.ok(noticeAt >= 0 && tileAt >= 0 && noticeAt < tileAt,
    '提示排在数字之后 —— 操作者会先把一张空表当成事实读一遍')
})

/** 集群不存在的空态必须说它不是「还没采过」。 */
test('集群不存在的空态说明它与「还没采过」不是一回事', () => {
  assert.match(PAGE_SOURCE, /没有这个集群/)
  assert.match(PAGE_SOURCE, /这里说的不是「还没采过」/,
    '空态没有把「打错 ID」与「采集器还没跑」区分开')
})

/**
 * 客户端只把 20002 转成状态，其余一律继续抛。
 *
 * 把所有失败都吞成 NO_RUN，一次持续的采集读取故障会显示成「这个集群
 * 还没被采过」—— 操作者会去等采集器，而问题在别处。
 */
test('客户端只吞下「从未采集过」，其余错误继续抛', () => {
  assert.match(CLIENT_SOURCE, /if \(isNoRunError\(e\)\) return \{ kind: 'NO_RUN' \}/)
  assert.match(CLIENT_SOURCE, /\n\s*throw e\n/, '非 NO_RUN 的错误没有被重新抛出')
})

/** NO_RUN 的空态必须说清它不是「采集过、什么都没采到」。 */
test('从未采集过的空态说明它与「采到空」不是一回事', () => {
  assert.match(PAGE_SOURCE, /还没有过任何一次资产采集/)
  assert.match(PAGE_SOURCE, /这不是「采集过、什么都没采到」/,
    '空态没有把两种「没有数字」区分开 —— 一次持续的采集故障会看起来像一个刚注册的集群')
})

/* ---------------------------------------------------------------------- */
/* 4. 其余                                                                 */
/* ---------------------------------------------------------------------- */

test('耗时算不出来时返回空串，不返回负数或 NaN', () => {
  assert.equal(collectionDuration('2026-08-16T03:00:00Z', '2026-08-16T03:00:09Z'), '9.0 秒')
  assert.equal(collectionDuration('2026-08-16T03:00:09Z', '2026-08-16T03:00:00Z'), '',
    '顺序颠倒时给出了一个负的耗时 —— 一个显然错误的数字会被当成真的读')
  assert.equal(collectionDuration('not-a-time', '2026-08-16T03:00:00Z'), '')
})

test('告警未收录的取值照原样显示', () => {
  const rows = collectionWarningRows([{ kind: 'NEW_KIND', count: 2 }])
  assert.equal(rows[0].label, 'NEW_KIND')
})

test('摘要视图把上面几件事一起交给页面', () => {
  const view = collectionSummaryView(summary())

  assert.equal(view.rows.length, 3)
  assert.equal(view.statusTone, 'unknown')
  assert.match(view.statusLabel, /部分资源没采到/)
  assert.match(view.coverageNote, /1 类没采到/)
  assert.equal(view.duration, '9.0 秒')
  assert.equal(view.warningRows.length, 1)
})

/** 入口与路由都在，且不按角色摘掉（规范 §34：前端不是安全边界）。 */
test('资产采集有自己的路由与导航入口，且不按角色隐藏', () => {
  assert.match(APP_SOURCE, /path="\/collection"/, '路由不在了 —— 这一屏敲地址也进不去')
  assert.match(APP_SOURCE, /<CollectionPage cluster=\{cluster\} \/>/)
  assert.match(SHELL_SOURCE, /\{ to: '\/collection', label: '资产采集' \}/,
    '导航入口不在了')
  assert.doesNotMatch(SHELL_SOURCE, /showsAccountAdminEntry\(identity\.role\)[\s\S]{0,200}\/collection/,
    '采集入口被塞进了角色过滤 —— 隐藏它只会让只读账号以为这块界面不存在')
})
