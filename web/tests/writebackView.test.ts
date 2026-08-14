import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { join } from 'node:path'

import {
  writebackCountDrift, writebackPushBody, writebackView,
} from '../src/pages/writebackView.ts'
import type {
  ChangeKind, PolicyPreview, TimeWindow, WritebackPlan,
} from '../src/api/types.ts'

const WINDOW: TimeWindow = { from: '2026-08-01T00:00:00Z', to: '2026-08-08T00:00:00Z' }
/** 另一段时间窗，只用来做反证：它绝不该出现在任何一条写回路径里。 */
const OTHER_WINDOW: TimeWindow = { from: '2026-01-01T00:00:00Z', to: '2026-01-02T00:00:00Z' }

const PAGE_COUNTS: Record<ChangeKind, number> = {
  WOULD_BREAK: 81, WOULD_OPEN: 0, UNCHANGED: 123, UNKNOWN: 44,
}

function preview(opts: { namespace?: string; window?: TimeWindow }): PolicyPreview {
  return {
    cluster: 'prod-asia-1',
    namespace: opts.namespace ?? '',
    window: opts.window ?? WINDOW,
  } as PolicyPreview
}

function plan(opts: Partial<WritebackPlan> = {}): WritebackPlan {
  return {
    repoId: 'repo-policies',
    files: [{ path: 'clusters/prod-asia-1/distill-policy.yaml', content: '# ...' }],
    branch: 'distill/prod-asia-1-20260814T101500Z',
    commitMessage: 'policy: distill 写回 prod-asia-1',
    counts: PAGE_COUNTS,
    extraneous: null,
    existingBranches: null,
    fingerprint: 'a'.repeat(64),
    ...opts,
  }
}

function params(path: string): URLSearchParams {
  return new URL(path, 'http://localhost:4000').searchParams
}

/**
 * 命名空间筛选生效时，入口必须在点击**之前**就不可用，并说出原因。
 *
 * 服务端拒绝（writebackNamespaceMsg）是权威的，这条断言不是它的替代。
 * 但一个点下去才失败的按钮教不会操作者任何东西：他下一步该做的是清掉
 * 筛选，因此理由里必须出现处置方式。
 */
test('命名空间筛选生效时不可用，且理由指出清除筛选', () => {
  const view = writebackView(preview({ namespace: 'payment' }))

  assert.equal(view.available, false, '筛选生效时入口必须不可用')
  assert.equal(view.planPath, '', '不可用时不留下一条仍然可用的出计划路径')
  assert.equal(view.pushPath, '', '不可用时更不能留下一条仍然可用的推送路径')
  assert.match(view.unavailableReason, /payment/, '理由里要写明是哪个筛选挡住了写回')
  assert.match(view.unavailableReason, /清除/, '理由必须给出下一步：清除筛选')
  assert.match(view.unavailableReason, /整集群/, '理由必须说明预测按整集群算这个成因')
})

/**
 * 两条路径的时间窗必须**逐字**取自这次预览响应，且两步必须是同一段。
 *
 * 这是本模块最硬的一条：服务端在推送前会拿请求里的时间窗**重新算一遍整份
 * 计划**再比指纹（writeback_handler.go 的 planWriteback）。两步带了不同的
 * 窗，轻则指纹永远对不上，重则某天"恰好"对上——那时推出去的是操作者没看过
 * 的那一份。
 */
test('出计划与推送的时间窗逐字取自预览响应，且两步同一段', () => {
  const pv = preview({ window: WINDOW })
  const view = writebackView(pv)

  assert.equal(view.available, true)
  for (const path of [view.planPath, view.pushPath]) {
    const q = params(path)
    assert.equal(q.get('from'), pv.window.from, 'from 必须是页面正在显示的那次预览的起点')
    assert.equal(q.get('to'), pv.window.to, 'to 必须是页面正在显示的那次预览的终点')
    assert.notEqual(q.get('from'), OTHER_WINDOW.from)
  }
  assert.ok(view.planPath.startsWith('/api/v1/clusters/prod-asia-1/policy-writeback/plan?'),
    `出计划路径必须指向本集群的写回计划端点，实际是 ${view.planPath}`)
  assert.ok(view.pushPath.startsWith('/api/v1/clusters/prod-asia-1/policy-writeback/push?'),
    `推送路径必须指向本集群的写回推送端点，实际是 ${view.pushPath}`)
})

/**
 * 时间窗换一段，两条路径都要跟着换。
 *
 * 上一条在常量窗口上也会通过（比如有人把 from 硬编码成 fixture 里那个值）。
 * 这条让"路径不随页面显示的窗口变化"这种写法无法蒙混过关。
 */
test('换一段时间窗，两条写回路径跟着换', () => {
  const a = writebackView(preview({ window: WINDOW }))
  const b = writebackView(preview({ window: OTHER_WINDOW }))

  assert.notEqual(a.planPath, b.planPath)
  assert.notEqual(a.pushPath, b.pushPath)
  assert.equal(params(b.pushPath).get('from'), OTHER_WINDOW.from)
  assert.equal(params(b.pushPath).get('to'), OTHER_WINDOW.to)
})

/**
 * 路径里不带 namespace 参数：服务端对非空 namespace 一律拒绝，这个端点
 * 唯一成立的取值是"没有筛选"，带一个注定被拒绝的参数等于留一条注定失败
 * 的调用路径。
 */
test('两条写回路径都不带 namespace 参数', () => {
  const view = writebackView(preview({}))
  assert.equal(params(view.planPath).has('namespace'), false)
  assert.equal(params(view.pushPath).has('namespace'), false)
})

/* ---------------------------------------------------------------------- */
/* 第二步的前提：没有计划就没有可推送的东西                                  */
/* ---------------------------------------------------------------------- */

/**
 * 没出计划时推送请求体恒为 null。
 *
 * 页面渲染推送按钮的条件就是这个函数返回了非 null（见页面接线那几条），
 * 因此这一条钉的是"未读计划即可推"这件事在界面上写不出来。服务端那道门
 * （writebackNoFingerprintMsg）才是权威的拒绝。
 */
test('没有计划时不产出推送请求体', () => {
  assert.equal(writebackPushBody(null), null)
})

/**
 * 计划没有指纹时同样不产出请求体。
 *
 * 指纹是服务端唯一放行的条件；不带指纹的推送只会被拒绝，发出去只是把一条
 * 注定失败的调用摆在操作者面前，而那条失败信息会盖住真正的原因。
 */
test('计划缺指纹或缺分支时不产出推送请求体', () => {
  assert.equal(writebackPushBody(plan({ fingerprint: '' })), null, '空指纹不得放行')
  assert.equal(writebackPushBody(plan({ fingerprint: '   ' })), null, '空白指纹不得放行')
  assert.equal(writebackPushBody(plan({ branch: '' })), null, '空分支不得放行')
})

/**
 * 有计划时，请求体里**只有**分支与指纹，且逐字来自计划。
 *
 * 多一个字段就是写回请求自述影响面的开始，而影响面必须由平台在写前重算
 * （design doc §4）。因此这里断言的是键集合本身，不只是那两个值。
 */
test('推送请求体只含分支与指纹，且逐字来自计划', () => {
  const p = plan()
  const body = writebackPushBody(p)

  assert.notEqual(body, null)
  assert.deepEqual(body, { branch: p.branch, fingerprint: p.fingerprint })
  assert.deepEqual(Object.keys(body ?? {}).sort(), ['branch', 'fingerprint'],
    '请求体里出现了第三个字段：写回请求不携带任何数字，计数由平台重算')
})

/* ---------------------------------------------------------------------- */
/* 计数漂移                                                                 */
/* ---------------------------------------------------------------------- */

/**
 * 四类计数一致时不报警，但四行仍要全给出来——操作者要能核对全部四个数，
 * 而不是只看到"没问题"。
 */
test('计数一致时不报警，四类仍逐行给出', () => {
  const drift = writebackCountDrift(PAGE_COUNTS, { ...PAGE_COUNTS })

  assert.equal(drift.drifted, false)
  assert.equal(drift.warning, '')
  assert.equal(drift.rows.length, 4)
  assert.deepEqual(drift.rows.map((r) => r.kind),
    ['WOULD_BREAK', 'WOULD_OPEN', 'UNCHANGED', 'UNKNOWN'])
  assert.equal(drift.rows.every((r) => !r.changed), true)
})

/**
 * 任何一类对不上都必须报警，且警告里要同时出现旧值与新值。
 *
 * 这是写回这一步的全部意义（design doc §4）：操作者确认规则的时刻与推送的
 * 时刻之间，集群或流量变了。只说"数字变了"而不给两端，等于要求他自己回忆
 * 刚才屏幕上那几个数——因此断言的是两个取值都出现在那句话里。
 */
test('WOULD_BREAK 变了必须报警，且警告里同时出现旧值与新值', () => {
  const drift = writebackCountDrift(PAGE_COUNTS, { ...PAGE_COUNTS, WOULD_BREAK: 96 })

  assert.equal(drift.drifted, true)
  assert.match(drift.warning, /WOULD_BREAK/)
  assert.match(drift.warning, /81/, '警告里必须有页面上原来那个数')
  assert.match(drift.warning, /96/, '警告里必须有计划重算出的那个数')
  const row = drift.rows.find((r) => r.kind === 'WOULD_BREAK')
  assert.equal(row?.changed, true)
  assert.equal(row?.pageText, '81')
  assert.equal(row?.planText, '96')
  assert.equal(drift.rows.find((r) => r.kind === 'UNKNOWN')?.changed, false,
    '没变的那几类不得被标成变了：假差异重复几次之后，真的那次也不会有人看')
})

/** WOULD_OPEN 同样要被盯住：敞口扩大与拦断增加同等重要，不能只盯一个方向。 */
test('WOULD_OPEN 变了同样报警', () => {
  const drift = writebackCountDrift(PAGE_COUNTS, { ...PAGE_COUNTS, WOULD_OPEN: 3 })

  assert.equal(drift.drifted, true)
  assert.match(drift.warning, /WOULD_OPEN/)
  assert.match(drift.warning, /3/)
})

/**
 * 计划里缺了某一类，不按 0 处理，而是当作不一致并显示"未给出"。
 *
 * 缺一类不是那一类为零，是没算过那一类（CLAUDE.md §3）。补成 0 会让一次
 * "没算出来"在屏幕上长得像一次"确认无影响"。
 */
test('计划缺一类计数时报警，且显示未给出而不是 0', () => {
  const partial = { WOULD_BREAK: 81, WOULD_OPEN: 0, UNCHANGED: 123 } as Record<ChangeKind, number>
  const drift = writebackCountDrift(PAGE_COUNTS, partial)

  assert.equal(drift.drifted, true)
  assert.equal(drift.rows.find((r) => r.kind === 'UNKNOWN')?.planText, '未给出')
  assert.match(drift.warning, /未给出/)
})

/* ---------------------------------------------------------------------- */
/* 页面接线                                                                 */
/* ---------------------------------------------------------------------- */

/*
 * 上面每一条测的都是纯逻辑层。它们全绿并不能证明 PolicyPage.tsx 还在用这
 * 一层。这个仓库的前端测试是 `node --test` 直接跑 TS 模块，没有 DOM、没有
 * React 测试渲染器，本任务也不得新增依赖 —— 因此能用来绑定调用点的手段
 * 只剩下读源码文本。**它挡得住"整段被删掉或改成另写一份"，挡不住"调用了
 * 但渲染在一个看不见的地方"、也挡不住样式上禁用而 onClick 仍然可点。**
 * 本轮上一任务已经证过这条局限：把导出控件包进一个恒假的条件里，四道门禁
 * 依旧全绿。这几条不是"写回入口已被测试覆盖"的证明。
 */
const PAGE_SOURCE = readFileSync(
  join(import.meta.dirname, '..', 'src', 'pages', 'PolicyPage.tsx'), 'utf8')

test('页面的写回入口仍然从 writebackView 取数', () => {
  assert.match(PAGE_SOURCE, /writebackView\(pv\)/,
    '页面不再调用 writebackView——纯逻辑层测得再全，可用性与两条路径已经另有出处了')
  assert.equal(/policy-writeback/.test(PAGE_SOURCE), false,
    '页面里出现了写回端点的字面量路径——路径只应由 writebackView 拼一次，'
    + '否则出计划与推送可以带上两段不同的时间窗')
})

test('两次请求都走 view 上的路径，页面不自己拼 URL', () => {
  assert.match(PAGE_SOURCE, /api\.policyWritebackPlan\(view\.planPath\)/,
    '出计划没有走 view.planPath')
  assert.match(PAGE_SOURCE, /api\.policyWritebackPush\(view\.pushPath, body\)/,
    '推送没有走 view.pushPath，或者请求体不是 writebackPushBody 的产出')
})

test('出计划的按钮禁用绑在 view.available 上，理由渲染出来', () => {
  assert.match(PAGE_SOURCE, /disabled=\{!view\.available \|\| busy\}/,
    '出计划按钮的禁用不再由 view.available 决定：命名空间筛选那条拒绝就退回成"点了才知道"')
  assert.match(PAGE_SOURCE, /\{view\.unavailableReason\}/,
    '不可用的原因没有渲染出来——一个没有理由的禁用按钮等于"它坏了"')
})

/**
 * 推送控件的存在条件必须是 pushBody（即"手上有一份带指纹的计划"）。
 *
 * 这条钉的是两件事：推送按钮不在计划之前出现，以及请求体是 writebackPushBody
 * 原样产出的那一个对象，页面不自己拼。
 */
test('推送按钮只在拿到计划之后才存在，且请求体原样来自 writebackPushBody', () => {
  assert.match(PAGE_SOURCE, /const pushBody = writebackPushBody\(plan\?\.plan \?\? null\)/,
    '推送请求体不再由 writebackPushBody 从计划产出：页面自己拼一个指纹上去，'
    + '"推的是操作者看过的那一份"这个保证就没了')
  assert.match(PAGE_SOURCE, /\{pushBody && \(/,
    '推送控件的渲染条件不再是 pushBody——未出计划就能推，两步就退化成一步')
  assert.match(PAGE_SOURCE, /onClick=\{\(\) => push\(pushBody\)\}/,
    '推送按钮点下去传的不是 pushBody')
})

/**
 * 计划的四项内容必须出现在推送按钮之前：文件、目标分支、提交信息、四类计数。
 *
 * 提交信息尤其不能省——它是合并请求上的评审人唯一会读的那句话，且会永久
 * 留在仓库历史里（design doc §7）。
 */
test('计划里的文件、分支、提交信息、多余文件都渲染出来', () => {
  for (const [needle, why] of [
    ['{plan.plan.branch}', '目标分支没有展示：操作者不知道这次会推到哪条分支'],
    ['{plan.plan.commitMessage}', '提交信息没有展示：评审人唯一会读的那句话没有经过操作者'],
    ['plan.plan.files.map', '将要新增/更新的文件清单没有逐条展示'],
    ['plan.plan.extraneous', '仓库里多余文件的清单没有展示（design doc §3）'],
    // 落点与"仓库上攒了几条分支"这两项都是本轮补上的：前者进指纹，因此
    // 必须被操作者读到；后者是唯一能看见人工合并那道门有没有人走的信号。
    ['{plan.plan.repoId}', '目标仓库没有展示：它进了指纹，操作者却没读到自己批准的落点'],
    ['plan.plan.existingBranches', '仓库上已存在的 distill 分支没有展示（design doc §2）'],
    ['不判断它们是否已被合并',
      '分支清单旁边没有写明合并状态平台没有判断：那会让"存在"被读成"未合并"，'
      + '而后者平台从没算过'],
  ] as const) {
    assert.equal(PAGE_SOURCE.includes(needle), true, why)
  }
})

/**
 * 计数不一致时那条提示必须真的被渲染。
 *
 * 纯逻辑层算出 warning 而页面不显示它，是本项目反复出现的那个形状：判定
 * 对了，调用点没了。
 */
test('计数不一致时渲染 drift.warning', () => {
  assert.match(PAGE_SOURCE, /writebackCountDrift\(pageCounts, plan\.plan\.counts\)/,
    '页面不再比对页面上的计数与计划重算出的那一套')
  assert.match(PAGE_SOURCE, /drift\.drifted && \(/,
    '不一致时的提示没有条件渲染点了')
  assert.match(PAGE_SOURCE, /\{drift\.warning\}/,
    '不一致的提示文字没有渲染出来——新数字悄悄出现在旧数字的位置上，'
    + '正是 design doc §4 要防的那件事')
})

/**
 * 比对的页面侧必须是 overridden 那一套：服务端算计划用的正是它。拿默认
 * 推荐去比，会在"人工决定改变了预测"时报出与集群无关的假差异。
 */
test('页面侧的计数取 overridden 那一套', () => {
  assert.match(PAGE_SOURCE, /pageCounts=\{pv\.overridden\.prediction\.counts\}/,
    '比对用的页面侧计数不是 overridden：假差异会把真差异淹掉')
})

test('写回入口在 dry-run 一屏内、紧挨导出控件', () => {
  const section = PAGE_SOURCE.indexOf('title="dry-run 影响"')
  const exportControl = PAGE_SOURCE.indexOf('<ExportControl')
  const writeback = PAGE_SOURCE.indexOf('<WritebackControl')
  const detail = PAGE_SOURCE.indexOf('<DryRunDetail')

  assert.ok(section >= 0 && exportControl >= 0 && writeback >= 0 && detail >= 0, '四个锚点都要在')
  assert.ok(section < exportControl && exportControl < writeback && writeback < detail,
    '写回入口不在 dry-run 那一屏、导出控件与明细之间：推进仓库的内容与刚下载的'
    + '那份文件是同一份，两个出口必须在视觉上挨着')
})

/**
 * 页面不展示文件内容，也不重建 YAML。
 *
 * 推进仓库的必须逐字节是服务端渲染的那一份；界面上多渲染一份内容既没有
 * 用处（要看内容走导出），又给了"前端改一改再推"这条路径一个起点。
 */
test('页面不渲染计划里的文件内容，也不重建 YAML', () => {
  assert.equal(PAGE_SOURCE.includes('f.content'), false,
    '页面渲染了计划里的文件内容：要看内容走导出，这里只需要路径（规范 §20）')
  for (const forbidden of ['apiVersion', 'networking.k8s.io', 'NetworkPolicySpec']) {
    assert.equal(PAGE_SOURCE.includes(forbidden), false,
      `页面里出现了 ${forbidden}：写回的内容必须逐字节是服务端产出的那一份`)
  }
})
