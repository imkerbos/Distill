import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { join } from 'node:path'

import {
  baselineGapViews, dataSourceView, listTruncationView, notAssessedNote, wouldBreakQualifier,
} from '../src/pages/preconditionsView.ts'
import type { Completeness, DataSource, Kind } from '../src/api/types.ts'

const src = (...parts: string[]) =>
  readFileSync(join(import.meta.dirname, '..', 'src', ...parts), 'utf8')

/* ---------------------------------------------------------------------- */
/* §2 数据来源                                                              */
/* ---------------------------------------------------------------------- */

test('两个登记来源各自说出自己是什么', () => {
  const fixture = dataSourceView('FIXTURE')
  assert.equal(fixture.code, 'FIXTURE')
  assert.match(fixture.label, /FIXTURE/)
  assert.match(fixture.detail, /合成数据集/,
    '演示数据没有说明它不描述任何真实集群——那正是这块标识存在的理由')

  const collected = dataSourceView('COLLECTED')
  assert.equal(collected.code, 'COLLECTED')
  assert.match(collected.label, /COLLECTED/)
})

/**
 * 缺字段不得默认成真实。
 *
 * 老响应、集群未注册、后端把字段改了名，在前端看起来是同一件事：我们不
 * 知道屏幕上这些数字描述的是哪种集群。把它显示成「已采集」，是把一件我们
 * 不知道的事说成最危险的那个方向（design doc 2026-08-17 §2）。
 */
test('没有 dataSource 时是「来源未知」，绝不是 COLLECTED', () => {
  const view = dataSourceView(undefined)
  assert.equal(view.code, 'UNKNOWN',
    '缺字段落到了 UNKNOWN 之外——缺席被解释成了一个具体的来源')
  assert.equal(/COLLECTED|已采集|真实采集/.test(view.label), false,
    '「来源未知」的标签里出现了采集字样：读的人会把一份来路不明的报告当成生产报告')
  assert.match(view.detail, /未知不等于真实/)
})

test('未登记的取值同样落到「来源未知」', () => {
  // 后端某天多一个取值、或字段被改成空串时的实际形状。
  for (const raw of ['', 'collected', 'SYNTHETIC']) {
    assert.equal(dataSourceView(raw as DataSource).code, 'UNKNOWN',
      `未登记的取值 ${JSON.stringify(raw)} 没有落到 UNKNOWN——一个界面读不懂的来源必须显示成读不懂`)
  }
})

test('演示数据与来源未知必须显著，真实采集不抢镜', () => {
  assert.equal(dataSourceView('FIXTURE').emphasized, true)
  assert.equal(dataSourceView(undefined).emphasized, true)
  assert.equal(dataSourceView('COLLECTED').emphasized, false)
})

/* ---------------------------------------------------------------------- */
/* §3 清单截断                                                              */
/* ---------------------------------------------------------------------- */

test('被截断的清单说清共几条、此处几条、上限是多少', () => {
  const v = listTruncationView('RISKY_FLOWS', 1000, { total: 4213, limit: 1000 })
  assert.equal(v.truncated, true)
  assert.match(v.countText, /4213/)
  assert.match(v.countText, /1000/)
  assert.notEqual(v.remedy, '', '截断了却不说下一步怎么办')
})

test('缩小时间窗只对两个流量类清单有用，对 nakedPods 明说无用', () => {
  const risky = listTruncationView('RISKY_FLOWS', 1000, { total: 4213, limit: 1000 })
  const egress = listTruncationView('EGRESS_TARGETS', 1000, { total: 1200, limit: 1000 })
  const naked = listTruncationView('NAKED_PODS', 1000, { total: 2400, limit: 1000 })

  assert.match(risky.remedy, /缩小时间窗可以/)
  assert.match(egress.remedy, /缩小时间窗可以/)
  assert.match(naked.remedy, /缩小时间窗对这份清单没有用/,
    'nakedPods 的处置说成了缩窗：它来自锚点快照，缩窗对它毫无作用，'
    + '照做的人会以为自己已经把清单看全了')
  assert.match(naked.remedy, /锚点/)
})

test('没被截断时就说共几条，不说任何截断的话', () => {
  const v = listTruncationView('NAKED_PODS', 6, { total: 6, limit: 1000 })
  assert.equal(v.truncated, false)
  assert.equal(v.countText, '共 6 个')
  assert.equal(v.remedy, '')
})

/**
 * 「空」与「缺失」必须可区分，这是三个回显字段存在的另一半理由。
 *
 * 一份没有回显的清单不知道有没有被截过，而它读起来就是"这个集群只有
 * 这些"——那是本平台唯一不能出的那种错。
 */
test('服务端没回显条数时，界面不得声称这就是全部', () => {
  for (const echo of [undefined, { total: 0, limit: 0 }]) {
    const v = listTruncationView('EGRESS_TARGETS', 12, echo)
    assert.equal(v.truncated, false)
    assert.match(v.countText, /未回显总数|无法判断/,
      '没有回显的清单被渲染成了一个确定的总数')
    assert.equal(/^共 /.test(v.countText), false,
      '"共 N 条"是一个关于集群的结论，一份没有回显的清单支撑不了它')
  }
})

/* ---------------------------------------------------------------------- */
/* §4 未评估的 Baseline                                                     */
/* ---------------------------------------------------------------------- */

const ALL_KINDS: Kind[] = ['DNS', 'LB_HEALTH_CHECK', 'METRICS_SCRAPE', 'CONTROL_PLANE', 'NODE_AGENT']

test('未评估是叠加：那几类仍然留在缺失清单里', () => {
  const gaps = baselineGapViews(ALL_KINDS, ['METRICS_SCRAPE', 'NODE_AGENT'])
  assert.deepEqual(gaps.map((g) => g.kind), ALL_KINDS,
    '未评估的类型被从缺失清单里摘掉了——一个「我们没看过」于是读起来像「这里没问题」')
  assert.deepEqual(
    gaps.filter((g) => g.notAssessed).map((g) => g.kind),
    ['METRICS_SCRAPE', 'NODE_AGENT'])
})

test('两种缺口的处置不同：一个去写策略，一个去修采集', () => {
  const gaps = baselineGapViews(['DNS', 'NODE_AGENT'], ['NODE_AGENT'])
  const dns = gaps[0]
  const nodeAgent = gaps[1]

  assert.match(dns.remedy, /写一条覆盖它的策略/)
  assert.equal(/采集/.test(dns.remedy), false,
    '真缺失的处置里混进了采集：那会把一条该写的策略变成一次查采集器')
  assert.match(nodeAgent.remedy, /修采集/,
    '未评估的处置没有指向采集——不标注就等于塌回普通缺失，'
    + '运维会去写一条 DNS 策略，而真正该做的是改 RBAC')
  assert.match(nodeAgent.remedy, /不是去写策略/)
  assert.notEqual(dns.remedy, nodeAgent.remedy)
})

test('没有未评估类型时不标注任何东西', () => {
  for (const na of [null, undefined, [] as Kind[]]) {
    assert.equal(baselineGapViews(ALL_KINDS, na).some((g) => g.notAssessed), false)
    assert.equal(notAssessedNote(na), '')
  }
})

test('未评估的总说明点名类型，并写明它不放宽 Enforcing', () => {
  const note = notAssessedNote(['METRICS_SCRAPE', 'NODE_AGENT'])
  assert.match(note, /METRICS_SCRAPE/)
  assert.match(note, /NODE_AGENT/)
  assert.match(note, /Enforcing/,
    '标注没有说清它不放宽任何门禁——一条读起来像豁免的注释比不标注更糟')
  assert.match(note, /修采集/)
})

/* ---------------------------------------------------------------------- */
/* §5 残缺窗口的 WOULD_BREAK                                                */
/* ---------------------------------------------------------------------- */

test('窗口完整时不加限定语', () => {
  assert.equal(wouldBreakQualifier('COMPLETE'), '')
})

test('窗口不完整时说明这个数不是上线影响数', () => {
  for (const c of ['DEGRADED', 'UNKNOWN'] as Completeness[]) {
    const q = wouldBreakQualifier(c)
    assert.notEqual(q, '', `${c} 窗口下 WOULD_BREAK 没有任何限定语`)
    assert.match(q, /WOULD_BREAK/)
    assert.match(q, /Baseline 拦下/,
      '限定语没有说出这个数究竟量的是什么——「候选集为空时有多少连接被 Baseline 拦下」')
    assert.match(q, /不是|不能/)
  }
})

test('完整度缺席或读不懂时同样加限定语', () => {
  assert.notEqual(wouldBreakQualifier(undefined), '',
    '缺字段被当成了 COMPLETE：一个我们读不懂的完整度不能当成"完整"')
  assert.notEqual(wouldBreakQualifier('PARTIAL' as Completeness), '')
})

/* ---------------------------------------------------------------------- */
/* 页面接线                                                                 */
/* ---------------------------------------------------------------------- */

/*
 * 上面每一条测的都是纯逻辑层，下面这些读的是源码文本。**两层加起来仍然
 * 证明不了任何东西出现在屏幕上**：这个仓库的前端没有 DOM、没有 React 测试
 * 渲染器，本轮也不得新增依赖（design doc 2026-08-17 §7）。这一点是实测过
 * 的，不是推测——把导出控件包进 `{false && …}`，全部前端测试仍然全绿。
 *
 * 因此这些断言挡得住"整段被删掉或改成另写一份"，挡不住"调用了但渲染在
 * 一个看不见的地方"。四条区别是否真的看得见，只有浏览器实测能回答。
 */

const PAGES: [string, string][] = [
  ['TopologyPage.tsx', '/topology'],
  ['FlowsPage.tsx', '/flows'],
  ['QualityPage.tsx', '/quality'],
  ['SecurityPage.tsx', '/security'],
  ['PolicyPage.tsx', '/policy'],
]

const SHELL_SOURCE = src('components', 'AppShell.tsx')
const NOTICE_SOURCE = src('components', 'DataSourceNotice.tsx')
const SECURITY_SOURCE = src('pages', 'SecurityPage.tsx')
const POLICY_SOURCE = src('pages', 'PolicyPage.tsx')

test('五屏的内容区各自渲染数据来源，不只靠侧边栏', () => {
  for (const [file, route] of PAGES) {
    const source = src('pages', file)
    assert.match(source, /<DataSourceNotice \/>/,
      `${route} 的内容区没有来源标识：截图、导出与单屏转发会把侧边栏留下，`
      + '而"有人把一份演示报告当成生产报告"正是发生在内容离开浏览器之后')
    assert.match(source, /import DataSourceNotice from '\.\.\/components\/DataSourceNotice'/,
      `${file} 没有引入 DataSourceNotice`)
  }
})

test('来源判定只有一处，页面不自己解释 dataSource', () => {
  for (const [file] of PAGES) {
    assert.equal(/dataSource/.test(src('pages', file)), false,
      `${file} 自己读了 dataSource：五处各判一次必然漂，而漂的方向恰恰是"缺字段时当成真实"`)
  }
  assert.match(NOTICE_SOURCE, /dataSourceView\(useContext\(/,
    '标识组件不再走 dataSourceView——缺字段该显示什么于是另有出处了')
})

test('来源沿既有的集群列表请求下发，本轮不新增请求', () => {
  assert.match(SHELL_SOURCE, /<ClusterDataSourceProvider value=\{clusters\.find\(\(c\) => c\.id === cluster\)\?\.dataSource\}>/,
    'AppShell 不再把选中集群的来源下发给内容区')
  for (const [file] of PAGES) {
    if (file === 'PolicyPage.tsx') continue // 本页原本就要读集群的接入状态。
    assert.equal(/api\.clusters\(/.test(src('pages', file)), false,
      `${file} 为了显示来源自己拉了一次集群列表：本轮不新增任何请求（design doc §9）`)
  }
})

test('安全发现三个清单各自回显自己的截断，且各用各的处置', () => {
  for (const [list, echo] of [
    ['RISKY_FLOWS', 'riskyFlowsTruncation'],
    ['EGRESS_TARGETS', 'egressTargetsTruncation'],
    ['NAKED_PODS', 'nakedPodsTruncation'],
  ]) {
    assert.match(SECURITY_SOURCE, new RegExp(`listTruncationView\\('${list}',[^)]*${echo}`),
      `${list} 没有接到 ${echo} 上：三个清单合用一份回显，就说不出被截的是哪一份、该怎么办`)
  }
  assert.match(SECURITY_SOURCE, /\{view\.countText\}/, '条数没有渲染出来')
  assert.match(SECURITY_SOURCE, /\{view\.remedy\}/, '截断了却不渲染处置指引')
  assert.equal(/rep\.riskyFlows\.length\} 条/.test(SECURITY_SOURCE), false,
    '区块的 meta 又退回成了数组长度——一份被截到 1000 条的清单于是读起来就是"这个集群只有这些"')
})

test('WOULD_BREAK 的限定语来自 windowCompleteness，不来自反推', () => {
  assert.match(POLICY_SOURCE, /wouldBreakQualifier\(pv\.windowCompleteness\)/,
    '页面不再读 windowCompleteness：限定语于是又要靠猜')
  // 断的是那个**推断**本身，不是 degradedCount 这个词：降级条数作为一个
  // 独立指标展示是正当的（DryRunDetail 的可信度降级磁贴），拿它去反推窗口
  // 完整度才是这一轮要消除的东西（design doc §5）。
  const INFERENCE = /degradedCount\s*===\s*\w*\.?totalEvaluated|totalEvaluated\s*===\s*\w*\.?degradedCount/
  for (const [file, source] of [
    ['PolicyPage.tsx', POLICY_SOURCE],
    ['DryRunDetail.tsx', src('pages', 'DryRunDetail.tsx')],
    ['dryRunView.ts', src('pages', 'dryRunView.ts')],
  ] as [string, string][]) {
    assert.equal(INFERENCE.test(source), false,
      `${file} 拿 degradedCount === totalEvaluated 反推窗口是否残缺：那是一个推断，`
      + '而这一轮的全部意义就是把推断换成事实（design doc §5）')
  }
  const qualifier = POLICY_SOURCE.indexOf('{breakQualifier !== \'\' && <Notice>')
  const firstTile = POLICY_SOURCE.indexOf('<DryRunMetric')
  assert.ok(qualifier >= 0 && firstTile >= 0, '两个锚点都要在')
  assert.ok(qualifier < firstTile,
    '限定语排在四个 tile 之后：读完那个大字才读到该怎么读它，等于没有限定语')
})

test('未评估叠加在缺失清单上，不另开一栏', () => {
  assert.match(POLICY_SOURCE, /notAssessed=\{pv\.notAssessedBaselines\}/,
    '缺失 Baseline 一节拿不到 notAssessedBaselines——未评估于是塌回普通缺失')
  assert.match(POLICY_SOURCE, /baselineGapViews\(m\.kinds, notAssessed\)/,
    '标注不再由 baselineGapViews 产出：哪几类是"没看过"于是另有一份判断')
  assert.match(POLICY_SOURCE, /notAssessedNote\(notAssessed\)/)
  assert.equal(/title="未评估/.test(POLICY_SOURCE), false,
    '未评估被单列成了另一节：那几类必须留在缺失清单里，只读缺失一栏的人才是 fail-closed 的')
})
