import type {
  ReconcileCounts, ReconcileSubjectCounts,
  ReconciliationReport, ReconciliationSample,
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
