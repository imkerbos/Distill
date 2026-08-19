import { api } from '../api/client'
import { RISK_CATEGORY_LABEL, type RiskPosition, type RiskyFlow, type SecurityReport } from '../api/types'
import { useResource } from '../api/useResource'
import DataSourceNotice from '../components/DataSourceNotice'
import { VerdictBadge } from '../components/Verdict'
import { Card, Chip, EmptyState, PageHeader, Section, TableCard } from '../components/ui'
import { listTruncationView, type ListTruncationView } from './preconditionsView'

/**
 * 位置按紧迫度从高到低排列，报告即按此顺序分组。
 *
 * 用分组与顺序表达严重程度，**不用颜色** —— ALLOW / DENY / UNKNOWN 的色值
 * 是判定结论的专属载体（spec §17.1）。把"跨 namespace"染成 UNKNOWN 的琥珀色，
 * 读者会把一条判定明确的连接读成无法判定。
 */
const POSITIONS: { key: RiskPosition; label: string; hint: string }[] = [
  {
    key: 'EGRESS_INTERNET',
    label: '出公网',
    hint: '风险最高：目标不在任何集群内，一旦通了就是一条数据出境路径。',
  },
  {
    key: 'CROSS_NAMESPACE',
    label: '跨 namespace',
    hint: '越过了服务边界，通常应由策略明确约束。',
  },
  {
    key: 'SAME_NAMESPACE',
    label: '同 namespace',
    hint: '多数属于正常架构，列出以便核对。',
  },
]

export default function SecurityPage({ cluster }: { cluster: string }) {
  const { data: rep, error, loading } = useResource(cluster, () => api.security(cluster))

  // 标题与数据来源一起提到早退分支之前，理由见 DataSourceNotice：来源标识
  // 必须与内容同屏，包括这一屏读不到数据的时候（design doc 2026-08-17 §2）。
  const head = (
    <>
      <PageHeader
        title="安全发现"
        description="按风险位置分组的可疑连接、公网出向目标，以及未被任何策略选中的 Pod。被策略拒绝的连接同样列出 —— 挡住了不等于没有人在连。"
      />
      <DataSourceNotice />
    </>
  )

  if (error) return <div>{head}<p className="text-deny">{error}</p></div>
  if (loading || !rep) return <div>{head}<p className="text-ink-muted">加载中…</p></div>

  return (
    <div>
      {head}

      <RiskySection rep={rep} />
      <EgressSection rep={rep} />
      <NakedSection rep={rep} />
    </div>
  )
}

/**
 * 一个清单的条数与截断说明，放在区块标题右侧的 meta 位。
 *
 * 句式与结构都照搬 FlowsPage 的 total / limit 那一处：条数是正文、截断是
 * 一句必须被读到的补充，用 UNKNOWN 的琥珀色强调。同一件事两种写法迟早
 * 各自漂一点，而读的人要靠它们说出同一句话（design doc 2026-08-17 §3）。
 *
 * 截断时一并给出处置：缩小时间窗对前两个清单有用，对「无策略覆盖的 Pod」
 * 无用——那份清单来自锚点快照，与流量窗口无关。哪一条适用由
 * listTruncationView 决定，这里不做判断。
 */
function TruncationMeta({ view }: { view: ListTruncationView }) {
  return (
    <>
      {view.countText}
      {view.truncated && (
        <strong style={{ color: 'var(--verdict-unknown)', marginLeft: 6 }}>
          （已按上限截断）{view.remedy}
        </strong>
      )}
    </>
  )
}

function RiskySection({ rep }: { rep: SecurityReport }) {
  const groups = POSITIONS
    .map((p) => ({ ...p, rows: rep.riskyFlows.filter((f) => f.position === p.key) }))
    .filter((g) => g.rows.length > 0)

  return (
    <Section
      title="高风险端口连接"
      description="风险来自端口背后的协议语义，不来自端口是否常见。同一个端口在不同位置的处置方式完全不同，因此按位置分组，不合成单一评分。"
      meta={<TruncationMeta view={listTruncationView('RISKY_FLOWS', rep.riskyFlows.length, rep.riskyFlowsTruncation)} />}
    >
      {groups.length === 0 ? (
        <EmptyState
          message="本集群未发现落在高风险端口上的连接。"
          detail={
            <>
              判定所用的端口清单（共 {rep.riskPortCatalog.length} 个）：
              {rep.riskPortCatalog.map((p) => `${p.name}:${p.port}`).join('、')}。
              列出清单是为了让这句"未发现"可被检验 —— 否则它与"根本没查"无法区分。
            </>
          }
        />
      ) : (
        groups.map((g) => (
          <div key={g.key} className="mb-4">
            <div style={{
              display: 'flex', alignItems: 'baseline', gap: 'var(--space-2)',
              marginBottom: 'var(--space-2)', flexWrap: 'wrap',
            }}>
              <strong className="text-sm">{g.label}</strong>
              <span className="text-xs text-ink-muted">
                {g.rows.length} 条 · {g.hint}
              </span>
            </div>
            <RiskyTable rows={g.rows} />
          </div>
        ))
      )}
    </Section>
  )
}

function RiskyTable({ rows }: { rows: RiskyFlow[] }) {
  return (
    <TableCard>
      <thead>
        <tr>
          <th>判定</th>
          <th>端口</th>
          <th>源</th>
          <th>目的</th>
        </tr>
      </thead>
      <tbody>
        {rows.map((f) => (
          <tr key={f.id}>
            <td><VerdictBadge verdict={f.verdict} confidence={f.confidence} /></td>
            <td>
              <span style={{ fontWeight: 500 }}>{f.portName}</span>
              <span className="mono" style={{ marginLeft: 6 }}>:{f.port}</span>
              <span style={{ marginLeft: 8 }}>
                <Chip>{RISK_CATEGORY_LABEL[f.category] ?? f.category}</Chip>
              </span>
            </td>
            <td className="mono text-sm">{f.sourceLabel}</td>
            <td className="mono text-sm">{f.destLabel}</td>
          </tr>
        ))}
      </tbody>
    </TableCard>
  )
}

function EgressSection({ rep }: { rep: SecurityReport }) {
  return (
    <Section
      title="公网出向目标"
      description="离开集群的连接去了哪里。放行数与总数分列 —— 一条畅通的外联与一条已被策略挡住的外联，只报总数会长得一样。"
      meta={<TruncationMeta view={listTruncationView('EGRESS_TARGETS', rep.egressTargets.length, rep.egressTargetsTruncation)} />}
    >
      {rep.egressTargets.length === 0 ? (
        <EmptyState
          message="本集群没有出公网流量。"
          detail="判定依据：目的端既无 Pod 身份，也不属于任何已注册集群。"
        />
      ) : (
        <TableCard>
          <thead>
            <tr>
              <th>目标</th>
              <th>端口</th>
              <th className="num">流量</th>
              <th className="num">其中放行</th>
              <th className="num">无法判定</th>
            </tr>
          </thead>
          <tbody>
            {rep.egressTargets.map((t) => (
              <tr key={t.address}>
                <td className="mono text-sm">{t.address}</td>
                <td>{t.ports.map((p) => <Chip key={p}>{p}</Chip>)}</td>
                <td className="num">{t.flowCount}</td>
                <td className="num" style={{
                  color: t.allowedCount > 0 ? 'var(--verdict-allow)' : undefined,
                  fontWeight: t.allowedCount > 0 ? 600 : 400,
                }}>
                  {t.allowedCount}
                </td>
                <td className="num" style={{
                  color: t.unknownCount > 0 ? 'var(--verdict-unknown)' : undefined,
                  fontWeight: t.unknownCount > 0 ? 600 : 400,
                }}>
                  {t.unknownCount}
                </td>
              </tr>
            ))}
          </tbody>
        </TableCard>
      )}
    </Section>
  )
}

function NakedSection({ rep }: { rep: SecurityReport }) {
  return (
    <Section
      title="无策略覆盖的 Pod"
      description="来自资产快照，不受上方时间窗约束。hostNetwork Pod 不在此列 —— 那是 NetworkPolicy 管不到，与没被策略选中是两回事。"
      meta={<TruncationMeta view={listTruncationView('NAKED_PODS', rep.nakedPods.length, rep.nakedPodsTruncation)} />}
    >
      {rep.nakedPods.length === 0 ? (
        <EmptyState
          message="本集群所有 Pod 都被至少一条策略选中。"
          detail="统计口径：已排除 hostNetwork Pod。"
        />
      ) : (
        <Card style={{
          padding: 'var(--space-3)', display: 'flex',
          flexWrap: 'wrap', gap: 'var(--space-2)',
        }}>
          {rep.nakedPods.map((p) => (
            <Chip key={`${p.namespace}/${p.name}`}>
              {p.namespace}/{p.name}
            </Chip>
          ))}
        </Card>
      )}
    </Section>
  )
}
