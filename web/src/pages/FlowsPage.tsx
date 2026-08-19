import { useEffect, useState } from 'react'
import { api } from '../api/client'
import type { Confidence, TimeWindow, Verdict } from '../api/types'
import { useResource } from '../api/useResource'
import DataSourceNotice from '../components/DataSourceNotice'
import DecisionDrawer from '../components/DecisionDrawer'
import { CrossClusterMark, UnmanagedMark, VerdictBadge } from '../components/Verdict'
import { Chip, Field, PageHeader, Section, Select, TableCard, Toolbar } from '../components/ui'

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
  const [pageIndex, setPageIndex] = useState(0)

  // key 编码了这次请求依赖的全部筛选条件：集群、判定、可信度任意一个
  // 变了都必须作废飞行中的旧请求，否则慢的旧响应可能落在快的新响应
  // 之后，把已经切换好的筛选结果重新覆盖回旧数据。
  const key = cluster ? `${cluster}|${verdict}|${confidence}` : ''
  // 筛选条件变了要回到第一页：停在旧页码上会展示一个空列表，
  // 读者会把它读成"这个筛选没有结果"。
  useEffect(() => { setPageIndex(0) }, [key])

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

      {/* 来源标识在内容区、且在筛选与列表之前：截图与转发带走的是这一块，
          不是侧边栏（design doc 2026-08-17 §2）。 */}
      <DataSourceNotice />

      <Toolbar>
        <Field label="判定">
          <Select value={verdict} ariaLabel="判定" onChange={(v) => setVerdict(v as Verdict | '')}
            options={[['', '全部'], ['ALLOW', '放行'], ['DENY', '阻断'], ['UNKNOWN', '无法判定']]} />
        </Field>
        <Field label="可信度">
          <Select value={confidence} ariaLabel="可信度" onChange={(v) => setConfidence(v as Confidence | '')}
            options={[['', '全部'], ['TRUSTED', '可信'], ['DEGRADED', '降级']]} />
        </Field>
      </Toolbar>

      {error && <p className="text-deny">{error}</p>}
      {!page && !error && <p className="text-ink-muted">加载中…</p>}

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
                共 {page.total} 条，已取回 {page.returned} 条
                {/*
                  分页是展示手段，截断是数据边界，两者必须分开说。
                  把"本页 50 条"和"服务端只给了 500 条"混成一句，
                  读者会以为翻到底就看全了。
                */}
                {page.returned < page.total && (
                  <strong style={{ color: 'var(--verdict-unknown)', marginLeft: 6 }}>
                    （已按上限 {page.limit} 截断，尚有 {page.total - page.returned} 条未取回）
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
                {page.items.slice(pageIndex * PAGE_SIZE, (pageIndex + 1) * PAGE_SIZE).map((f) => (
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
                      <span className="flex flex-wrap gap-1">
                        {f.crossCluster && <CrossClusterMark />}
                        {f.unmanaged && <UnmanagedMark />}
                      </span>
                    </td>
                  </tr>
                ))}
              </tbody>
            </TableCard>

            <Pager
              total={page.returned}
              index={pageIndex}
              onChange={setPageIndex}
            />
          </Section>
        </>
      )}

      {selected && <DecisionDrawer flowID={selected} onClose={() => setSelected(null)} />}
    </div>
  )
}


const PAGE_SIZE = 50

/**
 * 分页控件。
 *
 * 一次铺开两百多行会让页面高达两万像素，滚动条本身失去参考意义，
 * 读者也无法判断自己看到了哪一段。分页把"位置"重新变成可读的信息。
 */
function Pager({ total, index, onChange }: {
  total: number; index: number; onChange: (i: number) => void
}) {
  const pages = Math.max(1, Math.ceil(total / PAGE_SIZE))
  if (pages <= 1) return null
  const from = index * PAGE_SIZE + 1
  const to = Math.min((index + 1) * PAGE_SIZE, total)

  return (
    <div style={{
      display: 'flex', alignItems: 'center', gap: 'var(--space-3)',
      marginTop: 'var(--space-3)', fontSize: 'var(--text-sm)', color: 'var(--text-muted)',
    }}>
      <span>第 {from}–{to} 条（本次取回 {total} 条）</span>
      <span style={{ display: 'flex', gap: 'var(--space-2)', marginLeft: 'auto' }}>
        <PagerButton disabled={index === 0} onClick={() => onChange(index - 1)}>上一页</PagerButton>
        <Chip>{index + 1} / {pages}</Chip>
        <PagerButton disabled={index >= pages - 1} onClick={() => onChange(index + 1)}>下一页</PagerButton>
      </span>
    </div>
  )
}

function PagerButton({ disabled, onClick, children }: {
  disabled: boolean; onClick: () => void; children: React.ReactNode
}) {
  return (
    <button
      disabled={disabled}
      onClick={onClick}
      style={{
        padding: '4px 10px', fontSize: 'var(--text-sm)',
        border: '1px solid var(--border)', borderRadius: 'var(--radius-sm)',
        background: 'var(--surface)', cursor: disabled ? 'default' : 'pointer',
        color: disabled ? 'var(--text-muted)' : 'var(--text-secondary)',
        opacity: disabled ? 0.5 : 1,
      }}
    >
      {children}
    </button>
  )
}

