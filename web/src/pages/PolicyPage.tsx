import { useState, type CSSProperties, type ReactNode } from 'react'
import { EVIDENCE_LABEL, evidenceNote } from './evidenceView.ts'
import { api, ApiError } from '../api/client'
import {
  RISK_CATEGORY_LABEL,
  type CandidatePolicy, type CandidateRule, type ChangeKind, type ExcludedWorkload,
  type Granularity, type Kind, type MissingBaseline, type OverrideDecision, type Widening,
  type RuleOrigin, type RuleOverride, type StaleOverride,
  type UngeneratableItem, type UngeneratableReason, type WorkloadExclusionReason,
  type WritebackPlanResult, type WritebackPushResult,
} from '../api/types'
import { useResource } from '../api/useResource'
import DataSourceNotice from '../components/DataSourceNotice'
import { DryRunDetail } from './DryRunDetail'
import { dryRunView, type DryRunView } from './dryRunView'
import { policyExportView, type PolicyExportView } from './policyExportView'
import { baselineGapViews, notApplicableNote, notAssessedNote, wouldBreakQualifierFor } from './preconditionsView'
import { ALL_GRANULARITIES, granularityView, wideningNote } from './granularityView'
import { Disclosure, Segmented } from '../components/radix'
import {
  writebackCountDrift, writebackPushBody, writebackView,
  type WritebackPushBody, type WritebackView,
} from './writebackView'
import { Card, Chip, EmptyState, Notice, PageHeader, ScrollTableCard, Section, Skeleton, StatTile, StickyHead, TableCard } from '../components/ui'

/**
 * 不可生成原因的中文标签。只在本页使用，未像 unknownReason / RiskCategory
 * 那样出现在第二个页面，因此不进 types.ts 共享表 —— 真正出现复用需求时
 * 再提取（同 CLAUDE.md「依赖引入时机」的原则：需要时再抽，不预先抽象）。
 */
const UNGENERATABLE_REASON_LABEL: Record<UngeneratableReason, string> = {
  NO_WORKLOAD_LABEL: '缺少工作负载标签，podSelector 无法表达',
  IDENTITY_UNKNOWN: '端点身份无法确定',
  DEGRADED_EVIDENCE: '证据被降级，不可作为策略推荐依据',
  UNMANAGED_ENDPOINT: '对端不受 NetworkPolicy 管控',
  LABEL_KEY_CONFLICT: '同名 workload 挂在优先级更低的标签键上，候选策略的 podSelector 选不中它',
}

/**
 * 工作负载被排除在花名册之外的原因的中文标签。
 *
 * 键类型是封闭枚举而不是 string：后端新增一个排除原因却忘了在这里补一
 * 条文案，`tsc` 会直接报错，而不是让界面渲染出一个空白的原因列——一个
 * 空白的原因等于告诉运维「它没被覆盖，但我不告诉你为什么」。
 */
const WORKLOAD_EXCLUSION_REASON_LABEL: Record<WorkloadExclusionReason, string> = {
  UNMANAGED_ENDPOINT: 'Pod 使用 hostNetwork，NetworkPolicy 本身管不到它',
  NO_WORKLOAD_LABEL: '缺少可识别的 workload 标签，podSelector 无法表达这个 Pod',
  LABEL_KEY_CONFLICT: '同名 workload 另有优先级更高的归属标签键，赢家的 podSelector 选不中这个 Pod',
}

export default function PolicyPage({ cluster }: { cluster: string }) {
  // 集群的接入状态决定这一整屏该展示什么，而不是"候选策略查询是否
  // 返回了行"——REGISTERED 意味着平台还没采集到任何流量，此时候选
  // 策略永远是空的，但那是"还没有数据"而不是"查过、确实没有"，两者
  // 在界面上必须可区分（同 EmptyState 的纪律），因此整页替换而不是
  // 让下面的表格自己空着。
  const { data: clusters } = useResource('registered-clusters', () => api.clusters())
  const current = clusters?.find((c) => c.id === cluster)

  // refreshKey 驱动确认/撤销之后的重新拉取——服务端是人工决定的唯一
  // 真相源，本页不在本地叠加一份乐观状态（与 ClustersPage 的 refreshKey
  // 同一条纪律：写操作成功后自增，让 useResource 重新发请求）。
  const [refreshKey, setRefreshKey] = useState(0)
  const onChanged = () => setRefreshKey((k) => k + 1)

  // 粒度是一次**重新取数**，不是本地过滤：两个粒度是两批不同的策略，
  // 各自带着自己那一套 dry-run。在前端折叠会让屏幕上的策略配上另一套
  // 数字，而那个方向偏在让人放心的一侧（粗化只会放宽，拦断更少）。
  const [granularity, setGranularity] = useState<Granularity>('NAMESPACE')

  // cluster 为空串时 key 必须也是空串——useResource 把空 key 当作"还没有
  // 可查询的目标"，直接跳过请求（见 useResource 的 `if (!key)` 分支）。
  // 拼上 refreshKey 之前先判空，否则 `":0"` 这类非空字符串会绕过那道
  // 门禁，在集群尚未选定时就拿着空 clusterID 发一次注定失败的请求。
  const { data: pv, error, loading } = useResource(
    cluster ? `${cluster}:${refreshKey}:${granularity}` : '',
    () => api.policyPreview(cluster, undefined, granularity),
  )

  // 标题与数据来源一起提到早退分支之前，理由见 DataSourceNotice：来源标识
  // 必须与内容同屏，包括这一屏读不到数据的时候（design doc 2026-08-17 §2）。
  const head = (
    <>
      <PageHeader
        title="候选策略"
        description="dry-run 预测置顶：先看这条推荐会拦掉多少条当前正在工作的连接，再看策略本身长什么样。顺序即优先级。"
      />
      <DataSourceNotice />
    </>
  )

  if (current?.state === 'REGISTERED') return <NoTrafficNotice />

  if (error) return <div>{head}<p className="text-deny">{error}</p></div>
  if (loading || !pv) return <div>{head}<Skeleton /></div>

  // 零条覆盖时后端把 overrides 序列化成 null（见 types.ts 里的注释）——
  // 在这唯一一处兜底成 []，下游所有组件都能假设它是数组，不必每处重复判空。
  const overrides = pv.overrides ?? []

  return (
    <div>
      {head}

      {/* 粒度切换放在最前：它决定下面每一块内容 —— 策略、dry-run、导出。
          放在策略那一节里面会让人先读完 dry-run 才发现那是另一个粒度的数字。 */}
      <GranularitySection
        current={granularity} onPick={setGranularity}
        echoed={pv.granularity} widening={pv.widening} count={pv.candidates.length}
      />

      {/* 两套预测在整个前端只在这一处同时出现：dryRunView 收下它们，
          往下传的是一个已经选定的视图。哪一套该被强调、哪一套该出现在
          明细里，从这一行之后就不再是一个可以答错的问题。 */}
      <DryRunSection
        view={dryRunView(pv.prediction, pv.overridden.prediction, overrides.length)}
        overrideCount={overrides.length}
        // 窗口完整度是后端说出来的事实，页面不再拿 degradedCount ===
        // totalEvaluated 反推——把推断换成事实正是这个字段存在的理由
        // （design doc 2026-08-17 §5）。
        breakQualifier={wouldBreakQualifierFor(pv.trafficObserved, pv.windowCompleteness)}
        // 导出入口与 dry-run 同屏，取数同样只来自这一个 pv 对象：文件对应的
        // 时间窗与上面那四个数字来自同一次预览响应（design doc §2、§6）。
        exportView={policyExportView(pv)}
        writeback={writebackView(pv)}
        // 比对写回计划里那套重算后的计数时，页面这一侧必须是 overridden——
        // 服务端算计划用的正是它（writebackCounts 取 Overridden.Prediction）。
        // 拿默认推荐那一套去比，会在"人工决定改变了预测"时报出一个与集群
        // 无关的假差异，而假差异重复几次之后，真的那次也不会有人看。
        pageCounts={pv.overridden.prediction.counts}
      />
      <CandidateSection candidates={pv.candidates} overrides={overrides} cluster={cluster} onChanged={onChanged} />
      <PendingSection candidates={pv.candidates} overrides={overrides} cluster={cluster} onChanged={onChanged} />
      <StaleOverridesSection staleOverrides={pv.staleOverrides} />
      <MissingBaselineSection
        missing={pv.missingBaselines}
        baselineKinds={pv.baselineKinds}
        // 未评估是叠加在缺失清单上的标注，不是第二份清单：它改的是处置
        // （去修采集，而不是去写策略），不改门禁（design doc §4）。
        notAssessed={pv.notAssessedBaselines}
        notApplicable={pv.notApplicableBaselines}
      />
      <ExcludedWorkloadSection items={pv.excludedWorkloads ?? []} />
      <UngeneratableSection items={pv.ungeneratable} />
    </div>
  )
}

/**
 * REGISTERED 集群的整页替换态：一个空表格与"我们还没有数据"是两个
 * 不同的断言，这个平台在别处（缺失 baseline 清单、不可生成清单）已经
 * 把这条纪律当作硬约束，这里同样适用——不展示任何空表格。
 */
function NoTrafficNotice() {
  return (
    <div>
      <PageHeader
        title="候选策略"
        description="dry-run 预测置顶：先看这条推荐会拦掉多少条当前正在工作的连接，再看策略本身长什么样。顺序即优先级。"
      />
      {/* 整页替换态同样要说清来源：一个「尚未采集到流量」的演示集群与一个
          真集群，下一步动作完全不同（design doc 2026-08-17 §2）。 */}
      <DataSourceNotice />

      <Notice>尚未采集到流量，无法产出候选策略。</Notice>

      <Card className="mb-4 p-4">
        <p className="mt-0 mb-2 text-sm">
          候选规则来自对观测流量的学习。当前该集群只登记了元数据，还差：
        </p>
        <ul className="m-0 pl-[1.2em] text-sm">
          <li>流量日志尚未开启</li>
          <li>采集器尚未部署</li>
        </ul>
      </Card>

      <Card className="p-4">
        <p className="m-0 text-sm">
          已可用的部分：五类必备 Baseline 的推导依据来自资产快照，不依赖流量。
        </p>
      </Card>
    </div>
  )
}

/* ---------------------------------------------------------------------- */
/* 1. dry-run 影响                                                        */
/* ---------------------------------------------------------------------- */

/**
 * dry-run 影响：默认推荐 vs 应用人工决定之后的版本。
 *
 * 无覆盖时（overrideCount === 0）两套预测在结构上恒等——store 层的
 * Apply 对空覆盖列表是恒等变换——此时只显示一组数：否则每个集群第一次
 * 打开都要看一堆 `→ 0`，噪声掩盖了真正有覆盖时该看的差值。
 */
function DryRunSection({ view, overrideCount, breakQualifier, exportView, writeback, pageCounts }: {
  view: DryRunView
  overrideCount: number
  /** 窗口非 COMPLETE 时对 WOULD_BREAK 的限定语；完整时为空串。 */
  breakQualifier: string
  exportView: PolicyExportView
  writeback: WritebackView
  pageCounts: Record<ChangeKind, number>
}) {
  // 这个组件拿不到两套预测，只拿得到 dryRunView 选定后的视图：tile 的
  // 两端与明细区因此不可能来自不同的预测。C1 那条缺陷（清单列 81 行、
  // 正上方的 tile 写着 78）在这里不是"被测试盯着"，是写不出来。
  const c = view.baseline
  const o = view.emphasized
  const showDelta = view.showDelta

  const breakDelta = o.WOULD_BREAK - c.WOULD_BREAK
  const openDelta = o.WOULD_OPEN - c.WOULD_OPEN
  const unchangedDelta = o.UNCHANGED - c.UNCHANGED
  const unknownDelta = o.UNKNOWN - c.UNKNOWN
  const allZero = breakDelta === 0 && openDelta === 0 && unchangedDelta === 0 && unknownDelta === 0

  return (
    <Section
      title="dry-run 影响"
      description="按当前候选策略重放同一段观测流量得到的四类变化。WOULD_BREAK 是本页最重要的数字——它是这条推荐一旦下发会拦断的、当前正在工作的连接数。"
    >
      {/* 限定语在四个 tile 之前，不在下方脚注里：它说的是紧接着那个大字
          该怎么读——「候选集为空时有多少连接被 Baseline 拦下」，不是
          「上线会打断多少条」。读完数字才读到限定语，等于没有限定语
          （design doc 2026-08-17 §5）。 */}
      {breakQualifier !== '' && <Notice>{breakQualifier}</Notice>}

      {showDelta && !allZero && (
        <Notice>{summarizeDeltas(breakDelta, openDelta, overrideCount)}</Notice>
      )}
      {showDelta && allZero && (
        <Notice>
          已记录 {overrideCount} 次人工决定，但应用后 dry-run 预测的四类计数与默认推荐完全一致。
          常见原因：这条连接属于「镜像对」——集群内连接会拆成源端 egress 与目的端 ingress
          两条独立规则，NetworkPolicy 要求两端都放行才会真正生效，单独确认一侧不会改变判定。
          到下方「待确认规则」核对被标记「镜像对待确认」的行，把另一侧也确认掉。
        </Notice>
      )}

      <div className="mb-4 grid grid-cols-[repeat(auto-fit,minmax(200px,1fr))] gap-3">
        <DryRunMetric
          label="WOULD_BREAK · 会被拦断" defaultValue={c.WOULD_BREAK} overriddenValue={o.WOULD_BREAK}
          tone="deny" size="lg" showDelta={showDelta} note="当前放行、新策略会拒绝的连接"
        />
        <DryRunMetric
          label="WOULD_OPEN · 敞口扩大" defaultValue={c.WOULD_OPEN} overriddenValue={o.WOULD_OPEN}
          tone="unknown" size="lg" showDelta={showDelta}
          note="当前拒绝、新策略会放行 —— 不是好消息，增量与 WOULD_BREAK 的减量同等显著"
        />
        <DryRunMetric
          label="UNCHANGED · 无变化" defaultValue={c.UNCHANGED} overriddenValue={o.UNCHANGED}
          showDelta={showDelta} note="两侧都判得出、且结论一致"
        />
        <DryRunMetric
          label="UNKNOWN · 无法判定" defaultValue={c.UNKNOWN} overriddenValue={o.UNKNOWN}
          tone="unknown" showDelta={showDelta} note="当前判定或新策略判定有一侧给不出结论"
        />
      </div>

      {/* 导出入口紧跟四个 tile，不放页面底部：操作者刚读完"会拦断 81 条"，
          文件与它对应的那个结论要在视觉上绑在一起（design doc §6）。 */}
      <ExportControl view={exportView} />

      {/* 写回入口紧挨导出：推进仓库的内容与刚下载的那份文件逐字节相同
          （服务端 planWriteback 与导出共用同一段渲染），两个出口放在一起，
          操作者才看得出它们是同一份东西的两条去路（design doc §7）。 */}
      <WritebackControl view={writeback} pageCounts={pageCounts} />

      {showDelta && (
        <p className="mt-0 mb-2 text-xs text-ink-muted">
          {view.detail.basis}
        </p>
      )}

      <DryRunDetail view={view.detail} />
    </Section>
  )
}

/**
 * 把默认推荐与人工版本的差值拼成一句话，两个方向都提。
 *
 * 只有前者显眼时人的直觉是"数字变好了"——而变好的原因恰恰是刚放开了
 * 一个已知敞口。DISABLE 覆盖会反向移动这两个数（WOULD_BREAK 增、
 * WOULD_OPEN 减），因此四种符号组合都要覆盖，不能假设只有"启用风险
 * 规则"这一种操作方向。
 */
function summarizeDeltas(breakDelta: number, openDelta: number, overrideCount: number): string {
  const parts: string[] = []
  if (breakDelta < 0) parts.push(`让 ${-breakDelta} 条连接不再被拦断`)
  else if (breakDelta > 0) parts.push(`让 ${breakDelta} 条此前放行的连接被拦断`)
  if (openDelta > 0) parts.push(`放开了 ${openDelta} 条当前被拒绝的连接`)
  else if (openDelta < 0) parts.push(`收紧了 ${-openDelta} 条此前被放行的连接`)
  if (parts.length === 0) return `你的 ${overrideCount} 次人工决定没有改变会被拦断或放开的连接数。`
  return `你的 ${overrideCount} 次人工决定，${parts.join('，同时')}。`
}

/**
 * dry-run 单项指标：无覆盖时退化为原有的单值 StatTile；有覆盖时并列
 * 展示"默认 → 人工版本"与差值。
 *
 * WOULD_OPEN 与 WOULD_BREAK 用同一个 size='lg'，让敞口扩大的增量与
 * 拦断减少的降幅同等显著——这是 spec 的硬约束，不是排版偏好：只放大
 * WOULD_BREAK 会让人只看见"数字变好了"，看不见代价。
 */
function DryRunMetric({
  label, defaultValue, overriddenValue, tone, size, note, showDelta,
}: {
  label: string
  defaultValue: number
  overriddenValue: number
  tone?: 'unknown' | 'degraded' | 'deny'
  size?: 'lg'
  note?: ReactNode
  showDelta: boolean
}) {
  if (!showDelta) {
    return <StatTile label={label} value={String(defaultValue)} tone={tone} size={size} note={note} />
  }

  const delta = overriddenValue - defaultValue
  const deltaLabel = delta === 0 ? '±0' : delta > 0 ? `+${delta}` : String(delta)
  const accent =
    tone === 'unknown' ? 'var(--verdict-unknown)'
      : tone === 'degraded' ? 'var(--degraded-stroke)'
        : tone === 'deny' ? 'var(--verdict-deny)'
          : undefined

  return (
    <Card style={{ padding: 'var(--space-3)', borderLeft: accent ? `3px solid ${accent}` : undefined }}>
      <div className="text-xs text-ink-muted">{label}</div>
      <div className="mt-1 flex flex-wrap items-baseline gap-2">
        <span style={{
          fontSize: size === 'lg' ? 'var(--text-xl)' : 'var(--text-lg)', fontWeight: 500,
          color: 'var(--text-muted)', fontVariantNumeric: 'tabular-nums',
        }}>
          {defaultValue}
        </span>
        <span className="text-ink-muted">→</span>
        <span style={{
          fontSize: size === 'lg' ? 'var(--text-2xl)' : 'var(--text-xl)', fontWeight: 600,
          color: accent ?? 'var(--text)', fontVariantNumeric: 'tabular-nums',
        }}>
          {overriddenValue}
        </span>
        <span style={{ fontSize: 'var(--text-sm)', fontWeight: 600, color: accent ?? 'var(--text-muted)' }}>
          {deltaLabel}
        </span>
      </div>
      {note && (
        <div style={{ fontSize: 'var(--text-xs)', color: 'var(--text-muted)', marginTop: 'var(--space-1)' }}>
          {note}
        </div>
      )}
    </Card>
  )
}

/**
 * 导出已确认的策略。
 *
 * **禁用不是保护。** 服务端独立判定管理员权限、拒绝按命名空间筛选的导出、
 * 拒绝零条启用规则的导出，那三道判定是权威的，这里少画一个按钮改变不了
 * 任何一条（规范 §34）。这个控件存在的理由只有一个：让操作者在按下去
 * 之前就知道为什么不行、以及下一步该做什么——一个点了才失败的按钮什么
 * 也没教会他。
 *
 * 文件内容整份来自服务端的响应体：注释头里的集群、时间窗、四类计数、
 * 导出者与导出时刻都不在前端重建（design doc §2、§3）。前端重建一份，
 * "文件与页面上的预测同源"这个保证当场就没了。
 */
function ExportControl({ view }: { view: PolicyExportView }) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  async function download() {
    setBusy(true)
    setError('')
    try {
      const file = await api.policyExport(view.path)
      // Blob URL + <a download>：把服务端那几十 KB 字节原样落到磁盘。
      // 不提供"复制到剪贴板"作为唯一出口——大文件复制会静默截断，
      // 而且不留任何痕迹（design doc §6）。
      const url = URL.createObjectURL(file.blob)
      const a = document.createElement('a')
      a.href = url
      a.download = file.filename
      document.body.appendChild(a)
      a.click()
      a.remove()
      // 延后回收：点击只是把下载排进队列，立刻 revoke 在部分浏览器上会
      // 让这次下载中途失败，而失败的形状是"什么都没发生"。
      setTimeout(() => URL.revokeObjectURL(url), 10_000)
    } catch (err) {
      // 服务端的拒绝原样展示（emptyExportMsg / namespacedExportMsg 都是
      // 写给操作者看的完整理由），不收窄成一句"导出失败"。
      setError(err instanceof ApiError ? err.msg : '导出失败，请稍后重试')
    } finally {
      setBusy(false)
    }
  }

  return (
    <Card className="mb-4 p-3">
      <div className="flex flex-wrap items-center gap-3">
        <button
          type="button"
          onClick={download}
          disabled={!view.available || busy}
          style={{
            ...smallButtonStyle,
            opacity: !view.available || busy ? 0.5 : 1,
            cursor: !view.available || busy ? 'default' : 'pointer',
          }}
        >
          {busy ? '导出中…' : '下载 NetworkPolicy YAML'}
        </button>
        {/* 不可用的原因与按钮同屏、不折进 tooltip：它是一条处置指引，
            不是补充说明。理由为空的禁用等于"按钮坏了"。 */}
        {!view.available && (
          <span className="min-w-[280px] flex-1 text-sm text-ink-2">
            {view.unavailableReason}
          </span>
        )}
      </div>
      <p className="mt-2 mb-0 text-xs text-ink-muted">
        {view.windowNote}
      </p>
      {error && (
        <p role="alert" className="mt-2 mb-0 text-xs text-deny">
          {error}
        </p>
      )}
    </Card>
  )
}

/**
 * 把已确认的策略写回策略仓库的一条新分支。
 *
 * **两步，且第二步的控件在第一步完成之前根本不存在**：先出计划——将要新增
 * 或更新的文件、目标分支、写回前重算的四类计数、仓库里多余文件的清单，以及
 * 那段会永久留在仓库历史里、评审人唯一会读的提交信息——操作者看过之后，
 * 推送按钮才出现（design doc 2026-08-14 §4、§5）。
 *
 * **界面上少画一个按钮不是保护。** 服务端独立判定管理员权限、拒绝不带指纹的
 * 推送、拒绝指纹对不上的推送、拒绝仓库级校验结论不是 OK 的绑定，那几道判定
 * 是权威的（规范 §26、§34）。这里的两步存在的理由只有一个：让操作者在按下去
 * 之前真的读过他要推的那份东西。
 *
 * 计划里的每一项都原样来自服务端：文件清单、分支、提交信息、四类计数，前端
 * 一样都不重新推导（design doc §4）。**文件内容不展示**——要看内容走导出，
 * 推进仓库的与导出下载的是同一段渲染的产物（规范 §20）。
 */
function WritebackControl({ view, pageCounts }: {
  view: WritebackView
  pageCounts: Record<ChangeKind, number>
}) {
  const [plan, setPlan] = useState<WritebackPlanResult | null>(null)
  const [pushed, setPushed] = useState<WritebackPushResult | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  // 推送按钮的存在条件。计划为 null 时它恒为 null，因此"没出计划就能推"
  // 在这个组件里写不出来（见 writebackPushBody）。
  const pushBody = writebackPushBody(plan?.plan ?? null)
  const drift = plan === null ? null : writebackCountDrift(pageCounts, plan.plan.counts)

  async function loadPlan() {
    setBusy(true)
    setError('')
    setPushed(null)
    try {
      setPlan(await api.policyWritebackPlan(view.planPath))
    } catch (err) {
      // 出计划失败时把上一份计划丢掉：留着它意味着推送按钮还在，而它对应的
      // 是一次操作者已经在试图刷新的、可能已经过期的计划。
      setPlan(null)
      // 服务端的拒绝原样展示（仓库地址不是 SSH 形态、绑定未通过校验、
      // 没有启用中的规则各自是一句写给操作者看的完整理由），不收窄成
      // 一句"出计划失败"——三种情况的下一步动作完全不同。
      setError(err instanceof ApiError ? err.msg : '出计划失败，请稍后重试')
    } finally {
      setBusy(false)
    }
  }

  async function push(body: WritebackPushBody) {
    setBusy(true)
    setError('')
    try {
      setPushed(await api.policyWritebackPush(view.pushPath, body))
      // 一份计划只用一次：推成功之后它描述的事已经发生，留着它就等于允许
      // 对着同一个指纹再推一次。
      setPlan(null)
    } catch (err) {
      // 指纹失效（writebackStaleMsg）说的是"计划变了，你得重新看一遍"，
      // 因此连同计划一起丢掉，逼回第一步——把这条消息吞成通用失败、或者
      // 把按钮留在原地让人再点一次，都是在教操作者忽略它。
      setPlan(null)
      setError(err instanceof ApiError ? err.msg : '推送失败，请稍后重试')
    } finally {
      setBusy(false)
    }
  }

  return (
    <Card className="mb-4 p-3">
      <div className="flex flex-wrap items-center gap-3">
        <button
          type="button"
          onClick={loadPlan}
          disabled={!view.available || busy}
          style={{
            ...smallButtonStyle,
            opacity: !view.available || busy ? 0.5 : 1,
            cursor: !view.available || busy ? 'default' : 'pointer',
          }}
        >
          {busy ? '处理中…' : '生成写回计划'}
        </button>
        {/* 不可用的原因与按钮同屏，理由同导出：它是一条处置指引，不是补充
            说明。理由为空的禁用等于"按钮坏了"。 */}
        {!view.available && (
          <span className="min-w-[280px] flex-1 text-sm text-ink-2">
            {view.unavailableReason}
          </span>
        )}
      </div>
      <p className="mt-2 mb-0 text-xs text-ink-muted">
        {view.note}
      </p>

      {error && (
        <p role="alert" className="mt-2 mb-0 text-xs text-deny">
          {error}
        </p>
      )}

      {plan && drift && (
        <div className="mt-3">
          {/* 计数不一致时这条必须显著、必须在计划正文之前：把新数字悄悄
              渲染在旧数字的位置上，等于让人批准一个他从没考虑过的爆炸半径
              （design doc §4）。 */}
          {drift.drifted && (
            <div role="alert" style={{
              padding: 'var(--space-3)', marginBottom: 'var(--space-3)',
              background: 'var(--verdict-deny-bg)', border: '1px solid var(--verdict-deny)',
              borderRadius: 'var(--radius)', color: 'var(--verdict-deny)',
              fontSize: 'var(--text-sm)', fontWeight: 500,
            }}>
              {drift.warning}
            </div>
          )}

          {/* 目标仓库与目标分支一起给：仓库进指纹（design doc §4），而进
              指纹的东西必须是操作者屏幕上看得到的东西 —— 否则他确认的是一个
              自己没读过的落点。这里给的是仓库标识，不是仓库地址：地址是内部
              地址，不进任何会被读到的地方。 */}
          <PlanRow label="目标仓库">{plan.plan.repoId}</PlanRow>
          <PlanRow label="目标分支">{plan.plan.branch}</PlanRow>
          <PlanRow label="写回前重新校验">
            仓库级 {plan.repoVerifyResult} · 路径级 {plan.bindingVerifyResult}
          </PlanRow>
          <PlanRow label="将要新增/更新的文件">
            {plan.plan.files.length === 0
              ? '（无）'
              : plan.plan.files.map((f) => <div key={f.path}>{f.path}</div>)}
          </PlanRow>
          {/* 这两份清单都来自平台出计划时**真的枚举过一次仓库**（design doc
              §2、§3）：枚举失败时后端整次不出计划，因此这里渲染的"（无）"是一个
              空集，不是一句没人算过的话。 */}
          <PlanRow label="仓库里多余的文件">
            {(plan.plan.extraneous ?? []).length === 0
              ? '（无）平台从不删除仓库里的文件，多余的文件只列出来交人工处置。'
              : (plan.plan.extraneous ?? []).map((p) => <div key={p}>{p}</div>)}
          </PlanRow>
          {/* 攒着几条没人合的 distill 分支，说明这个流程没在运转（§2）——
              这是唯一能看见"人工合并那道门是否真的有人在走"的信号。
              **合并状态平台没有判断**，必须照实写：判断合并与否要拉全量历史，
              而写回全程只做浅克隆。 */}
          <PlanRow label="仓库上已存在的 distill 分支">
            {(plan.plan.existingBranches ?? []).length === 0
              ? '（无）'
              : (plan.plan.existingBranches ?? []).map((b) => <div key={b}>{b}</div>)}
            <div className="text-ink-muted">
              平台只列出这些分支存在，不判断它们是否已被合并。攒着几条没人合的分支，
              说明这条流程没在运转。
            </div>
          </PlanRow>

          <PlanRow label="重算后的四类计数">
            {/* 走统一的表格样式，不自己拼一份：三个页面三种表格，读者会
                以为它们在讲不同性质的事（ui.tsx 抬头）。这里嵌在计划行内，
                因此只取 .dt 的排版，不再套一层卡片。 */}
            <table className="dt">
              <thead>
                <tr><th>类别</th><th className="num">本页正在显示</th><th className="num">计划重算</th></tr>
              </thead>
              <tbody>
                {drift.rows.map((r) => (
                  <tr key={r.kind} style={{ color: r.changed ? 'var(--verdict-deny)' : undefined }}>
                    <td>{r.kind}</td>
                    <td className="num">{r.pageText}</td>
                    <td className="num" style={{ fontWeight: r.changed ? 600 : undefined }}>
                      {r.planText}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </PlanRow>

          {/* 提交信息必须在推送之前被读到：它是合并请求上的评审人唯一会读
              的那句话，据此决定合不合（design doc §7）。原文展示，不截断、
              不重排。 */}
          <PlanRow label="提交信息（会永久留在仓库历史里）">
            <pre style={{
              margin: 0, whiteSpace: 'pre-wrap', wordBreak: 'break-word',
              fontSize: 'var(--text-xs)', color: 'var(--text-secondary)',
            }}>
              {plan.plan.commitMessage}
            </pre>
          </PlanRow>

          {/* 推送按钮只在有计划时存在。pushBody 由计划产出，指纹原样回带——
              页面不拼请求体，也无从拼出一个指纹。 */}
          {pushBody && (
            <button
              type="button"
              onClick={() => push(pushBody)}
              disabled={busy}
              style={{
                ...smallButtonStyle, marginTop: 'var(--space-2)',
                opacity: busy ? 0.5 : 1, cursor: busy ? 'default' : 'pointer',
              }}
            >
              {busy ? '推送中…' : '推送这份计划到新分支'}
            </button>
          )}
        </div>
      )}

      {pushed && (
        <div style={{ marginTop: 'var(--space-3)', fontSize: 'var(--text-sm)' }}>
          <PlanRow label="已推送到分支">{pushed.branch}</PlanRow>
          <PlanRow label="平台推上去的 commit">{pushed.commit}</PlanRow>
          <PlanRow label="这一步之后">
            仓库里多了一条分支，集群上什么都还没变。需要有人在合并请求上审完并合并，
            Config Sync 才会应用它（design doc §2、§8）。
          </PlanRow>
        </div>
      )}
    </Card>
  )
}

/** 计划里的一项：左侧标签、右侧原样来自服务端的内容。 */
function PlanRow({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="mb-2">
      <div className="text-xs text-ink-muted">{label}</div>
      <div className="text-sm break-all">{children}</div>
    </div>
  )
}

/* ---------------------------------------------------------------------- */
/* 人工决定的共享控件                                                       */
/* ---------------------------------------------------------------------- */

/** 定位一条规则人工决定的复合键：(namespace, workload, fingerprint)。 */
function overrideKey(namespace: string, workload: string, fingerprint: string): string {
  return `${namespace}::${workload}::${fingerprint}`
}

function buildOverrideIndex(overrides: RuleOverride[]): Map<string, RuleOverride> {
  const idx = new Map<string, RuleOverride>()
  for (const o of overrides) idx.set(overrideKey(o.namespace, o.workload, o.fingerprint), o)
  return idx
}

/** 一条规则在应用人工决定之后是否生效——覆盖不存在时退回它自己的默认 enabled。 */
function effectiveEnabled(
  overrideIndex: Map<string, RuleOverride>, namespace: string, workload: string, rule: CandidateRule,
): boolean {
  const o = overrideIndex.get(overrideKey(namespace, workload, rule.fingerprint))
  return o ? o.decision === 'ENABLE' : rule.enabled
}

/**
 * 一条人工决定的操作位：未决定时是"确认启用/确认禁用"按钮 + 必填理由
 * 输入；已有覆盖时委托给 OverrideAppliedRow 展示谁/何时/为什么 + 撤销。
 *
 * 理由为空时提交按钮不可用——这是 reason NOT NULL 在界面上的对应物，
 * 不是排版意义上的 courtesy。
 */
function OverrideControl({
  cluster, namespace, workload, fingerprint, decision, override, onChanged, disabledReason,
}: {
  cluster: string
  namespace: string
  workload: string
  fingerprint: string
  decision: OverrideDecision
  override?: RuleOverride
  onChanged: () => void
  disabledReason?: string
}) {
  const [open, setOpen] = useState(false)
  const [reason, setReason] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  if (override) {
    return <OverrideAppliedRow cluster={cluster} override={override} onChanged={onChanged} />
  }

  if (disabledReason) {
    return (
      <span className="text-xs text-ink-muted" title={disabledReason}>
        不可禁用
      </span>
    )
  }

  if (!open) {
    return (
      <button type="button" onClick={() => setOpen(true)} style={secondarySmallButtonStyle}>
        {decision === 'ENABLE' ? '确认启用' : '确认禁用'}
      </button>
    )
  }

  async function submit() {
    setBusy(true)
    setError('')
    try {
      await api.createOverride(cluster, { namespace, workload, fingerprint, decision, reason: reason.trim() })
      setReason('')
      setOpen(false)
      onChanged()
    } catch (err) {
      // 后端把校验失败的具体字段写进 msg（比如"指纹与当前候选规则不匹配"）——
      // 原样展示，这是轮 1 的 WriteInvalid 存在的理由。
      setError(err instanceof ApiError ? err.msg : '提交失败，请稍后重试')
    } finally {
      setBusy(false)
    }
  }

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 4, minWidth: 240 }}>
      <textarea
        className="ctl"
        value={reason}
        onChange={(e) => setReason(e.target.value)}
        placeholder="理由（必填）——半年后有人会问为什么"
        rows={2}
        style={{
          width: '100%', fontFamily: 'inherit', fontSize: 'var(--text-xs)',
          resize: 'vertical', padding: 6, height: 'auto',
        }}
      />
      {error && <span role="alert" style={{ color: 'var(--verdict-deny)', fontSize: 'var(--text-xs)' }}>{error}</span>}
      <div style={{ display: 'flex', gap: 6 }}>
        {/* 理由为空时禁用：disabled 属性本身已经拦下点击，但按钮外观必须
            跟着变——不透明度不降，操作者会以为按钮坏了而不是"还差一步"。
            与 ClustersPage 的 removeApiServerRow 同一条约定。 */}
        <button
          type="button" onClick={submit} disabled={busy || reason.trim() === ''}
          style={{
            ...smallButtonStyle,
            opacity: busy || reason.trim() === '' ? 0.5 : 1,
            cursor: busy || reason.trim() === '' ? 'default' : 'pointer',
          }}
        >
          {busy ? '提交中…' : '提交'}
        </button>
        <button
          type="button"
          onClick={() => { setOpen(false); setReason(''); setError('') }}
          disabled={busy}
          style={{ ...secondarySmallButtonStyle, opacity: busy ? 0.5 : 1, cursor: busy ? 'default' : 'pointer' }}
        >
          取消
        </button>
      </div>
    </div>
  )
}

/**
 * 已有覆盖的行：谁、何时、什么理由直接显示在行上，不放进 tooltip——
 * 半年后有人问「这条 SSH 出公网为什么是开的」，答案得在他正在看的那一屏。
 * strong 徽标区分「人工启用」与「人工禁用」，同一处渲染同时覆盖两种方向。
 */
function OverrideAppliedRow({ cluster, override, onChanged }: {
  cluster: string
  override: RuleOverride
  onChanged: () => void
}) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  async function revoke() {
    setBusy(true)
    setError('')
    try {
      await api.deleteOverride(cluster, override.namespace, override.workload, override.fingerprint)
      onChanged()
    } catch (err) {
      setError(err instanceof ApiError ? err.msg : '撤销失败，请稍后重试')
    } finally {
      setBusy(false)
    }
  }

  return (
    <div style={{ fontSize: 'var(--text-xs)', minWidth: 200 }}>
      <Chip strong={override.decision === 'ENABLE'}>
        {override.decision === 'ENABLE' ? '人工启用' : '人工禁用'}
      </Chip>
      <div style={{ marginTop: 4, color: 'var(--text-secondary)' }}>
        {override.decidedBy} · {formatTime(override.decidedAt)}
      </div>
      <div className="mt-[2px]">「{override.reason}」</div>
      {error && <div role="alert" style={{ color: 'var(--verdict-deny)', marginTop: 4 }}>{error}</div>}
      <button type="button" onClick={revoke} disabled={busy} style={{ ...secondarySmallButtonStyle, marginTop: 4 }}>
        {busy ? '撤销中…' : '撤销'}
      </button>
    </div>
  )
}

/* ---------------------------------------------------------------------- */
/* 2. 候选策略列表（仅启用规则）                                            */
/* ---------------------------------------------------------------------- */

function CandidateSection({ candidates, overrides, cluster, onChanged }: {
  candidates: CandidatePolicy[]
  overrides: RuleOverride[]
  cluster: string
  onChanged: () => void
}) {
  const overrideIndex = buildOverrideIndex(overrides)
  return (
    <Section
      title="候选策略"
      description="按 namespace/workload 分组，仅展示会被启用的规则。BASELINE 来自基础设施事实推导，LEARNED 来自观测流量学习——两者证据强度不同，徽标视觉可分。待确认（enabled=false）的规则见下一节，不在此处出现；已被人工禁用的规则也移到下一节，同一处能看到全部人工决定。"
      meta={`${candidates.length} 组`}
    >
      {candidates.length === 0 ? (
        <EmptyState message="没有可生成的候选策略。" detail="见下方「不可生成清单」了解原因。" />
      ) : (
        candidates.map((c) => {
          // 默认启用、且没有被人工禁用覆盖的规则——被禁用的移到「待确认
          // 规则」一节，与其它人工决定同屏，不在这里以"仍然启用"的样子留着。
          const enabled = c.rules.filter((r) =>
            r.enabled && overrideIndex.get(overrideKey(c.namespace, c.workload, r.fingerprint))?.decision !== 'DISABLE')
          return (
            <div key={`${c.namespace}/${c.workload}`} className="mb-4">
              <div className="mb-2">
                <strong className="mono text-sm">
                  {c.namespace}/{c.workload}
                </strong>
                <span className="ml-2 text-xs text-ink-muted">
                  {enabled.length} 条启用规则
                </span>
              </div>
              {enabled.length === 0 ? (
                <EmptyState
                  message="该工作负载没有已启用的规则。"
                  detail="全部规则处于待确认状态，见下方「待确认规则」一节。"
                />
              ) : (
                <RuleTable
                  rules={enabled} namespace={c.namespace} workload={c.workload}
                  cluster={cluster} onChanged={onChanged}
                />
              )}
            </div>
          )
        })
      )}
    </Section>
  )
}

function RuleTable({ rules, namespace, workload, cluster, onChanged }: {
  rules: CandidateRule[]
  namespace: string
  workload: string
  cluster: string
  onChanged: () => void
}) {
  return (
    <TableCard>
      <thead>
        <tr>
          <th>来源</th>
          <th>依据</th>
          <th>方向</th>
          <th>对端</th>
          <th>端口</th>
          <th className="num">流量条数</th>
          <th>人工决定</th>
        </tr>
      </thead>
      <tbody>
        {rules.map((r) => (
          <tr key={r.fingerprint}>
            <td><OriginBadge origin={r.origin} /></td>
            <td><RuleBasis rule={r} /></td>
            <td><Chip>{r.direction}</Chip></td>
            <td><RuleTargets values={r.peers} /></td>
            <td><RuleTargets values={r.ports} /></td>
            <td className="num">{r.flowCount}</td>
            <td>
              <OverrideControl
                cluster={cluster} namespace={namespace} workload={workload}
                fingerprint={r.fingerprint} decision="DISABLE" onChanged={onChanged}
                disabledReason={
                  r.origin === 'BASELINE'
                    ? 'BASELINE 规则不接受人工禁用：需要修正其推导依据，而不是在这里覆盖'
                    : undefined
                }
              />
            </td>
          </tr>
        ))}
      </tbody>
    </TableCard>
  )
}

/**
 * 规则的对端与端口。
 *
 * 没有这两列，一条规则在页面上只剩「LEARNED · EGRESS」——读的人分不出
 * payment:8080 与 0.0.0.0/0:443，而后者是一次出公网敞口。因此即使值为空
 * 也要显式写出「未限定」，而不是留一个空单元格：空单元格会被读成"没这项"。
 */
function RuleTargets({ values }: { values: string[] }) {
  if (!values || values.length === 0) {
    return <span className="text-ink-muted">未限定</span>
  }
  return (
    <span className="mono" style={{
      display: 'flex', flexDirection: 'column', gap: 2, fontSize: 'var(--text-sm)',
    }}>
      {values.map((v, i) => <span key={`${i}-${v}`}>{v}</span>)}
    </span>
  )
}

/**
 * BASELINE 用实心徽标、LEARNED 用描边徽标——两者不能借用判定语义色
 * （那是 ALLOW/DENY/UNKNOWN 的专属颜色），因此靠填充与否区分，
 * 而不是靠色相区分。
 */
function OriginBadge({ origin }: { origin: RuleOrigin }) {
  return origin === 'BASELINE' ? <Chip strong>BASELINE</Chip> : <Chip>LEARNED</Chip>
}

/** 规则依据：BASELINE 展示类型与推导来源，LEARNED 展示证据等级与命中的风险端口。 */
function RuleBasis({ rule }: { rule: CandidateRule }) {
  if (rule.baseline) {
    return (
      <div>
        <div>{rule.baseline}</div>
        {rule.derivations && rule.derivations.length > 0 && (
          <div className="mt-[2px] text-xs text-ink-muted">
            {rule.derivations
              .map((d) => `${d.sourceKind}:${d.namespace ? `${d.namespace}/` : ''}${d.name}`)
              .join('、')}
          </div>
        )}
      </div>
    )
  }
  if (rule.evidence) {
    const note = evidenceNote(rule.evidence)
    return (
      <div>
        <div>{EVIDENCE_LABEL[rule.evidence] ?? rule.evidence}</div>
        {note !== '' && (
          <div style={{
            fontSize: 'var(--text-xs)', color: 'var(--verdict-unknown)', marginTop: 2,
          }}>
            {note}
          </div>
        )}
        {rule.risk && (
          <div className="mt-[2px] text-xs text-ink-muted">
            {RISK_CATEGORY_LABEL[rule.risk.category] ?? rule.risk.category}：{rule.risk.name} :{rule.risk.port}
          </div>
        )}
      </div>
    )
  }
  return <span className="text-ink-muted">—</span>
}

/* ---------------------------------------------------------------------- */
/* 3. 待确认规则（enabled = false）                                        */
/* ---------------------------------------------------------------------- */

interface PendingRow {
  namespace: string
  workload: string
  rule: CandidateRule
  /** true：默认启用、当前被一条 DISABLE 覆盖收紧；false：默认禁用，走常规确认流程。 */
  originallyEnabled: boolean
  override?: RuleOverride
}

function buildPendingRows(
  candidates: CandidatePolicy[], overrideIndex: Map<string, RuleOverride>,
): PendingRow[] {
  const rows: PendingRow[] = []
  for (const c of candidates) {
    for (const r of c.rules) {
      const o = overrideIndex.get(overrideKey(c.namespace, c.workload, r.fingerprint))
      if (!r.enabled) {
        rows.push({ namespace: c.namespace, workload: c.workload, rule: r, originallyEnabled: false, override: o })
      } else if (o?.decision === 'DISABLE') {
        rows.push({ namespace: c.namespace, workload: c.workload, rule: r, originallyEnabled: true, override: o })
      }
    }
  }
  return rows
}

/** ipBlock 对端形如 "203.0.113.10/32"（可能带 " except ..."），selector 对端形如 "payment/api"。 */
const CIDR_PEER = /^\d{1,3}(\.\d{1,3}){3}\//

function isWorkloadPeer(peer: string): boolean {
  return peer.includes('/') && !CIDR_PEER.test(peer)
}

function sameStrings(a: string[], b: string[]): boolean {
  return a.length === b.length && a.every((v, i) => v === b[i])
}

/**
 * 找出一条集群内规则的镜像对端。
 *
 * 集群内连接会被拆成两条独立指纹的规则：源端 egress、目的端 ingress
 * （internal/policygen/aggregate.go 的 classify）。NetworkPolicy 要求
 * 两端都放行连接才会真正打开，单独确认一侧不会移动任何 dry-run 数字——
 * 这是 Task 4 发现的产品事实，不提示的话，操作者会看到"我确认了、但
 * 预测没变"，转而怀疑功能本身坏了。只有 selector 对端才可能有镜像对，
 * ipBlock（公网/CIDR）对端不存在对称的另一侧。
 */
function findMirroredCounterpart(
  candidates: CandidatePolicy[], namespace: string, workload: string, rule: CandidateRule,
): { namespace: string; workload: string; rule: CandidateRule } | null {
  if (rule.peers.length !== 1 || !isWorkloadPeer(rule.peers[0])) return null
  const [peerNs, peerWl] = rule.peers[0].split('/')
  const mirrorDirection = rule.direction === 'EGRESS' ? 'INGRESS' : 'EGRESS'
  const selfRef = `${namespace}/${workload}`
  const target = candidates.find((c) => c.namespace === peerNs && c.workload === peerWl)
  if (!target) return null
  const counterpart = target.rules.find((r) =>
    r.direction === mirrorDirection && r.peers.length === 1 && r.peers[0] === selfRef
    && sameStrings(r.ports, rule.ports))
  return counterpart ? { namespace: peerNs, workload: peerWl, rule: counterpart } : null
}

function PendingSection({ candidates, overrides, cluster, onChanged }: {
  candidates: CandidatePolicy[]
  overrides: RuleOverride[]
  cluster: string
  onChanged: () => void
}) {
  const overrideIndex = buildOverrideIndex(overrides)
  const pending = buildPendingRows(candidates, overrideIndex)

  return (
    <Section
      title="待确认规则"
      description="enabled = false 的规则（证据不足以自动启用，或命中已知风险端口），以及被人工禁用的默认启用规则——两者都是这条推荐里已知的风险或人工判断，因此默认展开、不折叠、不灰化。「镜像对」列出集群内连接需要同时确认的另一侧规则：只确认一侧不会改变任何 dry-run 数字。"
      meta={`${pending.length} 条`}
    >
      {pending.length === 0 ? (
        <EmptyState message="没有待确认的规则。" detail="全部候选规则都满足自动启用条件，也没有人工禁用过任何默认启用的规则。" />
      ) : (
        <ScrollTableCard maxHeight={560}>
          <StickyHead>
            <tr>
              <th>namespace/workload</th>
              <th>依据</th>
              <th>风险</th>
              <th>方向</th>
              {/* 待确认规则尤其需要对端与端口：一条 SSH 规则的要害不是"它是 SSH"，
                  而是"它通向 203.0.113.10/32 还是通向整个公网"。 */}
              <th>对端 · 端口</th>
              <th className="num">流量条数</th>
              <th>镜像对</th>
              <th>人工决定</th>
            </tr>
          </StickyHead>
          <tbody>
            {pending.map(({ namespace, workload, rule: r, originallyEnabled, override }) => {
              const mirror = findMirroredCounterpart(candidates, namespace, workload, r)
              const mirrorEnabled = mirror
                ? effectiveEnabled(overrideIndex, mirror.namespace, mirror.workload, mirror.rule)
                : false
              return (
                <tr key={`${namespace}/${workload}/${r.fingerprint}`}>
                  <td className="mono text-sm">
                    {namespace}/{workload}
                    {originallyEnabled && (
                      <div className="mt-[2px]">
                        <span className="text-xs text-ink-muted">默认启用</span>
                      </div>
                    )}
                  </td>
                  <td>{r.evidence ?? r.baseline ?? '—'}</td>
                  <td>
                    {r.risk ? (
                      <Chip strong>
                        {RISK_CATEGORY_LABEL[r.risk.category] ?? r.risk.category} · {r.risk.name}:{r.risk.port}
                      </Chip>
                    ) : (
                      <span className="text-ink-muted">—</span>
                    )}
                  </td>
                  <td><Chip>{r.direction}</Chip></td>
                  <td>
                    <RuleTargets values={r.peers} />
                    <RuleTargets values={r.ports} />
                  </td>
                  <td className="num">{r.flowCount}</td>
                  <td className="text-xs">
                    {!mirror ? (
                      <span className="text-ink-muted">—</span>
                    ) : mirrorEnabled ? (
                      <span className="text-ink-muted">
                        对端 {mirror.namespace}/{mirror.workload} 已放行
                      </span>
                    ) : (
                      <Chip strong>镜像对待确认 · {mirror.namespace}/{mirror.workload}</Chip>
                    )}
                  </td>
                  <td>
                    <OverrideControl
                      cluster={cluster} namespace={namespace} workload={workload}
                      fingerprint={r.fingerprint}
                      decision={originallyEnabled ? 'DISABLE' : 'ENABLE'}
                      override={override} onChanged={onChanged}
                    />
                  </td>
                </tr>
              )
            })}
          </tbody>
        </ScrollTableCard>
      )}
    </Section>
  )
}

/* ---------------------------------------------------------------------- */
/* 5. 已失效的确认                                                         */
/* ---------------------------------------------------------------------- */

/**
 * 只在 staleOverrides 非空时出现，出现时不折叠：它代表一个人做过的判断
 * 现在悬空了——只说「已失效」等于告诉人「你上周的工作没了，自己去查」。
 */
function StaleOverridesSection({ staleOverrides }: { staleOverrides: StaleOverride[] }) {
  if (staleOverrides.length === 0) return null

  return (
    <Section
      title="已失效的确认"
      description="规则内容已变（指纹不再匹配当前候选集），或当初的决定试图禁用一条 BASELINE 规则（不接受）。当初的判断仍然列在这里，但它不再对应任何一条当前的候选规则，需要重新确认。"
      meta={`${staleOverrides.length} 条`}
    >
      <ScrollTableCard maxHeight={480}>
        <StickyHead>
          <tr>
            <th>namespace/workload</th>
            <th>当初确认</th>
            <th>现在这个位置</th>
            <th>失效原因</th>
          </tr>
        </StickyHead>
        <tbody>
          {staleOverrides.map((s) => (
            <tr key={`${s.override.namespace}/${s.override.workload}/${s.override.fingerprint}`}>
              <td className="mono text-sm">
                {s.override.namespace}/{s.override.workload}
              </td>
              <td className="text-xs">
                {formatTime(s.override.decidedAt)} by {s.override.decidedBy}
                <div style={{ marginTop: 2, color: 'var(--text-secondary)' }}>「{s.override.reason}」</div>
              </td>
              <td className="text-xs">
                {s.currentRules.length === 0 ? (
                  <span className="text-ink-muted">该 workload 已不存在</span>
                ) : (
                  <span className="mono" style={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
                    {s.currentRules.map((rule, ri) => <span key={ri}>{rule}</span>)}
                  </span>
                )}
              </td>
              <td className="text-xs">{s.reason}</td>
            </tr>
          ))}
        </tbody>
      </ScrollTableCard>
    </Section>
  )
}

/* ---------------------------------------------------------------------- */
/* 6. 缺失 Baseline                                                       */
/* ---------------------------------------------------------------------- */

/**
 * 缺失 Baseline，以及其中哪几类其实是「我们没看过」。
 *
 * 未评估**不单列一栏**：那几类仍然留在缺失清单里，只是多带一条不同的处置
 * （design doc 2026-08-17 §4）。摘出去等于让一个"没看过"悄悄读成"这里没
 * 问题"；不标注则等于塌回普通缺失，运维会去写一条 DNS 策略，而真正该做的
 * 是改 RBAC。两种缺口都照旧挡住 Enforcing——标注不放宽任何门禁。
 */
function MissingBaselineSection({ missing, baselineKinds, notAssessed, notApplicable }: {
  missing: MissingBaseline[]
  baselineKinds: Kind[]
  notAssessed: Kind[] | null
  notApplicable: MissingBaseline[] | null
}) {
  const note = notAssessedNote(notAssessed)
  const naNote = notApplicableNote(notApplicable)
  return (
    <Section
      title="缺失 Baseline"
      description="按 namespace 列出缺失的基础设施事实类型。下方同时列出本轮检查过的全部类型，用来区分「确认没缺」与「根本没查」。"
      meta={`${missing.length} 个 namespace`}
    >
      {/* 「未评估」在清单之前、且在空清单上同样出现：读完清单才读到"其中
          有几类我们压根没看过"，那份清单已经被当成结论读过一遍了；而一句
          没有任何限定的「没有 namespace 缺失 baseline」，正是这条标注要
          挡住的读法。 */}
      {note !== '' && <Notice>{note}</Notice>}
      {/* 「不适用」与缺失并列，且在空清单上同样出现：一份空缺失可能是
          "五类都推出来了"，也可能是"其中几类这个命名空间根本不需要"，
          而这两句话对下一步做什么的含义不同。少了它，那几行会凭空消失
          （design doc 2026-08-18-baseline-applicability §5）。 */}
      {naNote !== '' && <Notice>{naNote}</Notice>}
      {missing.length > 0 && <RemedyLegend missing={missing} notAssessed={notAssessed} />}
      {missing.length === 0 ? (
        <EmptyState
          message="没有 namespace 缺失 baseline。"
          detail={`已检查类型（${baselineKinds.length} 种）：${baselineKinds.join('、')}。`}
        />
      ) : (
        <>
          <TableCard>
            <thead>
              <tr><th>namespace</th><th>缺失类型</th></tr>
            </thead>
            <tbody>
              {missing.map((m) => {
                const gaps = baselineGapViews(m.kinds, notAssessed)
                return (
                  <tr key={m.namespace}>
                    <td className="mono">{m.namespace}</td>
                    <td>
                      <span className="flex flex-wrap gap-1">
                        {gaps.map((g) => (
                          <span key={g.kind} style={{ display: 'inline-flex', gap: 4 }}>
                            <Chip strong>{g.kind}</Chip>
                            {/* 未评估、以及「服务端没说评估过没有」的类型都仍然挂在
                                同一格里，不移出清单：它照旧是一个缺口，区别只在下一步
                                做什么。徽标文案由 baselineGapViews 给出，不在这里拼——
                                三种状态各自显示什么必须落在能被测试钉住的那一层。 */}
                            {g.badge !== '' && <Chip>{g.badge}</Chip>}
                          </span>
                        ))}
                      </span>
                    </td>
                    {/* 处置**不逐行重复**：它只取决于缺口种类（真缺失 /
                        未评估 / 无从判断），与 namespace 无关。UAT 上 42 个
                        namespace 全缺 NODE_AGENT，逐行渲染就是同一句话印
                        42 遍 —— 而一面读不完的墙与没有说明是同一个效果。
                        三种处置抽到表格上方，折一次。 */}
                  </tr>
                )
              })}
            </tbody>
          </TableCard>
          <p style={{ marginTop: 'var(--space-2)', fontSize: 'var(--text-xs)', color: 'var(--text-muted)' }}>
            已检查的 baseline 类型全集（{baselineKinds.length} 种）：{baselineKinds.join('、')}。
            未出现在上表中的 namespace，对这些类型均不缺失——不是没查。
          </p>
        </>
      )}
    </Section>
  )
}

/* ---------------------------------------------------------------------- */
/* 7. 未进入候选集的工作负载                                                */
/* ---------------------------------------------------------------------- */

/**
 * 从未进入候选策略花名册的 Pod。
 *
 * 与「不可生成清单」是两码事，因此单列一节：后者说的是"这条流量表达不成
 * 规则"，这一节说的是更前一步的缺口——这些 Pod 根本没进名册，因此不会作为
 * 主体出现在任何一条判定里，连"不可生成"都报不出它们。不展示它，页面就在
 * 用「候选策略 7 组、不可生成 97 条」这种读起来像是全都盘过一遍的口径，
 * 掩盖掉一批一条策略都没有的 Pod。
 */
function ExcludedWorkloadSection({ items }: { items: ExcludedWorkload[] }) {
  const groups = new Map<WorkloadExclusionReason, ExcludedWorkload[]>()
  for (const it of items) {
    const g = groups.get(it.reason) ?? []
    g.push(it)
    groups.set(it.reason, g)
  }

  return (
    <Section
      title="未被任何候选策略覆盖的工作负载"
      description="这些 Pod 从未进入候选策略的花名册，因此没有任何一条生成出来的策略覆盖它们——不是「规则偏少」，是一条都没有。原因是封闭枚举。与「不可生成清单」同理，它们不受 namespace 筛选影响：一个没进名册的 Pod 在哪个视图下都同样缺失。"
      meta={`${items.length} 个 Pod`}
    >
      {items.length === 0 ? (
        <EmptyState
          message="没有被排除在外的工作负载。"
          detail="本集群的每个 Pod 都进入了候选策略花名册。这不代表它们的每条流量都能表达成规则——那部分见下方「不可生成清单」。"
        />
      ) : (
        [...groups.entries()].map(([reason, rows]) => (
          <div key={reason} className="mb-4">
            <div className="mb-2 flex flex-wrap items-baseline gap-2">
              <strong className="text-sm">
                {WORKLOAD_EXCLUSION_REASON_LABEL[reason]}
              </strong>
              <span className="mono text-xs text-ink-muted">
                {reason}
              </span>
              <span className="text-xs text-ink-muted">· {rows.length} 个</span>
            </div>
            <ScrollTableCard maxHeight={320}>
              <StickyHead>
                {/* 标签必须列出来：这一节最常见的处置方式是"标签键写错了"或
                    "用了平台还不认识的键"，而这两件事只看 namespace/pod 看不出来。 */}
                <tr><th>namespace</th><th>pod</th><th>标签</th></tr>
              </StickyHead>
              <tbody>
                {rows.map((w) => (
                  <tr key={`${w.namespace}/${w.pod}`}>
                    <td className="mono">{w.namespace}</td>
                    <td className="mono">{w.pod}</td>
                    <td><PodLabels labels={w.labels} /></td>
                  </tr>
                ))}
              </tbody>
            </ScrollTableCard>
          </div>
        ))
      )}
    </Section>
  )
}

/** Pod 标签。空标签集显式写出来，理由同 RuleTargets 的「未限定」：空单元格会被读成"没这项"。 */
function PodLabels({ labels }: { labels: Record<string, string> }) {
  const entries = Object.entries(labels)
  if (entries.length === 0) {
    return <span className="text-ink-muted">无标签</span>
  }
  return (
    <span className="flex flex-wrap gap-1">
      {entries.map(([k, v]) => <Chip key={k}>{k}={v}</Chip>)}
    </span>
  )
}

/* ---------------------------------------------------------------------- */
/* 8. 不可生成清单                                                         */
/* ---------------------------------------------------------------------- */

function UngeneratableSection({ items }: { items: UngeneratableItem[] }) {
  const groups = new Map<string, UngeneratableItem[]>()
  for (const it of items) {
    const g = groups.get(it.reason) ?? []
    g.push(it)
    groups.set(it.reason, g)
  }

  return (
    <Section
      title="不可生成清单"
      description="这些流量表达不出候选规则，原因是封闭枚举。同一条流量在任何筛选条件下都同样表达不了，因此这份清单不随 namespace 筛选变化。"
      meta={`${items.length} 条`}
    >
      {items.length === 0 ? (
        <EmptyState message="没有不可生成的流量。" detail="观测到的全部流量都能表达为候选规则。" />
      ) : (
        [...groups.entries()].map(([reason, rows]) => (
          <div key={reason} className="mb-4">
            <div className="mb-2 flex flex-wrap items-baseline gap-2">
              <strong className="text-sm">
                {UNGENERATABLE_REASON_LABEL[reason as UngeneratableReason] ?? reason}
              </strong>
              <span className="mono text-xs text-ink-muted">
                {reason}
              </span>
              <span className="text-xs text-ink-muted">· {rows.length} 条</span>
            </div>
            <ScrollTableCard maxHeight={320}>
              <StickyHead>
                <tr><th>flowId</th><th>detail</th></tr>
              </StickyHead>
              <tbody>
                {rows.map((it) => (
                  <tr key={it.flowId}>
                    <td className="mono">{it.flowId}</td>
                    <td className="text-sm">{it.detail}</td>
                  </tr>
                ))}
              </tbody>
            </ScrollTableCard>
          </div>
        ))
      )}
    </Section>
  )
}

/* ---------------------------------------------------------------------- */
/* 共享小件                                                                 */
/* ---------------------------------------------------------------------- */

/** 与 ClustersPage 的 formatTime 同一形状，本页独立维护一份（同一处约定，非共享导出）。 */
function formatTime(iso: string): string {
  return new Date(iso).toISOString().replace('T', ' ').replace(/\.\d+Z$/, ' UTC')
}

const smallButtonStyle: CSSProperties = {
  padding: '4px 10px', fontSize: 'var(--text-xs)', fontWeight: 500,
  color: 'var(--text-on-dark)', background: 'var(--accent)',
  border: 'none', borderRadius: 'var(--radius-sm)', cursor: 'pointer',
}

const secondarySmallButtonStyle: CSSProperties = {
  padding: '4px 10px', fontSize: 'var(--text-xs)', fontWeight: 500,
  color: 'var(--text)', background: 'var(--surface)',
  border: '1px solid var(--border-strong)', borderRadius: 'var(--radius-sm)', cursor: 'pointer',
}

/**
 * 粒度切换与折叠的放宽报告。
 *
 * **回显的粒度优先于本地选择。** 两者不一致说明请求里那个值没被服务端认下来
 * （拼错、老版本后端），此时屏幕上这份策略是服务端给的那个粒度，不是你点的
 * 那个 —— 显示成你点的那个就是让界面替服务端撒谎。
 */
function GranularitySection({ current, onPick, echoed, widening, count }: {
  current: Granularity
  onPick: (g: Granularity) => void
  echoed: Granularity | null
  widening: Widening[] | null
  count: number
}) {
  const view = granularityView(echoed)
  const note = wideningNote(widening)
  const mismatch = view.code !== current
  return (
    <Section
      title="策略粒度"
      description="决定 podSelector 选中谁。对端不受粒度影响——放行的目标始终精确到 workload 与端口。"
      meta={`${count} 份`}
    >
      <div className="mb-3 flex flex-wrap items-center gap-3">
        <Segmented
          ariaLabel="策略粒度"
          value={current}
          onChange={onPick}
          options={ALL_GRANULARITIES.map((g) => ({ value: g, label: granularityView(g).label }))}
        />
        <span className="text-xs text-ink-muted">主体：{view.subject}</span>
      </div>

      {/* 粒度不一致要常驻，不折叠：屏幕上这份策略不是你点的那个粒度，
          而那句话读晚了，下面每一个数字都被读错了。 */}
      {mismatch && (
        <Notice>
          服务端回显的粒度是 <strong>{view.code}</strong>，与这里选中的
          <strong> {current} </strong>不一致。下方内容是服务端那个粒度的产物 ——
          请求里的取值没有被认下来（拼错，或后端版本不认识它）。
        </Notice>
      )}

      {/* 放宽报告默认展开：粗化只会放宽，那是操作者切过来之后第一件该知道
          的事。摘要自带结论，展开的是分布明细。 */}
      {note !== '' && (
        <Disclosure defaultOpen summary={<strong>折叠代价</strong>}>
          {note}
        </Disclosure>
      )}

      {/* 这个粒度的取舍折起来：它是一段稳定的说明，读过一次就不必再占版面。
          摘要仍然说出结论，不是一个"详情"。 */}
      <div className="mt-2">
        <Disclosure summary={<span className="text-ink-muted">这个粒度的代价</span>}>
          {view.detail}
        </Disclosure>
      </div>
    </Section>
  )
}

/**
 * 三种缺口各自的处置，**整表一次**。
 *
 * 处置只取决于缺口种类，与 namespace 无关 —— 逐行渲染会在 UAT 那种
 * 「42 个 namespace 全缺同一类」的集群上把同一句话印 42 遍，而一面读不完
 * 的墙与没有说明是同一个效果。
 *
 * 只列**这一次真的出现过**的那几种：把三种都摆出来，读者要自己去表里
 * 比对哪一种与自己有关，而那正是这一块想省掉的动作。
 */
function RemedyLegend({ missing, notAssessed }: {
  missing: MissingBaseline[]
  notAssessed: Kind[] | null
}) {
  const present = new Map<string, { badge: string; remedy: string }>()
  for (const m of missing) {
    for (const g of baselineGapViews(m.kinds, notAssessed)) {
      const key = g.assessment
      if (!present.has(key)) present.set(key, { badge: g.badge, remedy: g.remedy })
    }
  }
  const rows = [...present.entries()]
  if (rows.length === 0) return null
  return (
    <div className="mb-2">
      <Disclosure summary={<span>这份清单里有 <strong>{rows.length}</strong> 种缺口，各自的处置不同</span>}>
        <dl className="m-0 grid gap-2">
          {rows.map(([kind, v]) => (
            <div key={kind}>
              <dt className="text-xs font-semibold text-ink">{v.badge === '' ? '真缺失' : v.badge}</dt>
              <dd className="m-0 text-xs text-ink-muted">{v.remedy}</dd>
            </div>
          ))}
        </dl>
      </Disclosure>
    </div>
  )
}
