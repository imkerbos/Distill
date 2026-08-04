import { useEffect, useState } from 'react'
import { api } from '../api/client'
import type { Confidence, FlowPage, Verdict } from '../api/types'
import DecisionDrawer from '../components/DecisionDrawer'
import { CrossClusterMark, UnmanagedMark, VerdictBadge } from '../components/Verdict'

export default function FlowsPage({ cluster }: { cluster: string }) {
  const [page, setPage] = useState<FlowPage | null>(null)
  const [verdict, setVerdict] = useState<Verdict | ''>('')
  const [confidence, setConfidence] = useState<Confidence | ''>('')
  const [selected, setSelected] = useState<string | null>(null)
  const [error, setError] = useState('')

  useEffect(() => {
    if (!cluster) return
    setPage(null); setError('')
    api.flows({
      cluster,
      verdict: verdict || undefined,
      confidence: confidence || undefined,
    }).then(setPage).catch((e) => setError(String(e.message ?? e)))
  }, [cluster, verdict, confidence])

  return (
    <div>
      <h2 style={{ marginTop: 0 }}>流量与判定</h2>

      <div style={{ display: 'flex', gap: 'var(--space-3)', marginBottom: 'var(--space-3)' }}>
        <Select label="判定" value={verdict} onChange={(v) => setVerdict(v as Verdict | '')}
          options={[['', '全部'], ['ALLOW', '放行'], ['DENY', '阻断'], ['UNKNOWN', '无法判定']]} />
        <Select label="可信度" value={confidence} onChange={(v) => setConfidence(v as Confidence | '')}
          options={[['', '全部'], ['TRUSTED', '可信'], ['DEGRADED', '降级']]} />
      </div>

      {error && <p style={{ color: 'var(--verdict-deny)' }}>{error}</p>}
      {!page && !error && <p style={{ color: 'var(--text-muted)' }}>加载中…</p>}

      {page && (
        <>
          {/*
            截断必须可见。只给一个数组、不给 total，界面就无法区分
            "这就是全部" 与 "只给了你一部分"，而后者恰恰会让人以为
            平台什么都知道。
          */}
          <p style={{ fontSize: 13, color: 'var(--text-muted)', marginTop: 0 }}>
            共 {page.total} 条，已显示 {page.returned} 条
            {page.returned < page.total && (
              <strong style={{ color: 'var(--verdict-unknown)' }}>
                （已按上限 {page.limit} 截断，尚有 {page.total - page.returned} 条未显示）
              </strong>
            )}
          </p>

          <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 13 }}>
            <thead>
              <tr style={{ textAlign: 'left', color: 'var(--text-muted)', fontSize: 12 }}>
                <th style={th}>判定</th><th style={th}>源</th><th style={th}>目的</th>
                <th style={th}>端口</th><th style={th}>标记</th>
              </tr>
            </thead>
            <tbody>
              {page.items.map((f) => (
                <tr
                  key={f.id}
                  onClick={() => setSelected(f.id)}
                  style={{
                    cursor: 'pointer', borderTop: '1px solid var(--border)',
                    background: selected === f.id ? 'var(--bg)' : undefined,
                  }}
                >
                  <td style={td}><VerdictBadge verdict={f.verdict} confidence={f.confidence} /></td>
                  <td style={{ ...td, fontFamily: 'var(--mono)', fontSize: 11 }}>{f.sourceLabel}</td>
                  <td style={{ ...td, fontFamily: 'var(--mono)', fontSize: 11 }}>{f.destLabel}</td>
                  <td style={td}>{f.protocol}:{f.port}</td>
                  <td style={{ ...td, display: 'flex', gap: 4, flexWrap: 'wrap' }}>
                    {f.crossCluster && <CrossClusterMark />}
                    {f.unmanaged && <UnmanagedMark />}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </>
      )}

      {selected && <DecisionDrawer flowID={selected} onClose={() => setSelected(null)} />}
    </div>
  )
}

const th: React.CSSProperties = { padding: '6px 8px', fontWeight: 500 }
const td: React.CSSProperties = { padding: '8px', verticalAlign: 'top' }

function Select({ label, value, onChange, options }: {
  label: string; value: string; onChange: (v: string) => void; options: [string, string][]
}) {
  return (
    <label style={{ fontSize: 13 }}>
      <span style={{ color: 'var(--text-muted)', marginRight: 6 }}>{label}</span>
      <select value={value} onChange={(e) => onChange(e.target.value)} style={{
        padding: '4px 8px', border: '1px solid var(--border)',
        borderRadius: 'var(--radius)', background: 'var(--surface)', fontSize: 13,
      }}>
        {options.map(([v, l]) => <option key={v} value={v}>{l}</option>)}
      </select>
    </label>
  )
}
