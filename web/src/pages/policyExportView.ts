import type { CandidatePolicy, PolicyPreview, TimeWindow } from '../api/types'

/**
 * 导出入口的全部取数：能不能导、不能导时的理由、要请求哪个路径，
 * 以及按钮旁边那句说明这份文件对应哪一段判定的话。
 *
 * 抽成一个纯函数、一个返回值，与 dryRunView 同源：这里错一次的后果
 * 不是排版难看，而是导出的路径带着一个与页面正在显示的那次预测不同的
 * 时间窗——文件里的策略与操作者刚读完的「会拦断 81 条」描述两次不同的
 * 计算，而屏幕上没有任何迹象（design doc 2026-08-14 §2）。轮 3 那条
 * Critical（磁贴一套报告、下方表格另一套，四道门禁全绿）就是这个形状。
 *
 * 因此路径不由页面拼，也不由 api 层自己再选一次时间窗：它只能由这个
 * 函数从**页面正在渲染的那一个 pv 对象**里取。
 *
 * **界面上的不可用不是权限，也不是防护。** 服务端对非空 namespace、
 * 对零条启用规则、对非管理员一律拒绝，那三道判定是权威的（规范 §34）。
 * 这里提前把原因说出来，是因为让人点一下才知道"不行"教不会他下一步
 * 该做什么。
 */
export interface PolicyExportView {
  /** 可导出。false 时按钮禁用，unavailableReason 必然非空。 */
  readonly available: boolean
  /** 不可用的原因，含下一步怎么做；可用时为空串。 */
  readonly unavailableReason: string
  /**
   * 导出请求路径，恒由 pv.cluster 与 pv.window 拼出。
   *
   * 不可用时为空串——不给一个"禁用了但仍然可用"的路径留在对象里。
   */
  readonly path: string
  /** 按钮旁边那句话：这份文件对应哪一段时间窗，改了就要重新导出。 */
  readonly windowNote: string
}

/**
 * 导出端点的请求路径。
 *
 * 与判定同处一个文件、且只由 policyExportView 调用：路径唯一会出错的
 * 方式，是拿一个与页面正在显示的那次预览不同的时间窗去拼它。放在 api
 * 层由调用方各自传参，就又多了一个可以与页面分歧的取数点，而分歧的
 * 后果是文件与操作者刚读过的四个数字描述两次不同的计算，屏幕上却没有
 * 任何迹象（design doc §2）。这里也是它能被 tests/ 直接钉住的地方——
 * api/client.ts 在测试工程里编不过（那边没有 DOM lib）。
 *
 * 不带 namespace 参数：服务端对非空 namespace 一律拒绝（§4），这个端点
 * 唯一成立的取值就是"没有筛选"。做成可传的参数等于留一条注定失败的
 * 调用路径。
 */
/**
 * 窗口是不是"根本没有"。
 *
 * 后端在这个集群一次流量都没摄入时回一个零值窗口（TimeWindow 的零值），
 * 那不是一段时间，是"我们答不出这一屏覆盖哪一段时间"。把它当成时间用会
 * 有两个后果：界面显示 0001 年，以及**回传给服务端时被判成非法参数** ——
 * 一句关于集群的话于是变成一次参数错误。
 */
export function windowAbsent(window: TimeWindow): boolean {
  return !window || !window.from || !window.to
    || window.from === window.to
    || new Date(window.from).getUTCFullYear() <= 1
}

function policyExportPath(cluster: string, window: TimeWindow): string {
  // 窗口缺席就整个省掉 from/to，让服务端走它自己的默认那一支 —— 它知道
  // 这个集群没有流量摄入，也知道该怎么办（资产兜底）。我们不替它造一个
  // 非法值再让它拒绝。
  if (windowAbsent(window)) {
    return `/api/v1/clusters/${encodeURIComponent(cluster)}/policy-export`
  }
  const p = new URLSearchParams({ from: window.from, to: window.to })
  return `/api/v1/clusters/${encodeURIComponent(cluster)}/policy-export?${p}`
}

/**
 * 一份候选集里启用规则的条数。
 *
 * 数规则而不是数 workload：每个候选 workload 恒有一份文档（规则为空即
 * default-deny），文档数因此永远不为零，拿它判空那条判空永远不成立
 * （与服务端 countEnabledRules 同一条理由）。
 */
function enabledRuleCount(candidates: CandidatePolicy[]): number {
  let n = 0
  for (const c of candidates) {
    for (const r of c.rules) if (r.enabled) n++
  }
  return n
}

/**
 * 取数入口。入参是页面正在渲染的那一次预览，不是它的几个字段——
 * 拆成几个参数就又给了调用方一次"从别处取时间窗"的机会。
 */
export function policyExportView(pv: PolicyPreview): PolicyExportView {
  const windowNote = `这份文件对应当前时间窗 ${formatWindow(pv)} 下的结论；`
    + '换了时间窗、或此后又确认/撤销过规则，都要重新导出——文件里的注释头写着的是导出那一刻的四类计数。'

  // 命名空间筛选优先于空判定，与服务端同序：筛选是入参范围本身不成立，
  // 与集群里此刻有几条规则无关（export_handler.go 在读取之前就拒绝）。
  if (pv.namespace !== '') {
    return {
      available: false,
      unavailableReason:
        `当前按命名空间「${pv.namespace}」筛选，导出不可用。dry-run 预测恒按整集群计算，`
        + '一份被裁剪的文件会带着一个把文件里根本没有的规则也算进去的结论，'
        + '且朝着让人放心的方向偏。清除命名空间筛选后即可导出整集群的策略集。',
      path: '',
      windowNote,
    }
  }

  // 取应用人工决定之后的那一套候选集：服务端导出的正是它渲染出的文档
  // （store.OverriddenView.Enabled 由 Overridden.Candidates 渲染）。
  // 拿默认推荐那一套来判空，会在"默认有规则、但全被人工禁用了"时把按钮
  // 留着可用，点下去才撞上服务端的拒绝。
  if (enabledRuleCount(pv.overridden.candidates) === 0) {
    return {
      available: false,
      unavailableReason:
        '当前时间窗下没有任何启用中的规则，无可导出内容。一个空的策略文件应用下去是'
        + '「什么都不允许」还是「什么都没做」，取决于集群里已有什么——这个歧义不该由一次导出制造，'
        + '因此不生成空文件。先到「待确认规则」一节确认至少一条规则。',
      path: '',
      windowNote,
    }
  }

  return {
    available: true,
    unavailableReason: '',
    path: policyExportPath(pv.cluster, pv.window),
    windowNote,
  }
}

/** 时间窗的显示形状，与本页 formatTime 一致（UTC，不带毫秒）。 */
function formatWindow(pv: PolicyPreview): string {
  if (windowAbsent(pv.window)) {
    return '没有 —— 这个集群还没有任何流量观测'
  }
  return `${formatInstant(pv.window.from)} ~ ${formatInstant(pv.window.to)}`
}

function formatInstant(iso: string): string {
  return new Date(iso).toISOString().replace('T', ' ').replace(/\.\d+Z$/, ' UTC')
}
