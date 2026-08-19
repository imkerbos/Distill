import { api } from '../api/client'
import { UNKNOWN_REASON_LABEL } from '../api/types'
import { useResource } from '../api/useResource'
import DataSourceNotice from '../components/DataSourceNotice'
import { EmptyState, PageHeader, Section, StatTile, TableCard } from '../components/ui'

const pct = (v: number) => `${(v * 100).toFixed(1)}%`

export default function QualityPage({ cluster }: { cluster: string }) {
  const { data: q, error, loading } = useResource(cluster, () => api.quality(cluster))

  // 标题与数据来源一起提到早退分支之前：来源标识必须与内容同屏，包括这一
  // 屏读不到数据的时候——一句"加载失败"同样要说清它说的是哪一种集群
  // （design doc 2026-08-17 §2）。
  const head = (
    <>
      <PageHeader
        title="数据质量"
        description="平台能力边界的如实报告。覆盖率与无法判定比例必须同屏 —— 单独展示一个好看的覆盖率，会让人以为剩下的部分都是安全的。"
      />
      <DataSourceNotice />
    </>
  )

  if (error) return <div>{head}<p className="text-deny">{error}</p></div>
  if (loading || !q) return <div>{head}<p className="text-ink-muted">加载中…</p></div>

  return (
    <div>
      {head}

            <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(180px, 1fr))', gap: 'var(--space-3)' }}>
        <StatTile label="策略覆盖率" value={pct(q.policyCoverage)}
          note={`${q.nakedPodCount} 个 Pod 无任何策略`} />
        <StatTile label="可信判定占比" value={pct(q.trustedRate)} />
        <StatTile label="无法判定" value={`${q.unknownCount} 条`} tone="unknown"
          note={pct(q.unknownRate)} />
        <StatTile label="可信度降级" value={pct(q.degradedRate)} tone="degraded" />
        <StatTile label="跨集群敞口" value={`${q.crossClusterCount} 条`}
          note="当前版本不做管控" />
        <StatTile label="不受管控 Pod" value={`${q.unmanagedPodCount} 个`}
          note="hostNetwork，已排除出覆盖率" />
      </div>

      <div className="mt-5">
        <Section
          title="无法判定的构成"
          description={`只报一个比例无法告诉你该去修哪个子系统。下面是这 ${q.unknownCount} 条的具体成因。`}
        >
          {Object.keys(q.unknownComposition).length === 0 ? (
            <EmptyState
              message="本集群没有无法判定的流量。"
              detail="该集群的全部流量都得到了明确结论；这不代表结论都可信，可信度见上方降级比例。"
            />
          ) : (
            <TableCard>
              <thead>
                <tr>
                  <th>成因</th>
                  <th>枚举值</th>
                  <th className="num">条数</th>
                </tr>
              </thead>
              <tbody>
                {Object.entries(q.unknownComposition)
                  .sort((a, b) => b[1] - a[1])
                  .map(([reason, count]) => (
                    <tr key={reason}>
                      <td>{UNKNOWN_REASON_LABEL[reason] ?? reason}</td>
                      {/* 原始枚举值与中文标签并列：未收录的取值必须照原样显示，
                          不得因为没有标签就消失 —— unknown_reason 是封闭枚举，
                          少显示一种成因等于把一类系统性问题藏起来。 */}
                      <td className="mono">{reason}</td>
                      <td className="num" style={{ fontWeight: 600 }}>{count}</td>
                    </tr>
                  ))}
              </tbody>
            </TableCard>
          )}
        </Section>
      </div>

      <p style={{ marginTop: 'var(--space-5)', fontSize: 12, color: 'var(--text-muted)' }}>
        本集群共 {q.totalFlows} 条流量参与判定。跨集群流量在其涉及的两个集群中都会计入，
        因此各集群的总数之和会大于全局流量数。
      </p>
    </div>
  )
}

