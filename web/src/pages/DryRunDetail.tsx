import { UNKNOWN_REASON_LABEL, type ChangedFlow, type Confidence, type Verdict } from '../api/types'
import { CrossClusterMark, UnmanagedMark, VerdictBadge } from '../components/Verdict'
import { EmptyState, ScrollTableCard, StickyHead, StatTile, TableCard } from '../components/ui'
import { wouldOpenEmptyDetail, type DryRunDetailView } from './dryRunView'

/**
 * dry-run 明细区：连接清单、UNKNOWN 构成、六个次级统计。
 *
 * 单独成一个文件，不是为了文件短一点，而是为了让 C1 那条缺陷写不出来。
 * 明细区曾经与 tile 写在同一个函数体里，两套预测都在词法作用域内，于是
 * 「rows 属性指向另一套预测」是一个改一行就能犯、且编译、lint、测试、
 * 构建全部通过的错误——它的实际形态是 81 行连接列在一个写着 78 的 tile
 * 底下。这里的入参只有一个 view，两套预测都不在作用域内，那一行改不出来。
 *
 * 因此这个组件不接受 PredictionReport，只接受 dryRunView 选定后的 view：
 * 多一个入口就多一次选择，而 C1 就是两次选择的分歧。
 */
export function DryRunDetail({ view }: { view: DryRunDetailView }) {
  const d = view.report

  return (
    <>
      <ChangeDetailTable
        title="会被拦断的连接" rows={d.changes.WOULD_BREAK}
        emptyMessage="没有会被这条推荐拦断的连接。"
        emptyDetail={view.emptyDetail}
      />

      <div className="mt-4">
        <ChangeDetailTable
          title="敞口会被扩大的连接" rows={d.changes.WOULD_OPEN}
          emptyMessage="没有会被这条推荐放宽为放行的连接。"
          emptyDetail={wouldOpenEmptyDetail(view)}
        />
      </div>

      <div className="mt-4">
        <div style={{
          display: 'flex', alignItems: 'baseline', justifyContent: 'space-between',
          marginBottom: 'var(--space-2)', flexWrap: 'wrap', gap: 'var(--space-2)',
        }}>
          <strong className="text-sm">UNKNOWN 的构成</strong>
          <span className="text-xs text-ink-muted">
            只报一个总数无法说明该去修哪个子系统，下面是这 {d.counts.UNKNOWN} 条的具体成因。
          </span>
        </div>
        {Object.keys(d.unknownComposition).length === 0 ? (
          <EmptyState message="没有无法判定的变化。" detail="全部变化都得到了明确结论；这不代表结论都可信，可信度见下方降级计数。" />
        ) : (
          <TableCard>
            <thead>
              <tr><th>成因</th><th>枚举值</th><th className="num">条数</th></tr>
            </thead>
            <tbody>
              {Object.entries(d.unknownComposition)
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
        <StatTile label="可信判定" value={String(d.trustedCount)} />
        <StatTile label="可信度降级" value={String(d.degradedCount)} tone="degraded" />
        {/* 恒为 0 也要显示：三档之和等于"共评估"是这一行可以自检的地方，
            少一档就只能选择相信它。非 0 意味着后端出现了枚举外的可信度取值。 */}
        <StatTile
          label="可信度未登记" value={String(d.unratedCount)}
          tone={d.unratedCount > 0 ? 'unknown' : undefined}
          note="枚举外取值，正常为 0"
        />
        <StatTile label="跨集群" value={String(d.crossClusterCount)} note="当前版本不做管控" />
        <StatTile label="不受管控" value={String(d.unmanagedCount)} note="hostNetwork，策略管不到" />
        <StatTile label="共评估" value={String(d.totalEvaluated)} />
      </div>
    </>
  )
}

/**
 * 一类变化的连接清单。
 *
 * 只在明细区内使用，因此与 DryRunDetail 同文件：把它留在页面文件里，
 * rows 属性就又能够到另一套预测了。
 */
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
        <strong className="text-sm">{title}</strong>
        <span className="text-xs text-ink-muted">{rows.length} 条</span>
      </div>
      {rows.length === 0 ? (
        <EmptyState message={emptyMessage} detail={emptyDetail} />
      ) : (
        <ScrollTableCard maxHeight={420}>
          <StickyHead>
            <tr>
              <th>源 → 目的</th>
              <th>协议/端口</th>
              <th>判定变化</th>
              <th>标记</th>
            </tr>
          </StickyHead>
          <tbody>
            {rows.map((f) => (
              <tr key={f.flowId}>
                <td className="mono text-sm">
                  {f.sourceLabel} → {f.destLabel}
                </td>
                <td className="num">{f.protocol}:{f.port}</td>
                <td>
                  <span style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
                    <VerdictBadge verdict={f.current as Verdict} />
                    <span className="text-ink-muted">→</span>
                    <VerdictBadge verdict={f.predicted as Verdict} confidence={f.confidence as Confidence} />
                  </span>
                </td>
                <td>
                  <span className="flex flex-wrap gap-1">
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
