import { api } from '../api/client'
import type { CollectionSummary } from '../api/types'
import { useResource } from '../api/useResource'
import { Card, EmptyState, Notice, PageHeader, Section, Skeleton, StatTile, TableCard } from '../components/ui'
import { COLLECTION_FEEDS_NOTHING, collectionSummaryView } from './collectionView'
import { flowIngestView, missingEvidence } from './flowIngestView'
import { Disclosure } from '../components/radix'

/**
 * 资产采集页。
 *
 * 平台上唯一一块显示**真实集群数据**的屏。其余每一屏都跑在合成数据集上，
 * 而这件事必须在这一屏上说出来 —— 见 COLLECTION_FEEDS_NOTHING 的注释。
 *
 * 页面本身不做任何判定：它渲染服务端给的摘要，不合并、不补齐、不为
 * 没采到的资源填一个数字。
 */
export default function CollectionPage({ cluster }: { cluster: string }) {
  const { data: state, error, loading } = useResource(cluster, () => api.collection(cluster))

  return (
    <div>
      <PageHeader
        title="数据采集"
        description="平台看见了这个集群的什么 —— 资产与流量两条链路。两者独立：只有资产时候选策略照样出得来，但「谁在访问谁」与「加了会拦断什么」要等流量。"
      />

      {/*
        提示条排在标题之下、任何数字之上，且**在加载与错误分支之外** ——
        它是读这一屏的前提，不是随数据一起出现的脚注（spec §5.2）。
        放进成功分支里，一次读取失败会让这一屏在没有说明的情况下露出来；
        放在页尾则等于让操作者先把这些数字当成平台的事实读一遍，再告诉他不是。
      */}
      <Notice>{COLLECTION_FEEDS_NOTHING}</Notice>

      {error ? (
        <p className="text-deny">{error}</p>
      ) : loading || !state ? (
        <Skeleton />
      ) : state.kind === 'UNKNOWN_CLUSTER' ? (
        <EmptyState
          message="没有这个集群。"
          detail={
            '这里说的不是「还没采过」—— 平台的注册表里根本没有这个集群 ID。'
            + '通常是地址栏里的 ID 打错了，或者这个集群已经下线。'
          }
        />
      ) : state.kind === 'NO_RUN' ? (
        <EmptyState
          message="这个集群还没有过任何一次资产采集。"
          detail={
            '这不是「采集过、什么都没采到」—— 那种情况会显示一次采集运行与它的结果。'
            + '这里是根本没有运行过，通常意味着采集器还没有对这个集群跑过一次。'
          }
        />
      ) : (
        <CollectionRun summary={state.summary} />
      )}

      <FlowIngestSection cluster={cluster} />
    </div>
  )
}

function CollectionRun({ summary }: { summary: CollectionSummary }) {
  const view = collectionSummaryView(summary)

  return (
    <div>
      {/*
        「这一轮根本没开始」排在任何数字之上：它一出现，下面的表格就是空的，
        而一张空表在界面上与「采到了零个资源」无法区分。必须先说清楚
        平台根本没有看过这个集群，再让操作者去看那张表。
      */}
      {view.errorNote ? <Notice>{view.errorNote}</Notice> : null}

      <div className="grid grid-cols-[repeat(auto-fit,minmax(180px,1fr))] gap-3">
        <StatTile label="本次采集结果" value={view.statusLabel} tone={view.statusTone}
          note={view.coverageNote} />
        <StatTile label="采集完成于" value={view.collectedAt}
          note={view.duration ? `耗时 ${view.duration}` : undefined} />
        <StatTile label="采集告警" value={`${summary.warningTotal} 条`}
          tone={summary.warningTotal > 0 ? 'unknown' : undefined}
          note="采到的事实与注册表登记不符" />
      </div>

      <div className="mt-5">
        <Section
          title="各类资源"
          description={
            '「采到 0 条」与「没采到」是两件事，这张表把它们分列显示：'
            + '没采到的一类不会有数字，只有原因与处置。一次权限不足被读成'
            + '「这个集群没有任何策略」，平台会据此推荐一份 default-deny。'
          }
        >
          <TableCard>
            <thead>
              <tr>
                <th>资源类型</th>
                <th className="num">条数</th>
                <th>没采到的原因</th>
              </tr>
            </thead>
            <tbody>
              {view.rows.map((row) => (
                <tr key={row.resource}>
                  <td>{row.label}</td>
                  {/*
                    没采到时这一格是破折号，不是 0：countText 在失败行上恒为
                    空串（collectionView.ts）。在这里给它补一个数字兜底，
                    就是把「没被授权看」显示成「这个集群没有」。
                  */}
                  <td className="num" style={{ fontWeight: row.observed ? 600 : 400 }}>
                    {row.observed ? row.countText : '—'}
                  </td>
                  <td>
                    {row.observed ? (
                      <span className="text-ink-muted mono ml-2 text-xs">—</span>
                    ) : (
                      <span>
                        <span style={{ color: 'var(--verdict-unknown)', fontWeight: 600 }}>
                          {row.failureLabel}
                        </span>
                        <span>
                          {row.failureReason}
                        </span>
                        {row.failureAction && (
                          <span style={{
                            display: 'block', fontSize: 'var(--text-xs)', color: 'var(--text-muted)',
                          }}>
                            {row.failureAction}
                          </span>
                        )}
                      </span>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </TableCard>
          {/*
            ReplicaSet 的缺席在表上看不出来，因此在表下说明。它只用于把 Pod
            解到顶层控制器，不是被观测的资产（spec §4.2）—— 补一行「0」或
            一行「未采集」都是凭空造出来的信息。
          */}
          <p style={{ marginTop: 'var(--space-2)', fontSize: 12, color: 'var(--text-muted)' }}>
            ReplicaSet 不在这张表上：它只用于把 Pod 的 ownerRef 链解到顶层控制器，
            不是被观测的资产。它的缺席不是一次采集失败。
          </p>
        </Section>
      </div>

      <div className="mt-5">
        <Section
          title="采集告警"
          description={
            '采到的事实与注册表登记不符的地方。与上面的采集失败分列：'
            + '这些告警说明采集本身是成功的，但登记的网段或工作负载关系对不上。'
          }
        >
          {view.warningRows.length === 0 ? (
            <EmptyState
              message="这次采集没有产生任何告警。"
              detail="采到的 Pod IP 都落在登记的网段内，且 ownerRef 链都解到了顶层控制器。"
            />
          ) : (
            <TableCard>
              <thead>
                <tr>
                  <th>告警</th>
                  <th>枚举值</th>
                  <th className="num">条数</th>
                </tr>
              </thead>
              <tbody>
                {view.warningRows.map((w) => (
                  <tr key={w.kind}>
                    <td>{w.label}</td>
                    {/* 原始枚举值与标签并列，理由同 QualityPage：未收录的
                        取值必须照原样显示，少显示一种成因等于把一类系统性
                        问题藏起来。 */}
                    <td className="mono">{w.kind}</td>
                    <td className="num font-semibold">{w.count}</td>
                  </tr>
                ))}
              </tbody>
            </TableCard>
          )}
        </Section>
      </div>

      <Card style={{ marginTop: 'var(--space-5)', padding: 'var(--space-3)' }}>
        <p style={{ margin: 0, fontSize: 12, color: 'var(--text-muted)' }}>
          采集运行 <span className="mono">{summary.runId}</span>，集群{' '}
          <span className="mono">{summary.clusterId}</span>。
          排障时请把这个运行号连同 request_id 一起提供 —— 失败的详细原因只写在服务端日志里，
          不通过接口回传。
        </p>
      </Card>
    </div>
  )
}

/**
 * 流量摄入那一节。
 *
 * 与资产同屏，因为两者答的是同一个问题的两半：**平台看见了这个集群的什么**。
 * 分成两个导航项会让人以为流量是另一件事，而一个集群"能不能被回答"取决于
 * 两者都有（design doc 2026-08-19-flow-ingest-visibility §2）。
 */
function FlowIngestSection({ cluster }: { cluster: string }) {
  const { data, error, loading } = useResource(
    cluster ? `flow-ingest:${cluster}` : '',
    () => api.flowIngest(cluster),
  )

  // 「从未摄入过」是服务端的一个业务码，不是一次读取失败 —— 它到这里是
  // 一个 ApiError，而 view(null) 正是它该显示的那句话。
  const neverIngested = error !== '' && /从来没有过|从未|20009/.test(error ?? '')
  const summary = data ?? null
  const view = flowIngestView(loading && !neverIngested ? undefined : summary)

  return (
    <Section
      title="流量摄入"
      description="连接观测从哪来、上次什么时候试的、看到了多少。没有它，候选策略只有基础设施那一半。"
      meta={summary ? summary.source : undefined}
    >
      {loading && !neverIngested ? <Skeleton rows={2} /> : (
        <>
          <Card className="mb-3 px-4 py-3">
            <p className="m-0 text-sm">{view.headline}</p>
            {view.action !== '' && (
              <p className="mt-2 mb-0 text-xs leading-relaxed text-ink-muted">{view.action}</p>
            )}
          </Card>

          {summary && (
            <>
              <TableCard>
                <thead>
                  <tr><th>项</th><th>值</th></tr>
                </thead>
                <tbody>
                  <tr><td>来源</td><td className="mono">{summary.source}</td></tr>
                  <tr><td>状态</td><td className="mono">{summary.status}</td></tr>
                  <tr><td>连接数</td><td className="num">{summary.connections}</td></tr>
                  <tr><td>完整度</td><td className="mono">{summary.completeness}</td></tr>
                </tbody>
              </TableCard>

              {/* **只报一个 UNKNOWN 不够。** 操作者会以为那是平台的毛病，
                  而它其实是来源的性质 —— Hubble 报不出采样率与丢弃数，
                  conntrack 是轮询快照、说不出自己覆盖了多久。 */}
              {missingEvidence(summary).length > 0 && (
                <div className="mt-3">
                  <Disclosure
                    defaultOpen
                    summary={<span>完整度是 <strong>{summary.completeness}</strong>，因为这几项证据来源没给</span>}
                  >
                    <ul className="m-0 pl-[1.2em] text-sm leading-relaxed">
                      {missingEvidence(summary).map((m) => <li key={m}>{m}</li>)}
                    </ul>
                  </Disclosure>
                </div>
              )}
            </>
          )}
        </>
      )}
    </Section>
  )
}
