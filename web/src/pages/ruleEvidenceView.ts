import type { PolicyPreview, RuleEvidence } from '../api/types.ts'

/**
 * 规则证据的展示派生（design doc 2026-08-25-trust-engineering §P1）。
 *
 * 要解决的是这样一屏：两条规则并排显示「12 条流量」，一条已经被观察了三周
 * 每天都如此，另一条是刚才那一个小时里第一次出现。屏幕上它们长得一模一样，
 * 而能不能下发，两者结论相反。
 *
 * **证据不是门禁。** 它回答"看了多久"，不回答"看全了没有"—— 一条规则在一段
 * 始终不完整的观测里出现一百次，仍然可能漏掉真正会被拦断的那条连接。放行
 * 与否照旧由学习窗与一致率那两道门决定，这里只负责让人看见强弱之别。
 */

/**
 * 证据键：主体 + 规则指纹，与后端 snapshotstore.EvidenceKey 同形。
 *
 * **不能只用指纹**：指纹只覆盖规则内容，不含主体 ——「egress 到
 * kube-dns:53」在集群里每个 workload 上都是同一个指纹。
 */
export function evidenceKey(namespace: string, workload: string, fingerprint: string): string {
  return `${namespace}/${workload}/${fingerprint}`
}

/** 证据强度分档。UNTRACKED 是"没在记"，与"记了但很弱"必须分开。 */
export type EvidenceStrength = 'UNTRACKED' | 'NONE' | 'THIN' | 'GROWING' | 'ESTABLISHED'

/**
 * 分档阈值。
 *
 * **纯展示口径，不解锁任何操作**：调这两个数不会让任何东西变得可以下发，
 * 只会改变徽标的颜色。真正的门禁常量在 reconcileView（一致率）与集群登记的
 * 学习窗里，两处刻意不共用同一个数 —— 共用会让"改个显示阈值"顺手把门禁一起
 * 放宽。
 */
export const ESTABLISHED_WINDOWS = 12
export const GROWING_WINDOWS = 3

const STRENGTH_LABEL: Record<EvidenceStrength, string> = {
  UNTRACKED: '未记录',
  NONE: '本轮新出现',
  THIN: '证据薄',
  GROWING: '在积累',
  ESTABLISHED: '证据充分',
}

/**
 * UNTRACKED 与 NONE 各有一句说明，其余三档不给。
 *
 * 这两档最容易被读错：一个空白格子会被读成"证据为零"，而它真正的含义是
 * "这个集群没在记"（演示集群、或从未跑过采集）—— 与 trafficObserved 那条
 * 纪律同源。其余三档的标签本身已经说清楚了，再编一段解释只会稀释这两句。
 */
const STRENGTH_NOTE: Partial<Record<EvidenceStrength, string>> = {
  UNTRACKED: '这个集群没有记录跨窗口证据，空白不等于"没有证据"。',
  NONE: '这条规则在更早的窗口里没有出现过，可能是新行为，也可能是一次偶发。',
}

/** 一条规则的证据视图。 */
export interface RuleEvidenceView {
  strength: EvidenceStrength
  label: string
  /** 需要多说的一句话；不需要时为空串。 */
  note: string
  /** 形如 `7 个窗口 · 跨 3 天 · 共 412 次`；UNTRACKED / NONE 时为空串。 */
  detail: string
  /** 排序用：证据越弱越小。让"最不该先下发的"排在最前面。 */
  rank: number
}

/**
 * ruleEvidenceView 把一条规则的证据渲染成一个档位。
 *
 * @param evidence 整份预览的证据表；**为 null 或 undefined 表示没在记**，
 *   与"记了但这条规则没出现过"是两件事，因此不能先 `?? {}` 再查。
 */
export function ruleEvidenceView(
  evidence: Record<string, RuleEvidence> | null | undefined,
  namespace: string, workload: string, fingerprint: string,
): RuleEvidenceView {
  if (evidence == null) return view('UNTRACKED', '')

  const e = evidence[evidenceKey(namespace, workload, fingerprint)]
  if (e === undefined || e.windows <= 0) return view('NONE', '')

  // **ESTABLISHED 只认完整窗口。** 一条规则在二十个"证明不了看全"的窗口里
  // 出现过，说明的是"我们看了很多次"，不是"我们看全了"—— 漏看的连接不会进
  // 候选集，覆盖它的规则于是缺席，而缺席的规则会被判「无流量、可收紧」。
  // 用总窗口数升到最高档，等于把这种情况标成"证据充分"。
  const strength: EvidenceStrength =
    e.completeWindows >= ESTABLISHED_WINDOWS ? 'ESTABLISHED'
      : e.windows >= GROWING_WINDOWS ? 'GROWING'
        : 'THIN'
  return view(strength, evidenceDetail(e))
}

function view(strength: EvidenceStrength, detail: string): RuleEvidenceView {
  const rank: Record<EvidenceStrength, number> = {
    // UNTRACKED 排在最前面，与最弱的证据同侧：两者的处置是同一句
    // "先别急着下发"，而把"我们没在记"排到末尾会让它看起来像已经查过了。
    UNTRACKED: 0, NONE: 1, THIN: 2, GROWING: 3, ESTABLISHED: 4,
  }
  return {
    strength,
    label: STRENGTH_LABEL[strength],
    note: STRENGTH_NOTE[strength] ?? '',
    detail,
    rank: rank[strength],
  }
}

/**
 * evidenceDetail 把证据渲染成一行数字。
 *
 * 三个数并列而不是合成一个"证据分"：一个窗口里刷了十万次的规则与十个窗口里
 * 每次都出现几条的规则，一个合并出来的分数会把两者压成同一个值，而前者一次
 * 压测就能造出来。
 */
export function evidenceDetail(e: RuleEvidence): string {
  // 完整窗口数**恒显示**，包括 0：一句"其中 0 个完整"读起来刺眼，而它
  // 正是要传达的意思；缺席则会被读成"没有这个问题"。
  const parts = [`${e.windows} 个窗口（其中 ${e.completeWindows} 个完整）`]
  const span = observedSpan(e)
  if (span) parts.push(`跨 ${span}`)
  parts.push(`共 ${e.observations} 次`)
  return parts.join(' · ')
}

/**
 * observedSpan 返回首末观测之间的跨度文案；不合法时为空串。
 *
 * 时间来自窗口边界，因此顺序颠倒或无法解析都说明数据本身有问题 —— 这时
 * 宁可不显示，也不显示一个负数或 `NaN 天`：一个显然错误的数字会被当成真的读
 * （与 collectionDuration 同一条处置）。
 */
export function observedSpan(e: RuleEvidence): string {
  const ms = new Date(e.lastSeen).getTime() - new Date(e.firstSeen).getTime()
  if (!Number.isFinite(ms) || ms < 0) return ''
  const hours = ms / 3_600_000
  if (hours < 1) return `${Math.round(ms / 60_000)} 分钟`
  if (hours < 48) return `${Math.round(hours)} 小时`
  return `${Math.round(hours / 24)} 天`
}

/**
 * previewEvidenceNote 是整份预览一级的提示：证据根本没在记时说明这件事。
 *
 * 放在预览一级而不是每条规则上重复一遍：这不是某条规则的性质，是这个集群
 * 的状态，四十条规则各挂一个"未记录"只会把它变成背景噪声。
 */
export function previewEvidenceNote(pv: Pick<PolicyPreview, 'evidence'>): string {
  return pv.evidence == null
    ? '这个集群没有记录跨窗口证据，下面每条规则的流量条数只代表当前这一个窗口。'
    : ''
}
