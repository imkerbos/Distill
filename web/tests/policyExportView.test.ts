import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { join } from 'node:path'

import { policyExportView } from '../src/pages/policyExportView.ts'
import type { CandidatePolicy, CandidateRule, PolicyPreview, TimeWindow } from '../src/api/types.ts'

const WINDOW: TimeWindow = { from: '2026-08-01T00:00:00Z', to: '2026-08-08T00:00:00Z' }
/** 另一段时间窗，只用来做反证：它绝不该出现在任何一条导出路径里。 */
const OTHER_WINDOW: TimeWindow = { from: '2026-01-01T00:00:00Z', to: '2026-01-02T00:00:00Z' }

function rules(...enabled: boolean[]): CandidateRule[] {
  return enabled.map((e, i) => ({ fingerprint: `fp-${i}`, enabled: e } as CandidateRule))
}

function policy(namespace: string, workload: string, rs: CandidateRule[]): CandidatePolicy {
  return { cluster: 'prod-asia-1', namespace, workload, rules: rs }
}

function preview(opts: {
  namespace?: string
  window?: TimeWindow
  candidates?: CandidatePolicy[]
  overridden?: CandidatePolicy[]
}): PolicyPreview {
  const candidates = opts.candidates ?? [policy('payment', 'api', rules(true))]
  return {
    cluster: 'prod-asia-1',
    namespace: opts.namespace ?? '',
    window: opts.window ?? WINDOW,
    candidates,
    overridden: { candidates: opts.overridden ?? candidates },
  } as PolicyPreview
}

function params(path: string): URLSearchParams {
  return new URL(path, 'http://localhost:4000').searchParams
}

/**
 * 命名空间筛选生效时，入口必须在点击**之前**就不可用，并说出原因。
 *
 * 服务端拒绝（namespacedExportMsg）是权威的，这条断言不是它的替代。
 * 但一个点下去才失败的按钮教不会操作者任何东西：他下一步该做的是清掉
 * 筛选，而"请求参数不合法"会把他引向检查命名空间的拼写。因此理由里
 * 必须出现处置方式，这条断言钉的就是这一点。
 */
test('命名空间筛选生效时不可用，且理由指出清除筛选', () => {
  const view = policyExportView(preview({ namespace: 'payment' }))

  assert.equal(view.available, false, '筛选生效时按钮必须禁用')
  assert.equal(view.path, '', '不可用时不留下一条仍然可用的请求路径')
  assert.match(view.unavailableReason, /payment/, '理由里要写明是哪个筛选挡住了导出')
  assert.match(view.unavailableReason, /清除/, '理由必须给出下一步：清除筛选')
})

/**
 * 一条启用规则都没有时不可用。
 *
 * 空文件不是"导出了个小文件"：一份空的 policy 应用下去是「什么都不允许」
 * 还是「什么都没做」，取决于集群里已有什么（design doc §4）。这个歧义
 * 不该由一个下载按钮制造出来，理由里因此必须把它说清楚。
 */
test('没有启用规则时不可用，理由说明空文件的歧义', () => {
  const view = policyExportView(preview({
    candidates: [policy('payment', 'api', rules(false, false))],
  }))

  assert.equal(view.available, false)
  assert.equal(view.path, '')
  assert.match(view.unavailableReason, /空的策略文件|什么都不允许/)
})

/**
 * 判空必须数**应用人工决定之后**的那一套候选集。
 *
 * 服务端导出的正是它（OverriddenView.Enabled 由 Overridden.Candidates
 * 渲染）。拿默认推荐那一套判空，会在"默认有规则、但全被人工禁用"时把
 * 按钮留着可用——操作者点下去撞上服务端的拒绝，而页面刚刚才向他保证
 * 这次导出是可以的。这是轮 3「两个数据源各自都对、只是不是同一套」的
 * 同一形状。
 */
test('判空数的是 overridden 那一套候选集，不是默认推荐', () => {
  const view = policyExportView(preview({
    candidates: [policy('payment', 'api', rules(true, true))],
    overridden: [policy('payment', 'api', rules(false, false))],
  }))

  assert.equal(view.available, false,
    '默认推荐有两条启用规则，但人工全禁用后一条都不剩，导出必须不可用')
})

/**
 * 可用时，导出路径的时间窗必须**逐字**取自这次预览响应里的那一段。
 *
 * 这是本模块唯一真正的风险（design doc §2）：路径带着另一段时间窗时，
 * 文件里的策略与操作者刚读完的四类计数描述两次不同的计算，而屏幕上
 * 没有任何迹象。因此这里断言的是取值同一，而不是"有 from 和 to 两个参数"。
 */
test('导出路径的时间窗逐字取自预览响应', () => {
  const pv = preview({ window: WINDOW })
  const view = policyExportView(pv)

  assert.equal(view.available, true)
  const q = params(view.path)
  assert.equal(q.get('from'), pv.window.from, 'from 必须是页面正在显示的那次预览的起点')
  assert.equal(q.get('to'), pv.window.to, 'to 必须是页面正在显示的那次预览的终点')
  assert.notEqual(q.get('from'), OTHER_WINDOW.from)
  assert.ok(view.path.startsWith('/api/v1/clusters/prod-asia-1/policy-export?'),
    `路径必须指向本集群的导出端点，实际是 ${view.path}`)
})

/**
 * 时间窗换一段，路径必须跟着换。
 *
 * 上一条在常量窗口上也会通过（比如有人把 from 硬编码成 fixture 里那个
 * 值）。这条让"路径不随页面显示的窗口变化"这种写法无法蒙混过关。
 */
test('换一段时间窗，导出路径跟着换', () => {
  const a = policyExportView(preview({ window: WINDOW }))
  const b = policyExportView(preview({ window: OTHER_WINDOW }))

  assert.notEqual(a.path, b.path)
  assert.equal(params(b.path).get('from'), OTHER_WINDOW.from)
  assert.equal(params(b.path).get('to'), OTHER_WINDOW.to)
})

/**
 * 路径里不带 namespace 参数：这个端点唯一成立的取值是"没有筛选"，
 * 带一个注定被拒绝的参数等于留一条注定失败的调用路径。
 */
test('导出路径不带 namespace 参数', () => {
  const view = policyExportView(preview({}))
  assert.equal(params(view.path).has('namespace'), false)
})

/**
 * 两条都不成立时，报的是命名空间那条——与服务端同序（它在读取之前就
 * 拒绝筛选，此时"有几条启用规则"根本还没算）。报错顺序不一致时，
 * 操作者会先去确认几条规则，确认完发现还是不行。
 */
test('筛选与空同时成立时，报的是筛选那条', () => {
  const view = policyExportView(preview({
    namespace: 'payment',
    candidates: [policy('payment', 'api', rules(false))],
  }))

  assert.match(view.unavailableReason, /命名空间/)
  assert.doesNotMatch(view.unavailableReason, /没有任何启用中的规则/)
})

/**
 * 按钮旁边那句话必须写出这份文件对应的那一段时间窗，并说明改了要重导。
 * 它是一个关于文件口径的断言，不是装饰：文件脱离平台之后，读它的人
 * 唯一能对上的就是这两个时刻。
 */
test('说明文字带着当前时间窗，并说明改了要重新导出', () => {
  const view = policyExportView(preview({ window: WINDOW }))

  assert.match(view.windowNote, /2026-08-01/)
  assert.match(view.windowNote, /2026-08-08/)
  assert.match(view.windowNote, /重新导出/)
})

/* ---------------------------------------------------------------------- */
/* 页面接线                                                                 */
/* ---------------------------------------------------------------------- */

/*
 * 上面每一条测的都是纯逻辑层。它们全绿并不能证明 PolicyPage.tsx 还在用
 * 这一层——本项目上一次前端事故正是这个形状：dry-run 磁贴渲染一份报告、
 * 下面的表格渲染另一份，tsc / oxlint / vite build 三关全过，因为没有
 * 任何一个编译期检查能绑定"某个组件读的是哪个字段"。
 *
 * 这个仓库的前端测试是 `node --test` 直接跑 TS 模块，没有 DOM、没有
 * React 测试渲染器，本任务也不得新增依赖 —— 因此能用来绑定调用点的手段
 * 只剩下读源码文本。**它挡得住"整段被删掉或改成另写一份"，挡不住"调用
 * 了但渲染在一个看不见的地方"、也挡不住样式上禁用而 onClick 仍然可点。**
 * 这条局限写在这里，不要把它当成"导出入口已被测试覆盖"。
 */
const PAGE_SOURCE = readFileSync(
  join(import.meta.dirname, '..', 'src', 'pages', 'PolicyPage.tsx'), 'utf8')

test('页面的导出入口仍然从 policyExportView 取数', () => {
  assert.match(PAGE_SOURCE, /policyExportView\(pv\)/,
    '页面不再调用 policyExportView——纯逻辑层测得再全，可用性与路径已经另有出处了')
})

test('导出请求走的是 view.path，页面不自己拼 URL', () => {
  assert.match(PAGE_SOURCE, /api\.policyExport\(view\.path\)/,
    '页面自己拼了导出路径：那就是第二个可以与页面显示的时间窗分歧的取数点（design doc §2）')
  assert.equal(/policy-export/.test(PAGE_SOURCE), false,
    '页面里出现了导出端点的字面量路径——路径只应由 policyExportView 拼一次')
})

test('按钮的禁用绑在 view.available 上', () => {
  assert.match(PAGE_SOURCE, /disabled=\{!view\.available \|\| busy\}/,
    '按钮的禁用不再由 view.available 决定：命名空间筛选与零启用规则两种拒绝'
    + '就退回成"点了才知道"')
  assert.match(PAGE_SOURCE, /\{view\.unavailableReason\}/,
    '不可用的原因没有渲染出来——一个没有理由的禁用按钮等于"它坏了"')
})

test('导出入口在 dry-run 影响一屏内，不在页面底部', () => {
  const section = PAGE_SOURCE.indexOf('title="dry-run 影响"')
  const control = PAGE_SOURCE.indexOf('<ExportControl')
  const detail = PAGE_SOURCE.indexOf('<DryRunDetail')

  assert.ok(section >= 0 && control >= 0 && detail >= 0, '三个锚点都要在')
  assert.ok(section < control && control < detail,
    '导出入口不在 dry-run 那一屏的四个 tile 与明细之间：操作者刚读完"会拦断多少条"，'
    + '文件与它对应的结论要在视觉上绑在一起（design doc §6）')
})

test('页面不重建 YAML 或注释头', () => {
  // 只列 YAML 文档本身的标记：注释头里的字眼（"导出者""时间窗"）在页面
  // 的说明文字与注释里正当地出现，拿它们做断言会把一条真断言变成措辞禁令。
  for (const forbidden of ['apiVersion', 'networking.k8s.io', 'NetworkPolicySpec', 'yaml.Marshal']) {
    assert.equal(PAGE_SOURCE.includes(forbidden), false,
      `页面里出现了 ${forbidden}：文件必须逐字节是服务端产出的那一份，`
      + '前端重建一份，"它与页面上的预测同源"这个保证当场就没了（design doc §2、§3）')
  }
})
