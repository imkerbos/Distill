import type { PredictionReport } from '../api/types'

/**
 * dry-run 一屏的全部取数：tile 箭头的两端、明细区渲染哪一套预测，
 * 以及描述那套预测的几句话。
 *
 * 抽成一个函数、一个返回值，是因为这里错一次的后果不是排版难看：四个
 * tile 展示「默认 → 人工版本」的箭头与差值，而下面的连接清单、UNKNOWN
 * 构成、六个次级统计只能来自其中一套。如果 tile 显示人工版本、清单显示
 * 默认版本，页面就会在同一屏上用文字断言一件与正上方数字相反的事——
 * 「WOULD_OPEN 为 0 是一个真实的 0」印在一个写着 `0 → 3  +3` 的 tile
 * 底下。spec §5 存在的全部理由就是不让这条阅读成立：人要能看见自己那
 * 几次确认的代价，而代价恰恰在明细里。
 *
 * 「这条阅读不该成立」不能只靠一条事后发现它的测试，因为出错的形状是
 * 「有人改了一个 rows 属性」，而测试盯的是判定逻辑、不是 JSX 的接线。
 * 因此这里把它做成写不出来的：
 *
 *   1. 两套预测只在本文件里同时可见，取数结束后调用方拿到的是一个视图，
 *      不再是两个 PredictionReport；
 *   2. emphasized（tile 箭头右端、被强调的那个数）直接取自
 *      detail.report.counts——被强调的数与它下面的清单是同一个对象的两种
 *      呈现，不是两次各自选择的结果；
 *   3. 视图类型带品牌（见下），别处拼不出一个「tile 取 A、明细取 B」的
 *      视图，只能调用本文件这一个构造函数。
 */

/**
 * 品牌符号：只在本文件声明，不导出，运行期不存在。
 *
 * 它让下面两个视图类型无法在别处用对象字面量伪造出来——伪造一个
 * 「tile 取 overridden、明细取 prediction」的视图，正是 C1 那条缺陷的
 * 形状。外部拿不到这个符号，就构造不出这两个类型的值。
 */
declare const selected: unique symbol

/** 四类变化的计数，tile 箭头的一端。 */
type ChangeCounts = PredictionReport['counts']

/** 明细区的全部输入：一套已经选定的预测，加上描述它的两句话。 */
export interface DryRunDetailView {
  readonly [selected]: 'detail'
  /** 明细区渲染的那一套预测，与 tile 上被强调的数字同源。 */
  readonly report: PredictionReport
  /** 口径说明：这些行算的是哪一套推荐。有覆盖时必须写明，否则读的人默认它是「推荐」。 */
  readonly basis: string
  /**
   * 空态里那句「不是未检测」的补充说明。
   *
   * 与 report 同源：这句话断言的是「这个 0 是算出来的」，一旦它描述的
   * 是另一套预测，它就是一句关于敞口的假话。
   */
  readonly emptyDetail: string
}

/** dry-run 一屏的取数结果。只能由 dryRunView 构造。 */
export interface DryRunView {
  readonly [selected]: 'view'
  /** 存在人工决定：tile 展示「默认 → 人工版本」与差值，否则退化为单值。 */
  readonly showDelta: boolean
  /** tile 箭头左端，恒为默认推荐。 */
  readonly baseline: ChangeCounts
  /** tile 箭头右端，即被强调、字号更大的那一列。 */
  readonly emphasized: ChangeCounts
  /** 明细区。emphasized 就是 detail.report.counts，同一个对象。 */
  readonly detail: DryRunDetailView
}

/**
 * showDelta 为 true（存在人工决定）时，被强调的一列与明细区都是人工
 * 版本。没有人工决定时两套预测恒等（store 层的 Apply 对空覆盖是恒等
 * 变换），两端同为默认版本，tile 退化成单值。
 */
export function dryRunView(
  prediction: PredictionReport, overridden: PredictionReport, overrideCount: number,
): DryRunView {
  const showDelta = overrideCount > 0
  const detail = showDelta
    ? detailView(
      overridden,
      '以下明细来自应用人工决定之后的版本，即上方每个指标箭头右侧的那个数。',
      '基于应用人工决定之后的候选策略对观测流量重放计算得出，不是未检测。',
    )
    : detailView(
      prediction,
      '以下明细来自默认推荐。',
      '基于当前候选策略对观测流量重放计算得出，不是未检测。',
    )

  // emphasized 取 detail.report.counts，而不是在这里再选一次：再选一次
  // 就又出现了两个可以互相矛盾的选择点，而这两个选择点分歧的名字就叫 C1。
  return {
    showDelta,
    baseline: prediction.counts,
    emphasized: detail.report.counts,
    detail,
  } as DryRunView
}

/**
 * 唯一的品牌落点。断言是必要的：品牌属性只存在于类型层，运行期没有
 * 对应的值可写。把它关在一个函数里，是为了让「凭空造一个视图」这件事
 * 在整个前端只有这一处写法，改动它必然出现在 review 的视线里。
 */
function detailView(
  report: PredictionReport, basis: string, emptyDetail: string,
): DryRunDetailView {
  return { report, basis, emptyDetail } as DryRunDetailView
}

/** WOULD_OPEN 的空态补充说明：在口径说明之后，再点明这个 0 的性质。 */
export function wouldOpenEmptyDetail(view: DryRunDetailView): string {
  return `${view.emptyDetail}WOULD_OPEN 为 0 是一个真实的 0。`
}

/** 旧策略额外放行了多少 —— 两份预测的差额。 */
export interface ExistingReliefView {
  /** 差额是否有意义：两份预测相同时为 false，那时不显示任何东西。 */
  present: boolean
  /** 形如 `旧策略额外放行了 12 条连接`。 */
  text: string
  /** 该怎么读这个数字。 */
  note: string
}

/**
 * existingReliefView 算出"旧策略额外放行了多少"。
 *
 * 差额 = 只跑候选集会拦断的条数 − 合并之后会拦断的条数
 * （design doc 2026-08-25-existing-policies §3）。
 *
 * **这个数字本身就是一个结论**：差额大说明集群里的旧策略比新候选集宽得多，
 * 那些放行现在靠旧策略撑着，而候选集没有覆盖它们 —— 一旦有人清理旧策略，
 * 这些连接就会断。它是接管路线的入口指标。
 *
 * 差额为负时不显示：平台只加不删，合并之后不可能拦断得更多，出现负数说明
 * 两份预测跑在了不同的输入上，那是一个 bug 而不是一条要展示给操作者的结论。
 */
export function existingReliefView(
  candidatesOnly: PredictionReport, withExisting: PredictionReport,
): ExistingReliefView {
  const delta = (candidatesOnly.counts.WOULD_BREAK ?? 0) - (withExisting.counts.WOULD_BREAK ?? 0)
  if (delta <= 0) {
    return { present: false, text: '', note: '' }
  }
  return {
    present: true,
    text: `旧策略额外放行了 ${delta} 条连接`,
    note:
      '上面那几个数字算的是「合并之后」：集群里原有的策略不会因为这次写回消失，'
      + `因此真实影响按"已有 ∪ 候选"计。如果把旧策略也一并清理掉，会多拦断 ${delta} 条 —— `
      + '这些放行现在靠旧策略撑着，而候选集没有覆盖它们。这个数越大，'
      + '说明旧策略比新候选集宽得越多。',
  }
}
