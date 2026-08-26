import { api } from '../api/client'
import { UNKNOWN_REASON_LABEL } from '../api/types'
import { useResource } from '../api/useResource'
import {
  coverageView, reconcileView, SAMPLES_HELP, SAMPLES_NONE,
  TREND_HELP, TREND_NONE, trendRows,
} from './reconcileView'
import DataSourceNotice from '../components/DataSourceNotice'
import { EmptyState, PageHeader, Section, Skeleton, StatTile, TableCard } from '../components/ui'

const pct = (v: number) => `${(v * 100).toFixed(1)}%`

export default function QualityPage({ cluster }: { cluster: string }) {
  const { data: q, error, loading } = useResource(cluster, () => api.quality(cluster))
  // 一致率与其余质量指标同屏：它回答的是"这一屏的其它数字有多可信"，
  // 放到另一页去看等于没有（design doc 2026-08-25 §3）。
  //
  // 单独一次请求、失败不拖垮这一屏：对账依赖流量来源报不报判定，一个
  // 对不了账的集群仍然要能看到覆盖率与无法判定比例。
  const { data: rec } = useResource(cluster, () => api.reconciliation(cluster))
  const rv = reconcileView(rec ?? null)
  // 趋势再单独一次请求：它读的是历史记录，与当前窗口的对账是两条不同的路，
  // 而一个没有对账历史的部署仍然要能看到这一窗的一致率。
  const { data: trend } = useResource(cluster, () => api.reconciliationTrend(cluster))
  const trendData = trendRows(trend?.points)
  const cov = coverageView(trend?.coverage)

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
  if (loading || !q) return <div>{head}<Skeleton /></div>

  return (
    <div>
      {head}

            <div className="grid grid-cols-[repeat(auto-fit,minmax(180px,1fr))] gap-3">
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

      <Section
        title="平台判定与集群实际执行的一致率"
        description="这是唯一一个能在真实流量上度量的可信度指标：同一批连接，平台回放算出的判定与执行平面自己报的判定差多少。它不回答“规则全不全”，只回答“已经给出的判定准不准”。"
      >
        <div className="grid grid-cols-[repeat(auto-fit,minmax(180px,1fr))] gap-3">
          <StatTile label="一致率" value={rv.rateText}
            note={rv.available ? `${rv.comparable} 条可比对` : '算不出来'} />
          <StatTile label="平台低估放行面" value={`${rv.under.count} 条`} tone="deny"
            note="平台判 DENY、集群实际放行" />
          <StatTile label="平台高估放行面" value={`${rv.over.count} 条`} tone="unknown"
            note="平台判 ALLOW、集群实际拦下" />
          <StatTile label="平台答不出" value={`${rv.platformUnknown} 条`} tone="unknown"
            note="覆盖不足，不是判错" />
        </div>

        {!rv.available && (
          <p className="mt-2 text-sm text-ink-2">{rv.unavailableReason}</p>
        )}

        {/* 两个方向的说明必须跟着数字走：一个没有解释的“低估 3 条”，
            读者不知道该紧张还是该忽略。 */}
        <p className="mt-3 mb-0 text-xs text-deny">{rv.under.help}</p>
        <p className="mt-1 mb-0 text-xs text-ink-2">{rv.over.help}</p>

        {rv.subjects.length > 0 && (
          <div className="mt-3">
            <table className="dt">
            <thead>
              <tr>
                <th>主体</th><th className="num">一致</th><th className="num">低估</th>
                <th className="num">高估</th><th className="num">低估率</th><th>写回</th>
              </tr>
            </thead>
            <tbody>
              {rv.subjects.map((row) => (
                <tr key={row.label} style={{ color: row.blocked ? 'var(--verdict-deny)' : undefined }}>
                  <td>{row.label}</td>
                  <td className="num">{row.agreeCount}</td>
                  <td className="num">{row.underCount}</td>
                  <td className="num">{row.overCount}</td>
                  <td className="num">{row.underRate === null ? '—' : pct(row.underRate)}</td>
                  <td>{row.blocked ? '会被门禁拦下' : ''}</td>
                </tr>
              ))}
              </tbody>
            </table>
          </div>
        )}

        {/* 证据紧跟主体表：上面那张表回答"谁对不上、多少"，这里回答
            "具体是哪几条"。分开放会让操作者拿着一个比率无从下一步，而门禁
            正是按那个比率拦人的。 */}
        {rv.available && (
          <div className="mt-4">
            <h3 className="mb-1 text-sm">分歧证据</h3>
            {rv.samples.length === 0
              ? <p className="mt-0 mb-0 text-xs text-ink-2">{SAMPLES_NONE}</p>
              : (
                <>
                  <table className="dt">
                    <thead>
                      <tr><th>主体</th><th>方向</th><th>连接</th><th>发生时刻</th></tr>
                    </thead>
                    <tbody>
                      {rv.samples.map((row, i) => (
                        <tr
                          key={`${row.subject}/${row.connection}/${i}`}
                          style={{ color: row.blocking ? 'var(--verdict-deny)' : undefined }}
                        >
                          <td>{row.subject}</td>
                          <td>{row.classLabel}</td>
                          <td className="mono">{row.connection}</td>
                          <td>{row.atText}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                  <p className="mt-1 mb-0 text-xs text-ink-2">{SAMPLES_HELP}</p>
                </>
              )}
          </div>
        )}

        {/* 观测覆盖排在走向之前：走向上那些数字算在多长的一段观测上，
            决定了它们值多少。先知道"看了多久"，再读"看出了什么"
            （design doc §6.2a）。 */}
        <div className="mt-4">
          <h3 className="mb-1 text-sm">观测覆盖</h3>
          {!cov.available
            ? <p className="mt-0 mb-0 text-xs text-ink-2">{cov.unavailableReason}</p>
            : (
              <>
                <div className="flex gap-4">
                  <StatTile label="观测跨度" value={cov.spanText} />
                  <StatTile label="实际观测" value={cov.coveredText} />
                  {/* 间隙单独成一块，且在显著时上语义色：它是这一栏的结论，
                      不是一个附注。 */}
                  <StatTile
                    label="其中没有摄入"
                    value={cov.gapText}
                    tone={cov.alarming ? 'deny' : undefined}
                  />
                </div>
                {cov.alarm !== '' && (
                  <p role="alert" className="mt-1 mb-0 text-xs" style={{ color: 'var(--verdict-deny)' }}>
                    {cov.alarm}
                  </p>
                )}
              </>
            )}
        </div>

        {/* 走向排在最后：先看这一轮怎么样、分歧在谁身上、具体哪几条，
            再看它是在变好还是变坏。绝对值没有行动含义，走向才有。 */}
        <div className="mt-4">
          <h3 className="mb-1 text-sm">一致率走向</h3>
          {trendData.length === 0
            ? <p className="mt-0 mb-0 text-xs text-ink-2">{TREND_NONE}</p>
            : (
              <>
                <table className="dt">
                  <thead>
                    <tr>
                      <th>窗口</th><th className="num">一致率</th>
                      <th className="num">可比对</th><th className="num">低估</th>
                      <th className="num">高估</th><th>说明</th>
                    </tr>
                  </thead>
                  <tbody>
                    {trendData.map((row, i) => (
                      <tr
                        key={`${row.atText}/${i}`}
                        style={{ color: row.blocked ? 'var(--verdict-deny)' : undefined }}
                      >
                        <td>{row.atText}</td>
                        {/* 算不出的那几轮显示 '—'：一个 0% 在这一列里读起来
                            是"那天全错了"，而事实是那天没得比。 */}
                        <td className="num">{row.rateText}</td>
                        <td className="num">{row.comparable}</td>
                        <td className="num">{row.under}</td>
                        <td className="num">{row.over}</td>
                        <td className="text-ink-2">{row.missingReason}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
                <p className="mt-1 mb-0 text-xs text-ink-2">{TREND_HELP}</p>
              </>
            )}
        </div>
      </Section>

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
                      <td className="num font-semibold">{count}</td>
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

