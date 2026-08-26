import { windowAbsent } from './policyExportView.ts'
import type {
  ChangeKind, DeletionClass, PolicyPreview, TimeWindow,
  WritebackDeletion, WritebackExclusion, WritebackPlan,
} from '../api/types'

/**
 * 写回入口的全部取数：能不能走、不能走时的理由、两步各自要请求哪个路径，
 * 以及按钮旁边那句说明这次写回会落到哪里的话。
 *
 * 与 policyExportView 同源、同形状，理由也同一条：路径不由页面拼，也不由
 * api 层自己再选一次时间窗，它只能由这个函数从**页面正在渲染的那一个 pv
 * 对象**里取。写回这一步的后果比导出更硬 —— 出计划与推送是两次请求，服务端
 * 在推送前会拿请求里的时间窗**重新算一遍整份计划**再比指纹
 * （internal/httpapi/writeback_handler.go 的 planWriteback）。两次请求带的
 * 时间窗只要不同，重算出的就是另一份计划，指纹必然对不上；而更坏的方向是
 * 有一天它们"恰好"对得上，那时推出去的是操作者没看过的那一份。
 *
 * **界面上的不可用不是权限，也不是防护。** 服务端对非空 namespace、对零条
 * 启用规则、对仓库级校验结论不是 OK 的绑定、对非管理员、对不带指纹的推送
 * 一律拒绝，那几道判定是权威的（规范 §26、§34）。这里提前把原因说出来，
 * 只是因为让人点一下才知道"不行"教不会他下一步该做什么。
 */
export interface WritebackView {
  /** 可以出计划。false 时按钮禁用，unavailableReason 必然非空。 */
  readonly available: boolean
  /** 不可用的原因，含下一步怎么做；可用时为空串。 */
  readonly unavailableReason: string
  /** 出计划的请求路径，恒由 pv.cluster 与 pv.window 拼出；不可用时为空串。 */
  readonly planPath: string
  /**
   * 推送的请求路径，与 planPath 带**同一段**时间窗。
   *
   * 不可用时为空串——不给一个"禁用了但仍然可用"的路径留在对象里。
   */
  readonly pushPath: string
  /** 按钮旁边那句话：这次写回会落到哪里、以及它还不是一次生效。 */
  readonly note: string
}

/**
 * 推送请求体。**只有两个字段，且都不是数字**（design doc 2026-08-14 §4）：
 * 影响面由平台在写前重算，一个能自述"我这次只会打断 0 条"的调用方等于
 * 自己描述自己那次变更的爆炸半径。
 */
export interface WritebackPushBody {
  readonly branch: string
  readonly fingerprint: string
  /**
   * 操作者确认要删除的路径。
   *
   * 它是这个请求里唯一一项服务端重算不出来的东西 —— 删哪几个是人的决定。
   * 但它不是授权：能不能删由服务端重算出的清单说了算，而指纹钉住的正是
   * "文件 + 这份确认"这一整份计划（design doc 2026-08-24 §4.4）。
   */
  readonly deletions: readonly string[]
}

/** 计划里一条多余文件在界面上的形态。 */
export interface DeletionRow {
  readonly path: string
  readonly class: DeletionClass
  /** 能不能勾选删除。 */
  readonly selectable: boolean
  /** 为什么可以删 / 为什么不给删。**恒非空** —— 一个没有理由的禁用等于坏掉的界面。 */
  readonly reason: string
  /**
   * 删除影响那一行的成品文字；没有计数时为空串。
   *
   * 在视图层渲染而不是让页面再写一份枚举顺序：四类的次序是一条约定，
   * 两处各写一份就会出现同一份计数在两屏上顺序不同。
   */
  readonly impactText: string
}

/** 一类计数在"页面正在显示的那一份"与"计划里重算出的那一份"上的两个取值。 */
export interface CountDriftRow {
  readonly kind: ChangeKind
  /** 页面上的取值；未给出时为 '未给出'，不写成 0。 */
  readonly pageText: string
  /** 计划里重算出的取值，同上。 */
  readonly planText: string
  readonly changed: boolean
}

/** 页面上的四类计数与计划里重算出的那一套之间的差异。 */
export interface WritebackCountDrift {
  /** 有任何一类对不上。true 时 warning 必然非空。 */
  readonly drifted: boolean
  /** 四类齐全、次序固定，一致的那几类也在其中——操作者要能核对全部四个数。 */
  readonly rows: CountDriftRow[]
  /** 不一致时那句必须被看见的话；一致时为空串。 */
  readonly warning: string
}

/** 四类变化的固定次序，与 predict.AllChangeKinds 一致。 */
const CHANGE_KINDS: ChangeKind[] = ['WOULD_BREAK', 'WOULD_OPEN', 'UNCHANGED', 'UNKNOWN']

const CHANGE_KIND_LABEL: Record<ChangeKind, string> = {
  WOULD_BREAK: 'WOULD_BREAK · 会被拦断',
  WOULD_OPEN: 'WOULD_OPEN · 敞口扩大',
  UNCHANGED: 'UNCHANGED · 无变化',
  UNKNOWN: 'UNKNOWN · 无法判定',
}

/**
 * 两个端点的请求路径。
 *
 * 时间窗必须逐字来自页面正在显示的那次预览，两步也必须是同一段：服务端在
 * 推送前会用请求里的时间窗重新算一遍计划再比指纹。这里让两条路径由同一个
 * 函数、同一个 window 拼出，"两步带了不同的窗"就不是一个能写出来的状态。
 *
 * 不带 namespace 参数：服务端对非空 namespace 一律拒绝（writebackNamespaceMsg），
 * 这个端点唯一成立的取值就是"没有筛选"。做成可传的参数等于留一条注定失败
 * 的调用路径。
 */
function writebackPath(cluster: string, window: TimeWindow, step: 'plan' | 'push'): string {
  // 窗口缺席就省掉 from/to，理由同导出：回传一个零值时刻会被服务端判成
  // 非法参数，而事实是这个集群还没有流量观测（policyExportView.windowAbsent）。
  if (windowAbsent(window)) {
    return `/api/v1/clusters/${encodeURIComponent(cluster)}/policy-writeback/${step}`
  }
  const p = new URLSearchParams({ from: window.from, to: window.to })
  return `/api/v1/clusters/${encodeURIComponent(cluster)}/policy-writeback/${step}?${p}`
}

/**
 * 取数入口。入参是页面正在渲染的那一次预览，不是它的几个字段——拆成几个
 * 参数就又给了调用方一次"从别处取时间窗"的机会。
 */
export function writebackView(pv: PolicyPreview): WritebackView {
  const note = '写回只推一条新分支 distill/<集群>-<时间戳>，不推部署分支，也不删仓库里的任何文件；'
    + '合并与生效由人在合并请求上完成（design doc §2、§3）。'
    + '计划里的四类计数由平台在写之前重新算过，与上面这几个数字不是同一次计算。'

  // 命名空间筛选优先于其它判定，与服务端同序（planWriteback 在读集群之前
  // 就拒绝它），也与导出同源：预测恒按整集群跑，一份被裁剪的策略集配上
  // 整集群口径的计数，描述的是另一套东西，且朝着让人放心的方向偏。
  if (pv.namespace !== '') {
    return {
      available: false,
      unavailableReason:
        `当前按命名空间「${pv.namespace}」筛选，写回不可用。dry-run 预测恒按整集群计算，`
        + '一份被裁剪的策略集会带着一个把它里面根本没有的规则也算进去的结论推进仓库。'
        + '清除命名空间筛选后即可为整集群出一份写回计划。',
      planPath: '',
      pushPath: '',
      note,
    }
  }

  return {
    available: true,
    unavailableReason: '',
    planPath: writebackPath(pv.cluster, pv.window, 'plan'),
    pushPath: writebackPath(pv.cluster, pv.window, 'push'),
    note,
  }
}

/**
 * 推送请求体：**没有计划就没有可推送的东西**。
 *
 * 这一层不是那道门，服务端才是（不带指纹的请求被 writebackNoFingerprintMsg
 * 拒绝，指纹对不上被 writebackStaleMsg 拒绝）。它存在的理由是让"在读过计划
 * 之前就能推"这件事在界面上写不出来：页面渲染推送按钮的条件就是这个函数
 * 返回了非 null，而它只可能由一份带着指纹的计划产出。
 *
 * 指纹为空串时同样返回 null：一次不带指纹的推送在服务端只会被拒绝，发出去
 * 只是把一条注定失败的调用摆在操作者面前，而失败信息会盖住真正的原因。
 */
export function writebackPushBody(
  plan: WritebackPlan | null, confirmed: readonly string[] = [],
): WritebackPushBody | null {
  if (plan === null) return null
  if (plan.branch.trim() === '' || plan.fingerprint.trim() === '') return null
  // 只带平台在这份计划里标为可处置的那几条。多带的会被服务端拒（判据在
  // registry.NewWritebackPlan），在这一层先滤掉是为了让"勾了一个平台没提供
  // 的路径"这件事在界面上写不出来 —— 而不是替服务端做判断。
  const offered = new Set(
    (plan.deletions ?? []).filter(d => selectableClass(d.class)).map(d => d.path),
  )
  return {
    branch: plan.branch,
    fingerprint: plan.fingerprint,
    deletions: confirmed.filter(p => offered.has(p)),
  }
}

/**
 * 把计划里的多余文件清单渲染成界面要的那几项（design doc 2026-08-24 §4.2）。
 *
 * 四类都显示，但只有两类给勾选框：平台看不懂的文件（UNPARSEABLE）与影响
 * 算不出来的文件（IMPACT_UNKNOWN）不提供删除 —— 一个平台连"删了会怎样"
 * 都答不出来的文件，删除它的后果没有人评估过。
 *
 * 每一行都带一句为什么。一个禁用的勾选框配一句空理由，在界面上读起来是
 * "这里坏了"，而它其实是一条结论。
 */
export function deletionRows(deletions: readonly WritebackDeletion[] | null): DeletionRow[] {
  return (deletions ?? []).map(d => ({
    path: d.path,
    class: d.class,
    selectable: selectableClass(d.class),
    reason: DELETION_REASON[d.class] ?? '平台没有认出这个处置分类，因此不提供删除。',
    impactText: d.counts === null
      ? ''
      : CHANGE_KINDS.map(k => `${k} ${d.counts?.[k] ?? 0}`).join('　'),
  }))
}

/** 一条排除在界面上要显示的东西。 */
export interface ExclusionRow {
  /** 主体的写法：namespace 粒度下就是 namespace 本身。 */
  label: string
  /** 分歧率，形如 `20%`。 */
  rateText: string
}

/**
 * 把计划里的排除清单渲染成界面要的那几项
 * （design doc 2026-08-25-trust-engineering §3.4）。
 *
 * **零条时也要有话说**，由调用方渲染 EXCLUSION_NONE：一个空区块与"这一栏
 * 不存在"在屏幕上长得一样，而"没有任何主体被排除"是一条结论，值得占一行。
 *
 * 分歧率取整到百分点显示，但**不做四舍五入到 0**：6% 与 90% 都超阈，前者
 * 值得去看明细、后者说明平台在这个主体上基本不成立。
 */
export function exclusionRows(
  exclusions: readonly WritebackExclusion[] | null,
): ExclusionRow[] {
  return (exclusions ?? []).map(e => ({
    label: e.workload === '' ? e.namespace : `${e.namespace}/${e.workload}`,
    rateText: `${Math.round(e.underPermissiveRate * 100)}%`,
  }))
}

/**
 * 排除区块的说明。
 *
 * 必须说清三件事：为什么排除、为什么 dry-run 看不出来、这些主体的既有策略
 * 会怎样。少了第三条，读的人会以为平台把它们的策略删了。
 */
export const EXCLUSION_HELP =
  '这些主体上，平台判定与集群实际执行对不上，且分歧落在会造成阻断的那一侧'
  + '（平台判 DENY、集群实际放行）。它们的候选规则很可能缺了现在正通着的放行，'
  + '而 dry-run 看不出来 —— 在平台的世界里那些连接本来就不通。'
  + '本次不写它们，既有的策略文件也保持不动（既不更新也不删除）。'
  + '先去质量页看分歧明细，确认平台漏了什么再回来。'

/** 一条都没有时显示的话。空区块读起来像"这一栏坏了"，而它是一条结论。 */
export const EXCLUSION_NONE = '没有主体因判定分歧被排除，本次候选集覆盖全部主体。'

/** 哪几类允许被勾选删除。与服务端 registry.DeletionClass.confirmable 同一套判据。 */
function selectableClass(c: DeletionClass): boolean {
  return c === 'DELETABLE' || c === 'NOT_APPLIED'
}

/** 每一类的处置说明。封闭枚举，逐条写死 —— 它是操作者决定删不删的依据。 */
const DELETION_REASON: Record<DeletionClass, string> = {
  DELETABLE: '本次候选集不再包含它。删除影响已经算出来，见右侧四类计数。',
  NOT_APPLIED: '仓库里有、集群里没有：这些策略从没被应用，或已被人手工删掉。删掉它对集群没有影响。',
  IMPACT_UNKNOWN: '平台算不出删掉它的影响：这段时间窗里没有足够观测，'
    + '或者它是在这段窗口之后才被下发到集群的（GitOps 下的常见时序）。'
    + '此时重放算出来的会是「删掉一个当时并不存在的东西」，恒为无变化 —— '
    + '那是一个朝让人放心的方向错的数字。先重新采集一轮，再出一次计划。',
  UNPARSEABLE: '平台没能把它解析成 NetworkPolicy（别人放的文件、模板，或被改坏的策略）。永不提供删除。',
}

/**
 * 页面上正在显示的四类计数与计划里重算出的那一套之间的比对。
 *
 * **这是写回这一步的全部意义所在**（design doc §4）：操作者确认规则的时刻
 * 与推送的时刻之间，集群、流量窗口、别人的确认都可能变了。悄悄把新数字
 * 渲染在旧数字的位置上，等于让人批准一个他从没考虑过的爆炸半径。
 *
 * 页面这一侧取的必须是 overridden 那一套——服务端算计划用的正是它
 * （writebackCounts 取 pv.Overridden.Prediction.Counts）。拿默认推荐那一套来
 * 比，会在"人工决定改变了预测"时报出一个与集群无关的假差异，而假差异重复
 * 几次之后，真的那次也不会有人看。
 *
 * 任一侧缺了某一类不按 0 处理，而是当作不一致并显示"未给出"：缺一类不是
 * 那一类为零，是没算过那一类（CLAUDE.md §3 同一条纪律）。
 */
export function writebackCountDrift(
  pageCounts: Record<ChangeKind, number>, planCounts: Record<ChangeKind, number>,
): WritebackCountDrift {
  const rows: CountDriftRow[] = []
  const changed: string[] = []
  for (const kind of CHANGE_KINDS) {
    const page = countOf(pageCounts, kind)
    const plan = countOf(planCounts, kind)
    const differs = page === null || plan === null || page !== plan
    rows.push({
      kind,
      pageText: formatCount(page),
      planText: formatCount(plan),
      changed: differs,
    })
    if (differs) changed.push(`${CHANGE_KIND_LABEL[kind]} ${formatCount(page)} → ${formatCount(plan)}`)
  }

  if (changed.length === 0) {
    return { drifted: false, rows, warning: '' }
  }
  return {
    drifted: true,
    rows,
    warning: `平台在写回前重算的四类计数与本页正在显示的不一致：${changed.join('；')}。`
      + '这意味着在你确认这些规则之后，集群、时间窗或其他人的确认发生了变化——'
      + '推送出去的影响面是计划里这一套数字，不是上面那一套。'
      + '先按新的数字重新判断这次变更，必要时刷新本页重新核对规则，再决定推不推。',
  }
}

/** 取一类计数；不是有限数（缺键、null、NaN）时返回 null，交由调用方当作"没算过"。 */
function countOf(counts: Record<ChangeKind, number>, kind: ChangeKind): number | null {
  const v = counts?.[kind]
  return typeof v === 'number' && Number.isFinite(v) ? v : null
}

function formatCount(n: number | null): string {
  return n === null ? '未给出' : String(n)
}
