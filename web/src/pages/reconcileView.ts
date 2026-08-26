import type {
  ReconcileCounts, ReconcileSubjectCounts,
  ObservationCoverage, ReconciliationReport, ReconciliationSample, TrendPoint,
} from '../api/types'

/**
 * MAX_UNDER_PERMISSIVE_RATE 与后端 httpapi.maxUnderPermissiveRate 是同一个数。
 *
 * 界面上标"会被门禁拦"的那些，必须就是服务端实际会拦的那些 —— 两个数一旦
 * 分家，操作者会对着一份"看起来能推"的清单反复撞门，或者反过来以为某个
 * workload 推不出去而去改一个根本没问题的东西。有一条用例直接读 Go 源码
 * 比对这两个常量。
 */
export const MAX_UNDER_PERMISSIVE_RATE = 0.05

/** 两个方向各自的说明。放在这里而不是页面里：它要被用例断言真的渲染出来了。 */
export const DISAGREEMENT_HELP = {
  under:
    '平台判 DENY、集群实际放行。这是唯一一类能绕过 dry-run 造成阻断的分歧：'
    + '候选规则里很可能缺了现在正通着的放行，而 dry-run 会把它算成"无变化"，'
    + '因为在平台的世界里那些连接本来就不通。',
  over:
    '平台判 ALLOW、集群实际拦下了。平台高估了放行面，后果是多生成一条规则：'
    + '安全性变差，可用性无损。',
} as const

/** 一行主体的对账结果。 */
export interface SubjectRow {
  label: string
  /** 低估放行面的占比；没有可比对连接时为 null。 */
  underRate: number | null
  underCount: number
  overCount: number
  agreeCount: number
  /** 这个主体的候选规则会不会被写回门禁拦下。 */
  blocked: boolean
}

/** 一个方向的分歧在界面上的形态。 */
export interface DirectionView {
  count: number
  help: string
  tone: 'danger' | 'warn'
}

/** reconcileView 是一致率这一屏的全部内容。 */
export interface ReconcileView {
  /** 有没有可比对的连接。为 false 时 rateText 是占位符，不是数字。 */
  available: boolean
  /** 答不出来的原因；available 为 true 时是空串。 */
  unavailableReason: string
  /** 一致率的显示文本；答不出时是 '—'。 */
  rateText: string
  /** 参与计算的连接数。 */
  comparable: number
  /** 平台答不出的条数：**不是分歧**，是未覆盖。 */
  platformUnknown: number
  under: DirectionView
  over: DirectionView
  /** 按主体的明细，会被门禁拦的排在最前。 */
  subjects: SubjectRow[]
  /** 分歧证据的抽样，后端已按能造成阻断的那一类在前排好。 */
  samples: SampleRow[]
}

/** 一条分歧证据在界面上要显示的东西。 */
export interface SampleRow {
  /** 主体的写法，与 SubjectRow.label 同源 —— 两处不同会让人对不上号。 */
  subject: string
  /** 方向标签：能造成阻断的那一类要一眼可辨。 */
  classLabel: string
  /** true 表示这一条属于会造成阻断的那一类，界面据此上语义色。 */
  blocking: boolean
  /** 形如 `shop/web-1 -> payment/api-1  TCP/8080`。 */
  connection: string
  /** 连接发生的时刻，本地时间；不合法时原样回显。 */
  atText: string
}

/** comparableOf 是一致率的分母：只含可比对的那三类，与后端同一口径。 */
function comparableOf(c: ReconcileCounts): number {
  return c.AGREE + c.DISAGREE_OVER_PERMISSIVE + c.DISAGREE_UNDER_PERMISSIVE
}

/**
 * reconcileView 把一份对账报告变成界面要的那几项。
 *
 * **答不出来时给占位符，不给数字**：分母为零而显示 100%，会让一个什么都
 * 没比过的集群看起来最可信；显示 0% 则会被读成"平台全错"。两者都是编的。
 */
export function reconcileView(r: ReconciliationReport | null): ReconcileView {
  const empty: ReconcileView = {
    available: false, unavailableReason: '还没有对账数据。', rateText: '—',
    comparable: 0, platformUnknown: 0,
    under: { count: 0, help: DISAGREEMENT_HELP.under, tone: 'danger' },
    over: { count: 0, help: DISAGREEMENT_HELP.over, tone: 'warn' },
    subjects: [], samples: [],
  }
  if (r === null) return empty

  const c = r.report.overall
  const comparable = comparableOf(c)
  const under: DirectionView = {
    count: c.DISAGREE_UNDER_PERMISSIVE, help: DISAGREEMENT_HELP.under, tone: 'danger',
  }
  const over: DirectionView = {
    count: c.DISAGREE_OVER_PERMISSIVE, help: DISAGREEMENT_HELP.over, tone: 'warn',
  }

  if (!r.sourceReportsVerdicts) {
    return {
      ...empty, under, over, platformUnknown: c.PLATFORM_UNKNOWN,
      unavailableReason:
        '这个集群的流量来源不报判定（NODE_CONNTRACK 接入或演示数据集），因此对不了账。'
        + '这不是"一致率低"——是没有可以比对的执行平面结论。要拿到一致率，'
        + '得让流量走 Hubble 这类会报判定的来源。',
    }
  }
  if (comparable === 0) {
    return {
      ...empty, under, over, platformUnknown: c.PLATFORM_UNKNOWN,
      unavailableReason:
        '这段窗口里没有一条既被平台判出结论、又被执行平面报了判定的连接，'
        + '因此算不出一致率。先看"平台答不出"那一栏 —— 它说的是覆盖不足，不是判错。',
    }
  }

  return {
    available: true, unavailableReason: '',
    rateText: `${((c.AGREE / comparable) * 100).toFixed(1)}%`,
    comparable, platformUnknown: c.PLATFORM_UNKNOWN,
    under, over,
    subjects: subjectRows(r.report.bySubject),
    samples: sampleRows(r.samples),
  }
}

/**
 * sampleRows 把分歧证据排成一张表。
 *
 * **后端已经按"能造成阻断的那一类在前"排好了，这里不重排**：一份在两处各排
 * 一次的清单迟早会有两种次序，而操作者拿着界面上的第 3 条去对后端日志时，
 * 对不上号比排序不好看严重得多。
 */
export function sampleRows(samples: readonly ReconciliationSample[] | null): SampleRow[] {
  return (samples ?? []).map(s => ({
    subject: s.subject.workload === ''
      ? `${s.subject.namespace}/（这些 Pod 没有 workload 归属标签）`
      : `${s.subject.namespace}/${s.subject.workload}`,
    classLabel: s.class === 'DISAGREE_UNDER_PERMISSIVE'
      ? '平台判 DENY、集群放行'
      : '平台判 ALLOW、集群拦下',
    blocking: s.class === 'DISAGREE_UNDER_PERMISSIVE',
    connection: `${s.source} → ${s.dest}  ${s.protocol}/${s.port}`,
    atText: sampleTime(s.at),
  }))
}

/**
 * sampleTime 渲染连接发生的时刻。
 *
 * 不合法时原样回显而不是显示 `Invalid Date`：前者读起来是"这条数据有问题"，
 * 后者读起来是"界面坏了"，而排查方向不同（同 formatCollectedAt）。
 */
function sampleTime(iso: string): string {
  const t = new Date(iso)
  return Number.isNaN(t.getTime()) ? iso : t.toLocaleString()
}

/**
 * MAX_SAMPLES_PER_CLASS 与后端 reconcile.MaxSamplesPerClass 是同一个数。
 *
 * 界面上说"至多留 N 条"，那个 N 必须就是后端真的留下的条数 —— 两个数一旦
 * 分家，读的人会以为自己看到的是全部（同 MAX_UNDER_PERMISSIVE_RATE）。
 */
export const MAX_SAMPLES_PER_CLASS = 5

/**
 * 样本区块的说明。
 *
 * 必须说清**为什么只有几条**：一个只列了 5 条的清单，读的人会以为分歧只有
 * 5 条，而它其实是抽样 —— 真实条数在上面的计数里。
 */
export const SAMPLES_HELP =
  `这里是抽样，不是全部：每个 workload 每个方向至多留 ${MAX_SAMPLES_PER_CLASS} 条，`
  + '真实条数看上面的计数。取的是窗口里最早的几条，因此同一份报告反复打开是一样的。'
  + '要判断平台漏了什么，先看这些连接的对端与端口有没有共同点 —— '
  + '集中在少数几个端口，多半是某条策略被平台看不见的平面放行了。'

/** 一条样本都没有时显示的话。空区块读起来像"这一栏坏了"，而它是一条结论。 */
export const SAMPLES_NONE =
  '没有分歧证据：这段窗口里平台判定与集群实际执行没有对不上的连接。'


/**
 * subjectRows 把按主体的计数排成一张表，**会被门禁拦的排在最前**。
 *
 * 这一屏的读者要先看到推不出去的那些：一个按名字排序的表格会把唯一有问题的
 * workload 排到第 40 行。
 */
function subjectRows(in_: ReconcileSubjectCounts[]): SubjectRow[] {
  const rows = in_.map((s): SubjectRow => {
    const comparable = comparableOf(s.counts)
    const underRate = comparable === 0
      ? null
      : s.counts.DISAGREE_UNDER_PERMISSIVE / comparable
    return {
      label: subjectLabel(s),
      underRate,
      underCount: s.counts.DISAGREE_UNDER_PERMISSIVE,
      overCount: s.counts.DISAGREE_OVER_PERMISSIVE,
      agreeCount: s.counts.AGREE,
      blocked: underRate !== null && underRate > MAX_UNDER_PERMISSIVE_RATE,
    }
  })
  rows.sort((a, b) => {
    if (a.blocked !== b.blocked) return a.blocked ? -1 : 1
    return (b.underRate ?? -1) - (a.underRate ?? -1)
  })
  return rows
}

/**
 * subjectLabel 与后端 reconcile.Subject.Label 说的是同一件事。
 *
 * workload 为空不是"没有主体"，是这些 Pod 一个归属标签都没有 —— 渲染成
 * "ns/" 那样的断尾，读者拿着它无法行动。
 */
function subjectLabel(s: ReconcileSubjectCounts): string {
  if (s.subject.workload === '') {
    return `${s.subject.namespace}/（这些 Pod 没有 workload 归属标签）`
  }
  return `${s.subject.namespace}/${s.subject.workload}`
}

/** 趋势上的一个点在界面上要显示的东西。 */
export interface TrendRow {
  /** 窗口起点，本地时间。 */
  atText: string
  /** 一致率文本；算不出时是 '—'，**不是 0%**。 */
  rateText: string
  /**
   * 归一化到 0..1 的柱高；算不出时为 null。
   *
   * 与 rateText 分开给：一条算不出的记录必须**画不出柱子**，而不是画一根
   * 贴地的 —— 贴地的柱子读起来是"那天全错了"。
   */
  bar: number | null
  /** 算不出时的原因；算得出时是空串。 */
  missingReason: string
  /** 参与计算的连接数。 */
  comparable: number
  under: number
  over: number
  /** true 表示这一点的低估分歧率超过门禁阈值。 */
  blocked: boolean
}

/**
 * trendRows 把趋势渲染成一张按时间倒序的表（最近的在前）。
 *
 * **不重排、不补齐缺失的时段**：补齐要凭空造点，而造出来的点在图上与真实
 * 观测长得一样。采集断过的那段时间就该是空的 —— 那本身是要看的信息。
 */
export function trendRows(trend: TrendPoint[] | null | undefined): TrendRow[] {
  return (trend ?? []).map(p => {
    const blocked = p.comparable > 0
      && p.under / p.comparable > MAX_UNDER_PERMISSIVE_RATE
    return {
      atText: trendTime(p.windowFrom),
      rateText: p.rate === null ? '—' : `${(p.rate * 100).toFixed(1)}%`,
      bar: p.rate,
      missingReason: p.rate === null ? missingRateReason(p) : '',
      comparable: p.comparable,
      under: p.under,
      over: p.over,
      blocked,
    }
  })
}

/**
 * missingRateReason 说明这一轮为什么算不出一致率。
 *
 * 两种原因的处置完全不同：来源不报判定要换流量来源，而"这一轮没有可比对的
 * 连接"等下一轮就好。一句笼统的"无数据"会让人去做错的那件事。
 */
function missingRateReason(p: TrendPoint): string {
  return p.sourceReports
    ? '这一轮没有既被平台判出结论、又被执行平面报了判定的连接。'
    : '这一轮的流量来源不报判定，对不了账。'
}

/** trendTime 渲染窗口起点；不合法时原样回显（同 sampleTime）。 */
function trendTime(iso: string): string {
  const t = new Date(iso)
  return Number.isNaN(t.getTime()) ? iso : t.toLocaleString()
}

/**
 * 趋势区块的说明。
 *
 * 必须说清**这条线的读法**：绝对值没有行动含义，走向才有。
 */
export const TREND_HELP =
  '一次 97% 说明不了什么，从 100% 掉到 97% 才是信号 —— 看的是走向，不是某一个数。'
  + '算不出一致率的那几轮是空的，不是 0：把"算不出"画成 0 会让它读起来像那天全错了。'
  + '每一行的"可比对"是那一轮的分母 —— 基于 3 条连接的 100% 与基于 3 万条的 100% 不是一回事。'

/** 一轮都没有时显示的话。 */
export const TREND_NONE =
  '这个集群还没有对账历史。对账在每次流量摄入之后自动跑，跑过一轮之后这里就会有记录。'

/** 观测覆盖在界面上要显示的东西。 */
export interface CoverageView {
  /** 算不算得出来。为 false 时下面几项是占位符。 */
  available: boolean
  /** 算不出时的说明；算得出时是空串。 */
  unavailableReason: string
  spanText: string
  coveredText: string
  gapText: string
  /**
   * true 表示间隙已经大到值得去查采集链路。
   *
   * 判据是**间隙占了跨度的一半以上**：那时"我们观测了多久"这句话的一半以上
   * 是空的，而操作者多半以为整段都看着。
   */
  alarming: boolean
  /** alarming 为 true 时该说的那句话；否则是空串。 */
  alarm: string
}

/**
 * 间隙占比超过它就报警。
 *
 * **纯展示口径，不解锁也不阻断任何操作**：真正拦人的是写回那道学习期门禁，
 * 它拿的是覆盖时长与业务周期比（§5）。这里只负责让人在门禁拦他之前就看见。
 */
export const ALARMING_GAP_RATIO = 0.5

/**
 * coverageView 把观测覆盖渲染成一栏。
 *
 * 防的是一条**当前正在发生**的静默丢失：Hubble 的事件环形缓冲只保留最近很短
 * 一段（演练集群实测约 107 秒，繁忙集群是个位数秒），采集间隔一旦超过它，
 * 中间那段流量平台永远看不到，而库里没有任何迹象（design doc §6.2a）。
 */
export function coverageView(c: ObservationCoverage | null | undefined): CoverageView {
  const empty: CoverageView = {
    available: false, unavailableReason: '', spanText: '—', coveredText: '—',
    gapText: '—', alarming: false, alarm: '',
  }
  if (c == null) {
    return {
      ...empty,
      unavailableReason:
        '还算不出这个集群被观测了多久：一次成功的流量摄入都没有。'
        + '这不是"覆盖为零"——是还没开始。先把采集器跑起来。',
    }
  }
  const ratio = c.spanSeconds > 0 ? c.gapSeconds / c.spanSeconds : 0
  const alarming = ratio > ALARMING_GAP_RATIO
  return {
    available: true, unavailableReason: '',
    spanText: humanSeconds(c.spanSeconds),
    coveredText: humanSeconds(c.coveredSeconds),
    gapText: humanSeconds(c.gapSeconds),
    alarming,
    alarm: alarming
      ? `最早与最晚一次摄入之间跨了 ${humanSeconds(c.spanSeconds)}，`
        + `而其中只有 ${humanSeconds(c.coveredSeconds)} 真的收到了流量 —— `
        + '中间存在没有任何摄入的时段。Hubble 的事件缓冲只保留最近很短一段，'
        + '采集间隔超过它，那段流量就永远看不到了，而库里不会有任何迹象。'
        + '先查采集链路：光等不会把那一段补回来。'
      : '',
  }
}

/**
 * humanSeconds 把秒数写成人读得懂的样子。
 *
 * 与后端 humanDuration 同一套口径：同一段时长在写回的拒绝理由与这一屏上
 * 必须是同一种写法，否则操作者会以为是两个不同的数。
 */
export function humanSeconds(sec: number): string {
  if (!Number.isFinite(sec) || sec < 0) return '—'
  const day = 86400, hour = 3600, minute = 60
  if (sec >= day) {
    const days = Math.floor(sec / day)
    const hours = Math.floor((sec % day) / hour)
    return hours === 0 ? `${days} 天` : `${days} 天 ${hours} 小时`
  }
  if (sec >= hour) {
    return `${Math.floor(sec / hour)} 小时 ${Math.floor((sec % hour) / minute)} 分`
  }
  if (sec >= minute) return `${Math.floor(sec / minute)} 分`
  return `${Math.round(sec)} 秒`
}
