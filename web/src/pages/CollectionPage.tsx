import { api } from '../api/client'
import type { CollectionSummary } from '../api/types'
import { useResource } from '../api/useResource'
import { Card, EmptyState, Notice, PageHeader, Section, StatTile, TableCard } from '../components/ui'
import { COLLECTION_FEEDS_NOTHING, collectionSummaryView } from './collectionView'

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
        title="资产采集"
        description="平台从真实集群只读采回来的资产清单，以及这次采集有什么没能看到。"
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
        <p className="text-ink-muted">加载中…</p>
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

      <div style={{
        display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(180px, 1fr))',
        gap: 'var(--space-3)',
      }}>
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
                      <span className="text-ink-muted">—</span>
                    ) : (
                      <span>
                        <span style={{ color: 'var(--verdict-unknown)', fontWeight: 600 }}>
                          {row.failureLabel}
                        </span>
                        <span className="mono" style={{
                          marginLeft: 8, fontSize: 'var(--text-xs)', color: 'var(--text-muted)',
                        }}>
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
                    <td className="num" style={{ fontWeight: 600 }}>{w.count}</td>
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
