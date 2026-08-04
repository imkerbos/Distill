import { useEffect, useState } from 'react'
import { api } from '../api/client'
import type { Decision } from '../api/types'
import { CrossClusterMark, UnmanagedMark, VerdictBadge } from './Verdict'

const REASON_LABEL: Record<string, string> = {
  POLICY_MALFORMED: '策略本身无法解析',
  SNAPSHOT_MISSING: '缺少对应时刻的资产快照',
  IP_AMBIGUOUS: '同集群内 IP 复用，时间上不可区分',
  CLUSTER_AMBIGUOUS: '跨集群网段重叠，归属不唯一',
  IDENTITY_LOST_MESH: 'sidecar 导致源身份丢失',
  CCNP_PRESENT: '存在 Cilium 策略，标准 NetworkPolicy 结论不可靠',
  NAT_TRANSLATED: '地址被转换，无法还原原始主体',
  EXTERNAL_NO_IDENTITY: '公网流量无可归属主体',
  NAMED_PORT_UNRESOLVED: '命名端口无法解析为具体端口号',
  LOG_SAMPLED_OUT: '日志采样或限流导致记录缺失',
  UNSPECIFIED: '未记录具体原因',
}

export default function DecisionDrawer({ flowID, onClose }: { flowID: string; onClose: () => void }) {
  const [d, setD] = useState<Decision | null>(null)
  const [error, setError] = useState('')

  useEffect(() => {
    setD(null); setError('')
    api.decision(flowID).then(setD).catch((e) => setError(String(e.message ?? e)))
  }, [flowID])

  return (
    <aside style={{
      position: 'fixed', top: 0, right: 0, bottom: 0, width: 460,
      background: 'var(--surface)', borderLeft: '1px solid var(--border)',
      padding: 'var(--space-4)', overflow: 'auto', boxShadow: '-2px 0 12px rgba(0,0,0,.06)',
    }}>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
        <h3 style={{ margin: 0, fontSize: 15 }}>判定解释</h3>
        <button onClick={onClose} style={{
          border: '1px solid var(--border)', background: 'transparent',
          borderRadius: 'var(--radius)', padding: '2px 8px', cursor: 'pointer',
        }}>关闭</button>
      </div>

      {error && <p style={{ color: 'var(--verdict-deny)' }}>{error}</p>}
      {!d && !error && <p style={{ color: 'var(--text-muted)' }}>加载中…</p>}

      {d && (
        <>
          <div style={{ margin: 'var(--space-3) 0', display: 'flex', gap: 6, flexWrap: 'wrap' }}>
            <VerdictBadge verdict={d.verdict} confidence={d.confidence} />
            {d.crossCluster && <CrossClusterMark />}
            {d.unmanaged && <UnmanagedMark />}
          </div>

          <Field label="流量">
            <div style={{ fontFamily: 'var(--mono)', fontSize: 12, lineHeight: 1.7 }}>
              <div>{d.sourceLabel}</div>
              <div style={{ color: 'var(--text-muted)' }}>↓ {d.protocol} :{d.port}</div>
              <div>{d.destLabel}</div>
            </div>
          </Field>

          {d.verdict === 'UNKNOWN' && (
            <Field label="为什么无法判定">
              <div style={{ color: 'var(--verdict-unknown)', fontWeight: 500 }}>
                {REASON_LABEL[d.unknownReason] ?? d.unknownReason}
              </div>
              {d.reason.detail && (
                <pre style={{
                  marginTop: 'var(--space-2)', padding: 'var(--space-2)',
                  background: 'var(--bg)', border: '1px solid var(--border)',
                  borderRadius: 'var(--radius)', fontSize: 11, whiteSpace: 'pre-wrap',
                  fontFamily: 'var(--mono)',
                }}>{d.reason.detail}</pre>
              )}
            </Field>
          )}

          {d.verdict === 'ALLOW' && d.reason.matchedPolicy && (
            <Field label="被哪条规则放行">
              <div style={{ fontFamily: 'var(--mono)', fontSize: 12 }}>
                {d.reason.matchedPolicy} 的第 {d.reason.matchedRuleIdx} 条规则
              </div>
            </Field>
          )}

          {d.verdict === 'ALLOW' && !d.reason.matchedPolicy && (
            <Field label="为什么放行">
              <div style={{ fontSize: 13, color: 'var(--text-muted)' }}>
                {d.reason.unmanaged
                  ? '端点不受 NetworkPolicy 管控（hostNetwork），策略对它不生效 —— 这不是被策略放行。'
                  : '该方向未被任何 NetworkPolicy 选中，处于非隔离状态，默认放行。'}
              </div>
            </Field>
          )}

          {d.verdict === 'DENY' && (
            <Field label="为什么阻断">
              <div style={{ fontSize: 13 }}>
                {d.reason.isolated
                  ? `${d.reason.direction === 'EGRESS' ? '出向' : '入向'}已被策略隔离，且没有任何规则匹配这条流量。`
                  : '策略判定为阻断。'}
                {d.crossCluster && ' 跨集群对端只能靠 ipBlock 匹配，当前没有覆盖它的规则。'}
              </div>
            </Field>
          )}

          <Field label="判定路径">
            <dl style={{ margin: 0, fontSize: 12, display: 'grid', gridTemplateColumns: 'auto 1fr', gap: '4px 12px' }}>
              <dt style={{ color: 'var(--text-muted)' }}>决定方向</dt>
              <dd style={{ margin: 0 }}>{d.reason.direction || '—'}</dd>
              <dt style={{ color: 'var(--text-muted)' }}>是否隔离</dt>
              <dd style={{ margin: 0 }}>{d.reason.isolated ? '是' : '否'}</dd>
              <dt style={{ color: 'var(--text-muted)' }}>不受管控</dt>
              <dd style={{ margin: 0 }}>{d.reason.unmanaged ? '是' : '否'}</dd>
              <dt style={{ color: 'var(--text-muted)' }}>可信度</dt>
              <dd style={{ margin: 0 }}>{d.confidence === 'DEGRADED' ? '降级' : '可信'}</dd>
            </dl>
          </Field>
        </>
      )}
    </aside>
  )
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <section style={{ marginBottom: 'var(--space-4)' }}>
      <div style={{ fontSize: 12, color: 'var(--text-muted)', marginBottom: 'var(--space-1)' }}>{label}</div>
      {children}
    </section>
  )
}
