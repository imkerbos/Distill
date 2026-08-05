import { api } from '../api/client'
import type { RiskCategory, RiskPosition, SecurityReport } from '../api/types'
import { useResource } from '../api/useResource'
import { VerdictBadge } from '../components/Verdict'

const CATEGORY_LABEL: Record<RiskCategory, string> = {
  ADMIN_PLAINTEXT: '明文管理端口',
  DATABASE: '数据库直连',
  FILE_SHARE: '文件共享',
}

const POSITION_LABEL: Record<RiskPosition, string> = {
  EGRESS_INTERNET: '出公网',
  CROSS_NAMESPACE: '跨 namespace',
  SAME_NAMESPACE: '同 namespace',
}

/**
 * 位置决定紧迫度，与端口类别是两个维度。出公网的 SSH 与同 namespace 内的
 * SSH 都是 ADMIN_PLAINTEXT，但前者要立刻处理、后者多半是正常运维通道。
 */
const POSITION_TONE: Record<RiskPosition, string> = {
  EGRESS_INTERNET: 'var(--verdict-deny)',
  CROSS_NAMESPACE: 'var(--verdict-unknown)',
  SAME_NAMESPACE: 'var(--text-muted)',
}

const th: React.CSSProperties = {
  padding: 'var(--space-2)',
  borderBottom: '1px solid var(--border)',
  fontWeight: 500,
}
const td: React.CSSProperties = {
  padding: 'var(--space-2)',
  borderBottom: '1px solid var(--border)',
}

export default function SecurityPage({ cluster }: { cluster: string }) {
  const { data: rep, error, loading } = useResource(cluster, () => api.security(cluster))

  if (error) return <p style={{ color: 'var(--verdict-deny)' }}>{error}</p>
  if (loading || !rep) return <p style={{ color: 'var(--text-muted)' }}>加载中…</p>

  return (
    <div>
      <h2 style={{ marginTop: 0 }}>安全发现</h2>

      <RiskySection rep={rep} />
      <EgressSection rep={rep} />
      <NakedSection rep={rep} />
    </div>
  )
}

function RiskySection({ rep }: { rep: SecurityReport }) {
  return (
    <section>
      <h3 style={{ fontSize: 14, marginBottom: 'var(--space-2)' }}>高风险端口连接</h3>
      <p style={{ fontSize: 13, color: 'var(--text-muted)', marginTop: 0 }}>
        风险来自端口背后的协议语义，不来自端口是否常见。
        {/*
          被 DENY 的连接照样列出：策略这次挡住了，不代表没有人在尝试。
          把它们过滤掉会让报告看起来干净，同时丢掉最该被追问的那条线索。
        */}
        被策略拒绝的连接同样列出 —— 挡住了不等于没有人在连。
      </p>

      {rep.riskyFlows.length === 0 ? (
        <EmptyWithCatalog rep={rep} />
      ) : (
        <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 13 }}>
          <thead>
            <tr style={{ textAlign: 'left', color: 'var(--text-muted)', fontSize: 12 }}>
              <th style={th}>判定</th>
              <th style={th}>位置</th>
              <th style={th}>端口</th>
              <th style={th}>源</th>
              <th style={th}>目的</th>
            </tr>
          </thead>
          <tbody>
            {rep.riskyFlows.map((f) => (
              <tr key={f.id}>
                <td style={td}>
                  <VerdictBadge verdict={f.verdict} confidence={f.confidence} />
                </td>
                <td style={{ ...td, color: POSITION_TONE[f.position], fontWeight: 500 }}>
                  {POSITION_LABEL[f.position] ?? f.position}
                </td>
                <td style={td}>
                  {f.portName} :{f.port}
                  <span style={{ color: 'var(--text-muted)', marginLeft: 6, fontSize: 12 }}>
                    {CATEGORY_LABEL[f.category] ?? f.category}
                  </span>
                </td>
                <td style={td}>{f.sourceLabel}</td>
                <td style={td}>{f.destLabel}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  )
}

/**
 * 空结果必须连同判定依据一起展示。
 *
 * 只显示"未发现高风险连接"，读者无从分辨这是"查过了、干净"还是
 * "根本没查"。把用过的端口清单摆出来，这句话才有分量。
 */
function EmptyWithCatalog({ rep }: { rep: SecurityReport }) {
  return (
    <div style={{ fontSize: 13 }}>
      <p>本集群未发现落在高风险端口上的连接。</p>
      <p style={{ color: 'var(--text-muted)' }}>
        判定所用的端口清单（共 {rep.riskPortCatalog.length} 个）：
        {rep.riskPortCatalog.map((p) => `${p.name}:${p.port}`).join('、')}
      </p>
    </div>
  )
}

function EgressSection({ rep }: { rep: SecurityReport }) {
  return (
    <section style={{ marginTop: 'var(--space-5)' }}>
      <h3 style={{ fontSize: 14, marginBottom: 'var(--space-2)' }}>公网出向目标</h3>
      {rep.egressTargets.length === 0 ? (
        <p style={{ fontSize: 13 }}>本集群没有出公网流量。</p>
      ) : (
        <table style={{ borderCollapse: 'collapse', fontSize: 13, minWidth: 480 }}>
          <thead>
            <tr style={{ textAlign: 'left', color: 'var(--text-muted)', fontSize: 12 }}>
              <th style={th}>目标</th>
              <th style={th}>端口</th>
              <th style={th}>流量</th>
              <th style={th}>其中放行</th>
              <th style={th}>无法判定</th>
            </tr>
          </thead>
          <tbody>
            {rep.egressTargets.map((t) => (
              <tr key={t.address}>
                <td style={td}>{t.address}</td>
                <td style={td}>{t.ports.join('、')}</td>
                <td style={td}>{t.flowCount} 条</td>
                {/*
                  放行数单列。只给总数时，一条畅通的外联与一条已被策略
                  挡住的外联在表里长得完全一样，而这两件事的处置天差地别。
                */}
                <td style={{ ...td, color: t.allowedCount > 0 ? 'var(--verdict-allow)' : undefined }}>
                  {t.allowedCount} 条
                </td>
                <td style={{ ...td, color: t.unknownCount > 0 ? 'var(--verdict-unknown)' : undefined }}>
                  {t.unknownCount} 条
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  )
}

function NakedSection({ rep }: { rep: SecurityReport }) {
  return (
    <section style={{ marginTop: 'var(--space-5)' }}>
      <h3 style={{ fontSize: 14, marginBottom: 'var(--space-2)' }}>
        无策略覆盖的 Pod（{rep.nakedPods.length}）
      </h3>
      <p style={{ fontSize: 13, color: 'var(--text-muted)', marginTop: 0 }}>
        {/*
          时间语义与上面两节不同，必须写出来。否则读者会把它当成
          "这段时间内有 N 个裸奔 Pod"，而它其实来自当前资产快照。
        */}
        来自资产快照，不受上方时间窗约束。hostNetwork Pod 不在此列 ——
        那是 NetworkPolicy 管不到，与没被策略选中是两回事。
      </p>
      {rep.nakedPods.length === 0 ? (
        <p style={{ fontSize: 13 }}>本集群所有 Pod 都被至少一条策略选中。</p>
      ) : (
        <ul style={{ fontSize: 13, margin: 0, paddingLeft: '1.2em' }}>
          {rep.nakedPods.map((p) => (
            <li key={`${p.namespace}/${p.name}`}>
              {p.namespace}/{p.name}
            </li>
          ))}
        </ul>
      )}
    </section>
  )
}
