import type { Completeness, DataSource, Kind, ListTruncation } from '../api/types'

/*
 * 接入前置的四道栓在界面上的取数层（design doc 2026-08-17 §2—§5）。
 *
 * 后端刚刚把四组区别分了出来：演示数据 / 真实采集、截断过的清单 / 完整的
 * 清单、「没看过」/「真缺失」、残缺窗口的预测 / 完整窗口的预测。四条的失败
 * 方式是同一种——一个关于生产集群的错误结论，长得和正确结论一模一样。
 *
 * 判定集中在这一个纯函数模块里，页面只渲染它的返回值：散在五个 .tsx 里的
 * `c.dataSource === 'COLLECTED'` 会各自漂，而漂的方向恰恰是"缺字段时当成
 * 真实"——那正是这一轮要挡的东西。这里也是 tests/ 唯一能钉住它的地方
 * （api/client.ts 在测试工程里编不过，页面 .tsx 则根本无法被 node --test 载入）。
 */

/* ---------------------------------------------------------------------- */
/* §2 数据来源                                                              */
/* ---------------------------------------------------------------------- */

/**
 * 数据来源的三种呈现。UNKNOWN 与后端的两个登记值并列，是本模块自己的
 * 第三种状态：后端没给这个字段时**必须**落在它上面。
 */
export type DataSourceCode = 'FIXTURE' | 'COLLECTED' | 'UNKNOWN'

export interface DataSourceView {
  readonly code: DataSourceCode
  /** 一眼可读的来源名，含原始枚举值——枚举值是与后端对账的那个词。 */
  readonly label: string
  /** 这个来源意味着屏幕上这些数字能拿来做什么、不能做什么。 */
  readonly detail: string
  /**
   * true 时必须显著呈现。
   *
   * 演示数据与来源未知都不是"补充说明"：一份没被读出来的来源标注，与
   * 没有标注等价，而这一整条的场合正是内容离开浏览器之后。
   */
  readonly emphasized: boolean
}

/**
 * 把 registry.Cluster.DataSource 翻成屏幕上那一句话。
 *
 * **缺席、空串、以及任何未登记的取值一律落到 UNKNOWN**，不落到 COLLECTED：
 * 老响应、未注册的集群、后端把字段改了名，在前端看起来是同一件事——我们
 * 不知道这些数字描述的是哪种集群。把它显示成"真实采集"，就是把一个我们
 * 不知道的事情说成最危险的那个方向（design doc §2）。
 */
export function dataSourceView(raw: DataSource | undefined): DataSourceView {
  switch (raw) {
    case 'COLLECTED':
      return {
        code: 'COLLECTED',
        label: '真实采集数据 · COLLECTED',
        detail: '本屏内容来自该集群真实采集到的资产与流量。',
        emphasized: false,
      }
    case 'FIXTURE':
      return {
        code: 'FIXTURE',
        label: '演示数据 · FIXTURE',
        detail: '本屏内容来自仓库内的合成数据集，不描述任何真实集群。'
          + '不得据此判断生产集群的现状，也不得据此批准一次下发。',
        emphasized: true,
      }
    default:
      return {
        code: 'UNKNOWN',
        label: '数据来源未知',
        detail: '服务端没有给出这个集群的数据来源（集群未注册、老响应，或字段改了名）。'
          + '未知不等于真实：在弄清来源之前，本屏内容不得当作关于生产集群的结论使用。',
        emphasized: true,
      }
  }
}

/* ---------------------------------------------------------------------- */
/* §3 清单截断                                                              */
/* ---------------------------------------------------------------------- */

/**
 * 安全发现里三个各自截断的清单。
 *
 * 是枚举而不是一个「缩窗有没有用」的布尔参数：那个布尔一旦交给调用方，
 * 就有一天会有人在 nakedPods 上传 true，于是界面开始教运维去缩小一个
 * 与这份清单毫无关系的时间窗。
 */
export type FindingList = 'RISKY_FLOWS' | 'EGRESS_TARGETS' | 'NAKED_PODS'

/** 各清单的计量单位，与 SecurityPage 既有的 meta 措辞一致。 */
const UNIT: Record<FindingList, string> = {
  RISKY_FLOWS: '条',
  EGRESS_TARGETS: '个目标',
  NAKED_PODS: '个',
}

export interface ListTruncationView {
  /** 服务端明确回显了"截断前比现在多"。 */
  readonly truncated: boolean
  /** 条数那句话，句式与 FlowsPage 的 total / limit 一致。 */
  readonly countText: string
  /**
   * 被截断时的处置指引；未截断、或服务端没回显条数时为空串。
   *
   * 缩小时间窗只对前两个清单有用：nakedPods 来自锚点那一次资产快照，
   * 与流量窗口无关（design doc §3）。
   */
  readonly remedy: string
}

const NARROWING_HELPS =
  '缩小时间窗可以让这份清单落回上限之内——它来自这段窗口的流量。'

const NARROWING_USELESS =
  '缩小时间窗对这份清单没有用：它来自锚点那一次资产快照，与流量时间窗无关。'
  + '条数要降下来，只能是集群里没被策略选中的 Pod 真的变少。'

/**
 * 一个清单的条数回显。
 *
 * 三种情况必须彼此可分：截断了（说清共几条、此处几条、怎么办）、没截断
 * （共几条就是全部）、以及**服务端根本没回显**（一份老响应）。第三种不得
 * 渲染成前两种里的任何一种：那样一份不知道有没有被截过的清单，读起来就是
 * "这个集群只有这些"。
 */
export function listTruncationView(
  list: FindingList, shown: number, echo: ListTruncation | undefined,
): ListTruncationView {
  const unit = UNIT[list]

  // limit 恒大于 0（后端 store.MaxSecurityFindings）；不是的话这份回显
  // 本身不可信，按"没回显"处置，而不是拿它算一个说不清来历的差值。
  if (!echo || echo.limit <= 0) {
    return {
      truncated: false,
      countText: `此处显示 ${shown} ${unit}（服务端未回显总数，无法判断这份清单是否被截断）`,
      remedy: '',
    }
  }

  if (echo.total <= shown) {
    return { truncated: false, countText: `共 ${echo.total} ${unit}`, remedy: '' }
  }

  return {
    truncated: true,
    countText:
      `共 ${echo.total} ${unit}，此处显示 ${shown} ${unit}（服务端上限 ${echo.limit} ${unit}）`,
    remedy: list === 'NAKED_PODS' ? NARROWING_USELESS : NARROWING_HELPS,
  }
}

/* ---------------------------------------------------------------------- */
/* §4 未评估的 Baseline                                                     */
/* ---------------------------------------------------------------------- */

/**
 * 一个缺失类型的「依据我们看过没有」的三种取值。
 *
 * 是三值而不是布尔：后端契约（internal/store/policy.go 的
 * NotAssessedBaselines）把 `[]` 与 `null` 定义成**两件不同的事** ——
 * `[]` 是"五类依据我们都检查过、都在"，`null` 是"这个 Reader 根本没
 * 回答过这个问题"。一个布尔只能把后者折成前者，而那正好是朝"一切正常"
 * 的方向折（design doc §4）。
 */
export type BaselineAssessment = 'ASSESSED' | 'NOT_ASSESSED' | 'UNKNOWN'

export interface BaselineGapView {
  readonly kind: Kind
  /** 这一类的依据资产：看过（真缺失）/ 没看过 / 服务端没说。 */
  readonly assessment: BaselineAssessment
  /**
   * 徽标文案；ASSESSED 时为空串。
   *
   * 文案在这里而不在 .tsx 里：三种状态各自显示什么是本模块能被测试钉住的
   * 唯一一层，写进 JSX 就只剩浏览器能回答（design doc §7）。
   */
  readonly badge: string
  /** 处置——三种缺口的区别全在这里。 */
  readonly remedy: string
}

const MISSING_REMEDY = '去写一条覆盖它的策略。'

const NOT_ASSESSED_REMEDY =
  '未评估：这一类的依据资产从没采到过，或那次采集被拒绝/超时。'
  + '处置是去修采集（权限、采集器），不是去写策略。'

const UNKNOWN_REMEDY =
  '服务端没有说明这一类的依据评估过没有（老响应，或字段改了名）。'
  + '因此现在还断不出该做什么：真缺失要去写策略，未评估要去修采集，两条处置相反。'
  + '先弄清服务端为什么没回答这个问题。'

/**
 * 把一个 namespace 的缺失类型标注成「真缺失」/「未评估」/「无从判断」。
 *
 * **叠加，不是分栏**：未评估的类型仍然出现在返回值里，只是多带一条不同的
 * 处置（design doc §4）。把它们从清单里摘出去，等于让一个"我们没看过"
 * 悄悄变成"这里没问题"。三种都照旧挡住 Enforcing，标注不放宽任何门禁。
 *
 * **`null` / 缺席一律落到 UNKNOWN，不落到"一条都没未评估"**，与
 * dataSourceView、wouldBreakQualifier 同一条纪律：后端契约要求这个字段恒为
 * 非 nil，因此真拿到 `null` 只说明这不是一份守约的响应 —— 把它读成"五类
 * 依据都检查过了"，就是把一个我们不知道的事情说成最让人安心的那个方向。
 */
export function baselineGapViews(
  kinds: Kind[], notAssessed: Kind[] | null | undefined,
): BaselineGapView[] {
  if (notAssessed == null) {
    return kinds.map((kind) => ({
      kind, assessment: 'UNKNOWN' as const, badge: '评估情况未知', remedy: UNKNOWN_REMEDY,
    }))
  }
  const blind = new Set<Kind>(notAssessed)
  return kinds.map((kind) => blind.has(kind)
    ? { kind, assessment: 'NOT_ASSESSED' as const, badge: '未评估', remedy: NOT_ASSESSED_REMEDY }
    : { kind, assessment: 'ASSESSED' as const, badge: '', remedy: MISSING_REMEDY })
}

/**
 * 缺失清单上方那句总说明；服务端答了"一类都没未评估"时为空串。
 *
 * 不标注就等于塌回普通缺失，运维会去写一条 DNS 策略，而真正该做的是改
 * RBAC（design doc §4）。**服务端根本没回答时同样要出一句话** —— 那一句
 * 不是"没有未评估的类型"，而是"我们不知道有没有"。
 */
export function notAssessedNote(notAssessed: Kind[] | null | undefined): string {
  if (notAssessed == null) {
    return '服务端没有说明这些缺失类型的依据评估过没有（notAssessedBaselines 缺席，'
      + '按契约它恒为非 nil，因此这是一份老响应或字段改了名）。'
      + '缺席不等于"都评估过"：下面每一类都可能是「真缺失」，也可能是「平台压根没看过」，'
      + '而两者的处置相反（写策略 / 修采集）。在服务端答上来之前，不得把下表当作关于集群的结论读。'
      + '三种缺口都照旧挡住 Enforcing —— 这条标注不放宽任何门禁。'
  }
  if (notAssessed.length === 0) return ''
  return `其中 ${notAssessed.join('、')} 属于「未评估」：平台从没拿到过这几类的依据资产`
    + '（从未采集，或那次采集被拒绝、超时），因此无从判断它们是不是真的缺。'
    + '处置是去修采集，不是去写策略。两种缺口都照旧挡住 Enforcing —— 这条标注不放宽任何门禁。'
}

/* ---------------------------------------------------------------------- */
/* §5 残缺窗口的 WOULD_BREAK                                                */
/* ---------------------------------------------------------------------- */

/**
 * WOULD_BREAK 旁边的限定语；窗口完整时为空串。
 *
 * 窗口非 COMPLETE 时 policygen 学不出任何放行规则，这个数会逼近整窗连接
 * 数——它量的是「候选集为空时有多少连接被 Baseline 拦下」，不是「上线会
 * 打断多少条」。数字本身没错，错的是不加限定语地呈现它（design doc §5）。
 *
 * **未登记的取值与缺席一律加限定语**，与 dataSourceView 同一条纪律：
 * 一个我们读不懂的完整度，不能当成"完整"。
 */
export function wouldBreakQualifier(completeness: Completeness | undefined): string {
  switch (completeness) {
    case 'COMPLETE':
      return ''
    case 'DEGRADED':
      return '这段观测窗口不完整（DEGRADED，已知有漏）。窗口非 COMPLETE 时不会学出任何放行规则，'
        + '下面的 WOULD_BREAK 会逼近整窗连接数：它量的是「候选集为空时有多少连接被 Baseline 拦下」，'
        + '不是「这条推荐上线会打断多少条」。不要拿它当上线影响数。'
    case 'UNKNOWN':
      return '这段观测窗口的完整度无从判断（UNKNOWN：来源没有给出足以证明"没漏"的证据）。'
        + '窗口非 COMPLETE 时不会学出任何放行规则，下面的 WOULD_BREAK 会逼近整窗连接数：'
        + '它量的是「候选集为空时有多少连接被 Baseline 拦下」，不是「这条推荐上线会打断多少条」。'
    default:
      return '服务端没有给出这段观测窗口的完整度（老响应，或字段改了名）。'
        + '在确认它是 COMPLETE 之前，下面的 WOULD_BREAK 不能当作上线影响数：'
        + '窗口不完整时它量的是「候选集为空时有多少连接被 Baseline 拦下」。'
  }
}
