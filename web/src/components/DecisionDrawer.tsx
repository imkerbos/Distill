import { api } from '../api/client'
import { UNKNOWN_REASON_LABEL } from '../api/types'
import { useResource } from '../api/useResource'
import { CrossClusterMark, UnmanagedMark, VerdictBadge } from './Verdict'

/**
 * 判定解释抽屉 —— 全平台最需要被信任的一屏。
 *
 * 它回答的是"引擎凭什么给出这个结论"。因此结构固定为三段：这条流量是什么、
 * 结论为什么成立、判定过程中的关键状态。三段都必须在，即使某段无话可说 ——
 * 一个只在有内容时才出现的字段，会让读者以为平台每次都想清楚了。
 */
export default function DecisionDrawer({ flowID, onClose }: { flowID: string; onClose: () => void }) {
  const { data: d, error } = useResource(flowID, () => api.decision(flowID))

  return (
    <>
      {/*
        遮罩：抽屉直接压在表格上时，被盖住的那几列看起来像"数据缺了"。
        压暗背景让读者知道那是被临时遮挡，不是内容不全。
      */}
      <div
        onClick={onClose}
        style={{
          position: 'fixed', inset: 0, background: 'rgba(28,28,26,.28)', zIndex: 10,
        }}
      />
      <aside style={{
        position: 'fixed', top: 0, right: 0, bottom: 0, width: 480, zIndex: 11,
        background: 'var(--bg)', borderLeft: '1px solid var(--border)',
        overflow: 'auto', boxShadow: '-4px 0 24px rgba(28,28,26,.10)',
        display: 'flex', flexDirection: 'column',
      }}>
        <header style={{
          position: 'sticky', top: 0, background: 'var(--surface)',
          borderBottom: '1px solid var(--border)', padding: 'var(--space-3) var(--space-4)',
          display: 'flex', justifyContent: 'space-between', alignItems: 'center',
        }}>
          <h2 style={{ margin: 0, fontSize: 'var(--text-lg)', fontWeight: 600 }}>判定解释</h2>
          <button onClick={onClose} style={{
            border: '1px solid var(--border)', background: 'var(--surface)',
            borderRadius: 'var(--radius-sm)', padding: '4px 10px', cursor: 'pointer',
            fontSize: 'var(--text-sm)', color: 'var(--text-secondary)',
          }}>关闭</button>
        </header>

        <div className="p-4">
          {error && <p className="text-deny">{error}</p>}
          {!d && !error && <p className="text-ink-muted">加载中…</p>}

          {d && (
            <>
              <div style={{
                display: 'flex', gap: 6, flexWrap: 'wrap', marginBottom: 'var(--space-3)',
              }}>
                <VerdictBadge verdict={d.verdict} confidence={d.confidence} />
                {d.crossCluster && <CrossClusterMark />}
                {d.unmanaged && <UnmanagedMark />}
              </div>

              <Block label="流量">
                <div style={{ fontFamily: 'var(--mono)', fontSize: 'var(--text-xs)', lineHeight: 1.9 }}>
                  <div>{d.sourceLabel}</div>
                  <div className="text-ink-muted">↓ {d.protocol} :{d.port}</div>
                  <div>{d.destLabel}</div>
                </div>
              </Block>

              {d.verdict === 'UNKNOWN' && (
                <Block label="为什么无法判定">
                  <div style={{ color: 'var(--verdict-unknown)', fontWeight: 600 }}>
                    {UNKNOWN_REASON_LABEL[d.unknownReason] ?? d.unknownReason}
                  </div>
                  {/*
                    detail 是这一屏的落点：它把"不知道"变成"不知道，而且这是
                    卡住的确切位置"。没有它，UNKNOWN 与推脱无法区分。
                  */}
                  {d.reason.detail && (
                    <pre style={{
                      margin: 'var(--space-2) 0 0', padding: 'var(--space-3)',
                      background: 'var(--surface-sunken)', border: '1px solid var(--border)',
                      borderRadius: 'var(--radius-sm)', fontSize: 'var(--text-xs)',
                      whiteSpace: 'pre-wrap', wordBreak: 'break-all',
                      fontFamily: 'var(--mono)', lineHeight: 1.7, color: 'var(--text-secondary)',
                    }}>{d.reason.detail}</pre>
                  )}
                </Block>
              )}

              {d.verdict === 'ALLOW' && d.reason.matchedPolicy && (
                <Block label="被哪条规则放行">
                  <div style={{ fontFamily: 'var(--mono)', fontSize: 'var(--text-xs)' }}>
                    {d.reason.matchedPolicy}
                  </div>
                  <div style={{ fontSize: 'var(--text-sm)', color: 'var(--text-muted)', marginTop: 4 }}>
                    第 {d.reason.matchedRuleIdx} 条规则
                  </div>
                </Block>
              )}

              {d.verdict === 'ALLOW' && !d.reason.matchedPolicy && (
                <Block label="为什么放行">
                  <div className="text-sm">
                    {d.reason.unmanaged
                      ? '端点不受 NetworkPolicy 管控（hostNetwork），策略对它不生效 —— 这不是被策略放行。'
                      : '该方向未被任何 NetworkPolicy 选中，处于非隔离状态，默认放行。'}
                  </div>
                </Block>
              )}

              {d.verdict === 'DENY' && (
                <Block label="为什么阻断">
                  <div className="text-sm">
                    {d.reason.isolated
                      ? `${d.reason.direction === 'EGRESS' ? '出向' : '入向'}已被策略隔离，且没有任何规则匹配这条流量。`
                      : '策略判定为阻断。'}
                    {d.crossCluster && ' 跨集群对端只能靠 ipBlock 匹配，当前没有覆盖它的规则。'}
                  </div>
                </Block>
              )}

              <Block label="判定路径">
                <dl style={{
                  margin: 0, fontSize: 'var(--text-sm)',
                  display: 'grid', gridTemplateColumns: 'max-content 1fr',
                  rowGap: 'var(--space-2)', columnGap: 'var(--space-4)',
                }}>
                  <Term k="决定方向" v={d.reason.direction || '—'} />
                  <Term k="是否隔离" v={d.reason.isolated ? '是' : '否'} />
                  <Term k="不受管控" v={d.reason.unmanaged ? '是' : '否'} />
                  {/*
                    可信度与判定并列展示，不合并：一个"能判定但不可信"的结论
                    必须有地方安放，否则 mesh 与 CCNP 场景会被读成正常结论。
                  */}
                  <Term k="可信度" v={d.confidence === 'DEGRADED' ? '降级' : '可信'} />
                </dl>
              </Block>
            </>
          )}
        </div>
      </aside>
    </>
  )
}

function Block({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <section style={{
      background: 'var(--surface)', border: '1px solid var(--border)',
      borderRadius: 'var(--radius)', padding: 'var(--space-3)',
      marginBottom: 'var(--space-3)',
    }}>
      <div style={{
        fontSize: 'var(--text-xs)', color: 'var(--text-muted)',
        marginBottom: 'var(--space-2)',
      }}>
        {label}
      </div>
      {children}
    </section>
  )
}

function Term({ k, v }: { k: string; v: string }) {
  return (
    <>
      <dt className="text-ink-muted">{k}</dt>
      <dd style={{ margin: 0, fontWeight: 500 }}>{v}</dd>
    </>
  )
}
