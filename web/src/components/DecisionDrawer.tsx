import { api } from '../api/client'
import { UNKNOWN_REASON_LABEL, type DecisionReason } from '../api/types'
import { isAdminPlane, policyRefView } from '../pages/policyRefView'
import { useResource } from '../api/useResource'
import { Drawer } from './radix'
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
    <Drawer open onClose={onClose} title="判定解释">
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
                  <MatchedPolicy reason={d.reason} />
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
                  {/*
                    被管理面策略拦下时必须点名是哪一条。NetworkPolicy 的 DENY
                    还能从「已被策略隔离」推断出该去看哪个命名空间，而 ANP 的
                    DENY 此前只显示一句「策略判定为阻断。」—— 一个操作者看到它，
                    没有任何线索指向那条集群级策略，只会去翻自己命名空间里的
                    YAML，而那里没有东西可改。
                  */}
                  {d.reason.matchedPolicy ? (
                    <MatchedPolicy reason={d.reason} />
                  ) : (
                    <div className="text-sm">
                      {d.reason.isolated
                        ? `${d.reason.direction === 'EGRESS' ? '出向' : '入向'}已被策略隔离，且没有任何规则匹配这条流量。`
                        : '策略判定为阻断。'}
                      {d.crossCluster && ' 跨集群对端只能靠 ipBlock 匹配，当前没有覆盖它的规则。'}
                    </div>
                  )}
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
    </Drawer>
  )
}

/**
 * 命中的那条策略：名字、第几条规则、它属于哪个平面。
 *
 * 放行与阻断共用一个组件：两处各写一份，其中一处迟早会漏掉平面那一行，
 * 而那正是这个组件要修的缺陷 —— 一条 ANP 的命中读起来像一条命名空间里的
 * NetworkPolicy。
 */
function MatchedPolicy({ reason }: { reason: DecisionReason }) {
  const ref = policyRefView(reason.matchedPolicy)
  return (
    <>
      <div style={{ fontFamily: 'var(--mono)', fontSize: 'var(--text-xs)' }}>
        {ref.name}
      </div>
      <div style={{ fontSize: 'var(--text-sm)', color: 'var(--text-muted)', marginTop: 4 }}>
        {ref.planeLabel} · 第 {reason.matchedRuleIdx} 条规则
      </div>
      {/* 平面之间的次序关系只在它不是普通 NetworkPolicy 时才说：对那一种，
          多一句「命名空间级策略」是噪声。 */}
      {isAdminPlane(reason.matchedPolicy) && (
        <div style={{ fontSize: 'var(--text-sm)', color: 'var(--text-muted)', marginTop: 6 }}>
          {ref.precedence}
        </div>
      )}
      {reason.detail && (
        <div style={{
          marginTop: 8, fontFamily: 'var(--mono)', fontSize: 'var(--text-xs)',
          color: 'var(--text-secondary)',
        }}>{reason.detail}</div>
      )}
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
