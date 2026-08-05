import { useState } from 'react'
import { api } from '../api/client'
import type { Confidence, TimeWindow, Verdict } from '../api/types'
import { useResource } from '../api/useResource'
import DecisionDrawer from '../components/DecisionDrawer'
import { CrossClusterMark, UnmanagedMark, VerdictBadge } from '../components/Verdict'
import { PageHeader, Section, TableCard, Toolbar } from '../components/ui'

/**
 * 把时间窗格式化成可读区间。用 UTC 而非本地时区：判定与快照都以 UTC
 * 落库，界面按浏览器时区显示会让人对着两套时间核对同一条流量。
 */
function formatWindow(w: TimeWindow): string {
  const fmt = (iso: string) =>
    new Date(iso).toISOString().replace('T', ' ').replace(/\.\d+Z$/, ' UTC')
  return `${fmt(w.from)} — ${fmt(w.to)}`
}

export default function FlowsPage({ cluster }: { cluster: string }) {
  const [verdict, setVerdict] = useState<Verdict | ''>('')
  const [confidence, setConfidence] = useState<Confidence | ''>('')
  const [selected, setSelected] = useState<string | null>(null)

  // key 编码了这次请求依赖的全部筛选条件：集群、判定、可信度任意一个
  // 变了都必须作废飞行中的旧请求，否则慢的旧响应可能落在快的新响应
  // 之后，把已经切换好的筛选结果重新覆盖回旧数据。
  const key = cluster ? `${cluster}|${verdict}|${confidence}` : ''
  const { data: page, error } = useResource(key, () => api.flows({
    cluster,
    verdict: verdict || undefined,
    confidence: confidence || undefined,
  }))

  return (
    <div>
      <PageHeader
        title="流量与判定"
        description="点击任意一行，查看求值引擎当场算出的判定与理由。判定与可信度是两个独立维度，可分别筛选。"
      />

      <Toolbar>
        <Select label="判定" value={verdict} onChange={(v) => setVerdict(v as Verdict | '')}
          options={[['', '全部'], ['ALLOW', '放行'], ['DENY', '阻断'], ['UNKNOWN', '无法判定']]} />
        <Select label="可信度" value={confidence} onChange={(v) => setConfidence(v as Confidence | '')}
          options={[['', '全部'], ['TRUSTED', '可信'], ['DEGRADED', '降级']]} />
      </Toolbar>

      {error && <p style={{ color: 'var(--verdict-deny)' }}>{error}</p>}
      {!page && !error && <p style={{ color: 'var(--text-muted)' }}>加载中…</p>}

      {page && (
        <>
          <Section
            title="流量"
            meta={
              <>
                {/*
                  截断与时间窗必须与列表同屏。只给一个数组、不给 total，
                  界面就无法区分"这就是全部"与"只给了你一部分"；按时间
                  筛过却不说明筛的是哪一段，同理。
                */}
                共 {page.total} 条，已显示 {page.returned} 条
                {page.returned < page.total && (
                  <strong style={{ color: 'var(--verdict-unknown)', marginLeft: 6 }}>
                    （已按上限 {page.limit} 截断，尚有 {page.total - page.returned} 条未显示）
                  </strong>
                )}
                <span style={{ marginLeft: 10 }}>· 时间范围 {formatWindow(page.window)}</span>
              </>
            }
          >
            <TableCard>
              <thead>
                <tr>
                  <th>判定</th><th>源</th><th>目的</th><th>端口</th><th>标记</th>
                </tr>
              </thead>
              <tbody>
                {page.items.map((f) => (
                  <tr
                    key={f.id}
                    className="clickable"
                    onClick={() => setSelected(f.id)}
                    style={{ background: selected === f.id ? 'var(--surface-sunken)' : undefined }}
                  >
                    <td><VerdictBadge verdict={f.verdict} confidence={f.confidence} /></td>
                    <td className="mono">{f.sourceLabel}</td>
                    <td className="mono">{f.destLabel}</td>
                    <td className="num">{f.protocol}:{f.port}</td>
                    <td>
                      <span style={{ display: 'flex', gap: 4, flexWrap: 'wrap' }}>
                        {f.crossCluster && <CrossClusterMark />}
                        {f.unmanaged && <UnmanagedMark />}
                      </span>
                    </td>
                  </tr>
                ))}
              </tbody>
            </TableCard>
          </Section>
        </>
      )}

      {selected && <DecisionDrawer flowID={selected} onClose={() => setSelected(null)} />}
    </div>
  )
}


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
