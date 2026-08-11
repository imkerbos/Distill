import type { ReactNode } from 'react'
import { api } from '../api/client'
import {
  RISK_CATEGORY_LABEL, UNKNOWN_REASON_LABEL,
  type CandidatePolicy, type CandidateRule, type ChangedFlow, type Confidence,
  type Kind, type MissingBaseline, type PredictionReport, type RuleOrigin,
  type UngeneratableItem, type UngeneratableReason, type Verdict,
} from '../api/types'
import { useResource } from '../api/useResource'
import { CrossClusterMark, UnmanagedMark, VerdictBadge } from '../components/Verdict'
import { Card, Chip, EmptyState, PageHeader, Section, StatTile, TableCard } from '../components/ui'

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
}

export default function PolicyPage({ cluster }: { cluster: string }) {
  const { data: pv, error, loading } = useResource(cluster, () => api.policyPreview(cluster))

  if (error) return <p style={{ color: 'var(--verdict-deny)' }}>{error}</p>
  if (loading || !pv) return <p style={{ color: 'var(--text-muted)' }}>加载中…</p>

  return (
    <div>
      <PageHeader
        title="候选策略"
        description="dry-run 预测置顶：先看这条推荐会拦掉多少条当前正在工作的连接，再看策略本身长什么样。顺序即优先级。"
      />

      <DryRunSection prediction={pv.prediction} />
      <CandidateSection candidates={pv.candidates} />
      <PendingSection candidates={pv.candidates} />
      <MissingBaselineSection missing={pv.missingBaselines} baselineKinds={pv.baselineKinds} />
      <UngeneratableSection items={pv.ungeneratable} />
    </div>
  )
}

/* ---------------------------------------------------------------------- */
/* 1. dry-run 影响                                                        */
/* ---------------------------------------------------------------------- */

function DryRunSection({ prediction }: { prediction: PredictionReport }) {
  const c = prediction.counts

  return (
    <Section
      title="dry-run 影响"
      description="按当前候选策略重放同一段观测流量得到的四类变化。WOULD_BREAK 是本页最重要的数字——它是这条推荐一旦下发会拦断的、当前正在工作的连接数。"
    >
      <div style={{
        display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(180px, 1fr))',
        gap: 'var(--space-3)', marginBottom: 'var(--space-4)',
      }}>
        <StatTile
          label="WOULD_BREAK · 会被拦断" value={String(c.WOULD_BREAK)}
          tone="deny" size="lg" note="当前放行、新策略会拒绝的连接"
        />
        <StatTile
          label="WOULD_OPEN · 敞口扩大" value={String(c.WOULD_OPEN)}
          tone="unknown" note="当前拒绝、新策略会放行 —— 不是好消息"
        />
        <StatTile
          label="UNCHANGED · 无变化" value={String(c.UNCHANGED)}
          note="两侧都判得出、且结论一致"
        />
        <StatTile
          label="UNKNOWN · 无法判定" value={String(c.UNKNOWN)}
          tone="unknown" note="当前判定或新策略判定有一侧给不出结论"
        />
      </div>

      <ChangeDetailTable
        title="会被拦断的连接" rows={prediction.changes.WOULD_BREAK}
        emptyMessage="没有会被这条推荐拦断的连接。"
        emptyDetail="基于当前候选策略对观测流量重放计算得出，不是未检测。"
      />

      <div style={{ marginTop: 'var(--space-4)' }}>
        <ChangeDetailTable
          title="敞口会被扩大的连接" rows={prediction.changes.WOULD_OPEN}
          emptyMessage="没有会被这条推荐放宽为放行的连接。"
          emptyDetail="基于当前候选策略对观测流量重放计算得出，不是未检测；WOULD_OPEN 为 0 是一个真实的 0。"
        />
      </div>

      <div style={{ marginTop: 'var(--space-4)' }}>
        <div style={{
          display: 'flex', alignItems: 'baseline', justifyContent: 'space-between',
          marginBottom: 'var(--space-2)', flexWrap: 'wrap', gap: 'var(--space-2)',
        }}>
          <strong style={{ fontSize: 'var(--text-sm)' }}>UNKNOWN 的构成</strong>
          <span style={{ fontSize: 'var(--text-xs)', color: 'var(--text-muted)' }}>
            只报一个总数无法说明该去修哪个子系统，下面是这 {c.UNKNOWN} 条的具体成因。
          </span>
        </div>
        {Object.keys(prediction.unknownComposition).length === 0 ? (
          <EmptyState message="没有无法判定的变化。" detail="全部变化都得到了明确结论；这不代表结论都可信，可信度见下方降级计数。" />
        ) : (
          <TableCard>
            <thead>
              <tr><th>成因</th><th>枚举值</th><th className="num">条数</th></tr>
            </thead>
            <tbody>
              {Object.entries(prediction.unknownComposition)
                .sort((a, b) => b[1] - a[1])
                .map(([reason, count]) => (
                  <tr key={reason}>
                    <td>{UNKNOWN_REASON_LABEL[reason] ?? reason}</td>
                    <td className="mono">{reason}</td>
                    <td className="num" style={{ fontWeight: 600 }}>{count}</td>
                  </tr>
                ))}
            </tbody>
          </TableCard>
        )}
      </div>

      <div style={{
        display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(150px, 1fr))',
        gap: 'var(--space-3)', marginTop: 'var(--space-4)',
      }}>
        <StatTile label="可信判定" value={String(prediction.trustedCount)} />
        <StatTile label="可信度降级" value={String(prediction.degradedCount)} tone="degraded" />
        {/* 恒为 0 也要显示：三档之和等于"共评估"是这一行可以自检的地方，
            少一档就只能选择相信它。非 0 意味着后端出现了枚举外的可信度取值。 */}
        <StatTile
          label="可信度未登记" value={String(prediction.unratedCount)}
          tone={prediction.unratedCount > 0 ? 'unknown' : undefined}
          note="枚举外取值，正常为 0"
        />
        <StatTile label="跨集群" value={String(prediction.crossClusterCount)} note="当前版本不做管控" />
        <StatTile label="不受管控" value={String(prediction.unmanagedCount)} note="hostNetwork，策略管不到" />
        <StatTile label="共评估" value={String(prediction.totalEvaluated)} />
      </div>
    </Section>
  )
}

function ChangeDetailTable({ title, rows, emptyMessage, emptyDetail }: {
  title: string
  rows: ChangedFlow[]
  emptyMessage: string
  emptyDetail: string
}) {
  return (
    <div>
      <div style={{
        display: 'flex', alignItems: 'baseline', justifyContent: 'space-between',
        marginBottom: 'var(--space-2)',
      }}>
        <strong style={{ fontSize: 'var(--text-sm)' }}>{title}</strong>
        <span style={{ fontSize: 'var(--text-xs)', color: 'var(--text-muted)' }}>{rows.length} 条</span>
      </div>
      {rows.length === 0 ? (
        <EmptyState message={emptyMessage} detail={emptyDetail} />
      ) : (
        <ScrollTableCard maxHeight={420}>
          <thead style={STICKY_HEAD}>
            <tr>
              <th>源 → 目的</th>
              <th>协议/端口</th>
              <th>判定变化</th>
              <th>标记</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((f) => (
              <tr key={f.flowId}>
                <td className="mono" style={{ fontSize: 'var(--text-sm)' }}>
                  {f.sourceLabel} → {f.destLabel}
                </td>
                <td className="num">{f.protocol}:{f.port}</td>
                <td>
                  <span style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
                    <VerdictBadge verdict={f.current as Verdict} />
                    <span style={{ color: 'var(--text-muted)' }}>→</span>
                    <VerdictBadge verdict={f.predicted as Verdict} confidence={f.confidence as Confidence} />
                  </span>
                </td>
                <td>
                  <span style={{ display: 'flex', gap: 4, flexWrap: 'wrap' }}>
                    {f.crossCluster && <CrossClusterMark />}
                    {f.unmanaged && <UnmanagedMark />}
                  </span>
                </td>
              </tr>
            ))}
          </tbody>
        </ScrollTableCard>
      )}
    </div>
  )
}

/* ---------------------------------------------------------------------- */
/* 2. 候选策略列表（仅启用规则）                                            */
/* ---------------------------------------------------------------------- */

function CandidateSection({ candidates }: { candidates: CandidatePolicy[] }) {
  return (
    <Section
      title="候选策略"
      description="按 namespace/workload 分组，仅展示会被启用的规则。BASELINE 来自基础设施事实推导，LEARNED 来自观测流量学习——两者证据强度不同，徽标视觉可分。待确认（enabled=false）的规则见下一节，不在此处出现。"
      meta={`${candidates.length} 组`}
    >
      {candidates.length === 0 ? (
        <EmptyState message="没有可生成的候选策略。" detail="见下方「不可生成清单」了解原因。" />
      ) : (
        candidates.map((c) => {
          const enabled = c.rules.filter((r) => r.enabled)
          return (
            <div key={`${c.namespace}/${c.workload}`} style={{ marginBottom: 'var(--space-4)' }}>
              <div style={{ marginBottom: 'var(--space-2)' }}>
                <strong className="mono" style={{ fontSize: 'var(--text-sm)' }}>
                  {c.namespace}/{c.workload}
                </strong>
                <span style={{ marginLeft: 8, fontSize: 'var(--text-xs)', color: 'var(--text-muted)' }}>
                  {enabled.length} 条启用规则
                </span>
              </div>
              {enabled.length === 0 ? (
                <EmptyState
                  message="该工作负载没有已启用的规则。"
                  detail="全部规则处于待确认状态，见下方「待确认规则」一节。"
                />
              ) : (
                <RuleTable rules={enabled} />
              )}
            </div>
          )
        })
      )}
    </Section>
  )
}

function RuleTable({ rules }: { rules: CandidateRule[] }) {
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
        </tr>
      </thead>
      <tbody>
        {rules.map((r, i) => (
          <tr key={i}>
            <td><OriginBadge origin={r.origin} /></td>
            <td><RuleBasis rule={r} /></td>
            <td><Chip>{r.direction}</Chip></td>
            <td><RuleTargets values={r.peers} /></td>
            <td><RuleTargets values={r.ports} /></td>
            <td className="num">{r.flowCount}</td>
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
    return <span style={{ color: 'var(--text-muted)' }}>未限定</span>
  }
  return (
    <span className="mono" style={{
      display: 'flex', flexDirection: 'column', gap: 2, fontSize: 'var(--text-sm)',
    }}>
      {values.map((v) => <span key={v}>{v}</span>)}
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
          <div style={{ fontSize: 'var(--text-xs)', color: 'var(--text-muted)', marginTop: 2 }}>
            {rule.derivations
              .map((d) => `${d.sourceKind}:${d.namespace ? `${d.namespace}/` : ''}${d.name}`)
              .join('、')}
          </div>
        )}
      </div>
    )
  }
  if (rule.evidence) {
    return (
      <div>
        <div>{rule.evidence}</div>
        {rule.risk && (
          <div style={{ fontSize: 'var(--text-xs)', color: 'var(--text-muted)', marginTop: 2 }}>
            {RISK_CATEGORY_LABEL[rule.risk.category] ?? rule.risk.category}：{rule.risk.name} :{rule.risk.port}
          </div>
        )}
      </div>
    )
  }
  return <span style={{ color: 'var(--text-muted)' }}>—</span>
}

/* ---------------------------------------------------------------------- */
/* 3. 待确认规则（enabled = false）                                        */
/* ---------------------------------------------------------------------- */

interface PendingRule extends CandidateRule {
  namespace: string
  workload: string
}

function PendingSection({ candidates }: { candidates: CandidatePolicy[] }) {
  const pending: PendingRule[] = candidates.flatMap((c) =>
    c.rules
      .filter((r) => !r.enabled)
      .map((r) => ({ ...r, namespace: c.namespace, workload: c.workload })))

  return (
    <Section
      title="待确认规则"
      description="enabled = false 的规则：证据不足以自动启用，或命中已知风险端口。这一节承载着这条推荐里已知的风险，因此默认展开、不折叠、不灰化。"
      meta={`${pending.length} 条`}
    >
      {pending.length === 0 ? (
        <EmptyState message="没有待确认的规则。" detail="全部候选规则都满足自动启用条件。" />
      ) : (
        <ScrollTableCard maxHeight={480}>
          <thead style={STICKY_HEAD}>
            <tr>
              <th>namespace/workload</th>
              <th>依据</th>
              <th>风险</th>
              <th>方向</th>
              {/* 待确认规则尤其需要对端与端口：一条 SSH 规则的要害不是"它是 SSH"，
                  而是"它通向 203.0.113.10/32 还是通向整个公网"。 */}
              <th>对端 · 端口</th>
              <th className="num">流量条数</th>
            </tr>
          </thead>
          <tbody>
            {pending.map((r, i) => (
              <tr key={`${r.namespace}/${r.workload}/${i}`}>
                <td className="mono" style={{ fontSize: 'var(--text-sm)' }}>
                  {r.namespace}/{r.workload}
                </td>
                <td>{r.evidence ?? r.baseline ?? '—'}</td>
                <td>
                  {r.risk ? (
                    <Chip strong>
                      {RISK_CATEGORY_LABEL[r.risk.category] ?? r.risk.category} · {r.risk.name}:{r.risk.port}
                    </Chip>
                  ) : (
                    <span style={{ color: 'var(--text-muted)' }}>—</span>
                  )}
                </td>
                <td><Chip>{r.direction}</Chip></td>
                <td>
                  <RuleTargets values={r.peers} />
                  <RuleTargets values={r.ports} />
                </td>
                <td className="num">{r.flowCount}</td>
              </tr>
            ))}
          </tbody>
        </ScrollTableCard>
      )}
    </Section>
  )
}

/* ---------------------------------------------------------------------- */
/* 4. 缺失 Baseline                                                       */
/* ---------------------------------------------------------------------- */

function MissingBaselineSection({ missing, baselineKinds }: {
  missing: MissingBaseline[]
  baselineKinds: Kind[]
}) {
  return (
    <Section
      title="缺失 Baseline"
      description="按 namespace 列出缺失的基础设施事实类型。下方同时列出本轮检查过的全部类型，用来区分「确认没缺」与「根本没查」。"
      meta={`${missing.length} 个 namespace`}
    >
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
              {missing.map((m) => (
                <tr key={m.namespace}>
                  <td className="mono">{m.namespace}</td>
                  <td>
                    <span style={{ display: 'flex', gap: 4, flexWrap: 'wrap' }}>
                      {m.kinds.map((k) => <Chip key={k} strong>{k}</Chip>)}
                    </span>
                  </td>
                </tr>
              ))}
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
/* 5. 不可生成清单                                                         */
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
          <div key={reason} style={{ marginBottom: 'var(--space-4)' }}>
            <div style={{
              display: 'flex', alignItems: 'baseline', gap: 'var(--space-2)',
              marginBottom: 'var(--space-2)', flexWrap: 'wrap',
            }}>
              <strong style={{ fontSize: 'var(--text-sm)' }}>
                {UNGENERATABLE_REASON_LABEL[reason as UngeneratableReason] ?? reason}
              </strong>
              <span className="mono" style={{ fontSize: 'var(--text-xs)', color: 'var(--text-muted)' }}>
                {reason}
              </span>
              <span style={{ fontSize: 'var(--text-xs)', color: 'var(--text-muted)' }}>· {rows.length} 条</span>
            </div>
            <ScrollTableCard maxHeight={320}>
              <thead style={STICKY_HEAD}>
                <tr><th>flowId</th><th>detail</th></tr>
              </thead>
              <tbody>
                {rows.map((it) => (
                  <tr key={it.flowId}>
                    <td className="mono">{it.flowId}</td>
                    <td style={{ fontSize: 'var(--text-sm)' }}>{it.detail}</td>
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

const STICKY_HEAD = { position: 'sticky' as const, top: 0, background: 'var(--surface)' }

/**
 * 带纵向滚动的表格容器。
 *
 * 后端刻意不分页、不截断（每类变化都带完整连接清单），把全部行铺开会
 * 让页面高达数万像素；这里用固定高度 + 内部滚动承接，而不是截断数据——
 * 总数与每一行都必须可达，只是不必同时进入视口。
 */
function ScrollTableCard({ children, maxHeight = 420 }: { children: ReactNode; maxHeight?: number }) {
  return (
    <Card style={{ overflow: 'auto', maxHeight }}>
      <table className="dt">{children}</table>
    </Card>
  )
}
