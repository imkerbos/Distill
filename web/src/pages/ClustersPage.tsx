import { Fragment, useState, type CSSProperties, type FormEvent, type ReactNode } from 'react'
import { clusterDriftView, driftView } from './driftView.ts'
import { api, ApiError } from '../api/client'
import {
  IMPORT_ROLE_LABEL, IMPORT_SOURCE_LABEL, ONBOARD_STATE_LABEL,
  type APIServer, type ClusterDriftResult, type DriftResult, type GitBinding, type GitRepo, type ImportRole, type ImportSource,
  type CNI, type PolicyImportItem, type RegisteredCluster,
} from '../api/types'
import { Checkbox, Disclosure } from '../components/radix'
import { useResource } from '../api/useResource'
import {
  blankFormValues, blankGitValues, buildClusterWrite, describePathVerifyOutcome,
  describeEnforcedPlanes, describePathVerifyStatus, emptyApiServerRow,
  ENFORCED_PLANE_CHOICES, formValuesOf,
  gitFormValuesOf, resolveGitBinding,
  type ApiServerRow, type ClusterFormValues, type GitFormValues,
} from './clusterForm'
import { formatUtcTime, type VerifyOutcomeView } from './verifyView'
import {
  AGENT_TOKEN_ONCE_WARNING, activeAgentCount, agentStateLabel, isRevocable, lastSeenLabel,
} from './agentTokenView'
import type { IssuedAgentToken } from '../api/types'
import { VerifyBadge, VerifyOutcomeNote } from '../components/Verdict'
import { Button, Card, Chip, EmptyState, ErrorNotice, Field, PageHeader, Section, Select, Skeleton, TableCard } from '../components/ui'

/**
 * 集群管理页：注册、下线、Git 绑定、策略导入。
 *
 * 三节共用同一个 refreshKey——注册/下线/导入/删除任一操作成功后自增，
 * 驱动集群列表与导入清单重新拉取。不各自维护一套本地状态叠加服务端
 * 响应：服务端是唯一真相源（接入状态尤其如此，由服务端推进，本页
 * 任何表单字段都不能影响它）。
 *
 * 仓库清单与集群列表一起拉，且同样受 refreshKey 驱动：绑定只存一个
 * repoId，仓库地址与分支要从这份清单里查。清单单独缓存一份会让这一页
 * 显示的地址与仓库页的不一致，而平台真正会去连的是仓库页那一份。
 */
export default function ClustersPage() {
  const [refreshKey, setRefreshKey] = useState(0)
  const bump = () => setRefreshKey((k) => k + 1)

  const { data: clusters, error, loading } = useResource(
    `clusters:${refreshKey}`,
    () => api.clusters(),
  )
  const { data: repos, error: reposError } = useResource(
    `git-repos:${refreshKey}`,
    () => api.gitRepos(),
  )

  return (
    <div>
      <PageHeader
        title="集群管理"
        description="登记新集群、把集群绑定到一个已登记的策略仓库、导入已有 NetworkPolicy。接入状态（已登记/学习中/可产出候选策略）完全由服务端根据实际采集到的数据推进，本页任何表单都无法直接指定它。仓库的地址、分支与凭据在本页只读 —— 它们属于仓库，改它们请去「策略仓库」页。"
      />

      <ClusterListSection
        clusters={clusters} repos={repos} reposError={reposError}
        error={error} loading={loading} onChanged={bump}
      />
      <RegisterSection onCreated={bump} />
      <ImportSection clusters={clusters ?? []} refreshKey={refreshKey} onChanged={bump} />
    </div>
  )
}

/* ---------------------------------------------------------------------- */
/* 1. 已注册集群                                                           */
/* ---------------------------------------------------------------------- */

function ClusterListSection({ clusters, repos, reposError, error, loading, onChanged }: {
  clusters: RegisteredCluster[] | null
  repos: GitRepo[] | null
  reposError: string
  error: string
  loading: boolean
  onChanged: () => void
}) {
  const [busyId, setBusyId] = useState<string | null>(null)
  const [editingId, setEditingId] = useState<string | null>(null)
  const [agentsId, setAgentsId] = useState<string | null>(null)

  async function offboard(id: string) {
    // 二次确认必须说清后果，与本页其它两处、以及仓库页那一处同一条纪律
    // （「仍被集群绑定的仓库不会被删除…」「该集群将不再有策略仓库…」）。
    // 这一处原先只有一句「确认下线集群 X？」——它问的是"你确定吗"，
    // 而操作者需要知道的是"确定之后会怎样"。
    //
    // 三句话都要，且顺序是刻意的：先说不可逆（那是唯一一件事后补不回来的），
    // 再说凭据（那是会立刻停止工作的东西），最后说数据（那是会被误以为
    // 一并消失、其实还在的东西）。
    if (!window.confirm(
      `确认下线集群 ${id}？\n\n`
      + '这个动作没有恢复入口：下线之后它会从每一屏消失，平台没有把它重新上线的路径。\n'
      + `已经签发给 ${id} 的 agent 凭据会立刻失效并标记为已吊销 —— 装在那个集群里的 `
      + 'agent 会开始被拒绝，要重新接入需要重新登记、重新签发、重新滚一次 DaemonSet。\n'
      + '已经采到的历史数据保留在库里，但不再更新，也不会再出现在任何一屏上。',
    )) return
    setBusyId(id)
    try {
      await api.deleteCluster(id)
      onChanged()
    } catch (err) {
      window.alert(err instanceof ApiError ? err.msg : '下线失败，请稍后重试')
    } finally {
      setBusyId(null)
    }
  }

  return (
    <Section
      title="已注册集群"
      description="Git 绑定为空时显式写「未绑定」——空单元格会被读成「加载中」或「未知」，两者都不是这里想表达的事实。"
      meta={clusters ? `${clusters.length} 个` : undefined}
    >
      {/*
        仓库清单没拉到时必须说出来：下面那一格要靠它把 repoId 翻成地址，
        缺了它界面只能显示一个光秃秃的 ID，而读者无从判断这是「仓库没
        登记」还是「这次没查到」。
      */}
      {reposError && <ErrorNotice>仓库清单加载失败：{reposError}</ErrorNotice>}
      {error ? (
        <p className="text-deny">{error}</p>
      ) : loading || !clusters ? (
        <Skeleton />
      ) : clusters.length === 0 ? (
        <EmptyState message="尚未注册任何集群。" detail="使用下方表单登记第一个集群。" />
      ) : (
        <>
        {/* 其余说明折起来：十行散文压在表格上方，读者要先读完一屏字
            才看得到数据。摘要说结论，展开的是理由。 */}
        <div className="mb-3">
          <Disclosure summary={<span className="text-xs">这张表里的几格为什么这么写</span>}>
            <span className="text-sm leading-relaxed">Git 绑定为空时显式写「未绑定」——空单元格会被读成「加载中」或「未知」，两者都不是这里想表达的事实；同一条理由，没校验过的路径写「路径未校验」而不是留白。这一格展示的是「路径级」结论：policyPath 在不在。仓库那一层能不能连上是仓库页的事，两层各有各的结论，不合成一个。校验只做只读查询，它从不向仓库提交任何内容，因此证明不了平台能往那个路径提交——那要等真正提交一次才知道。「编辑」在行内展开成两份各自提交的表单：登记信息一份、Git 绑定一份。绑定是一个有自己生命周期的资源，改集群不会碰它，改绑定也不会重写集群，更不会改动仓库。</span>
          </Disclosure>
        </div>
        <TableCard>
          <thead>
            <tr>
              <th>ID</th>
              <th>显示名</th>
              <th>Pod 网段</th>
              <th>Node 网段</th>
              <th>apiserver</th>
              <th>接入状态</th>
              <th>CNI</th>
              <th>CCNP</th>
              <th>已声明生效的平面</th>
              <th>Git 绑定</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            {clusters.map((c) => (
              // Fragment 而非单个 <tr>：编辑面板作为紧随其后的整宽行展开，
              // 与被编辑的那一行同处一张表，操作者不必在弹层与表格之间
              // 比对自己正在改哪个集群。
              <Fragment key={c.id}>
                <tr>
                  <td className="mono">{c.id}</td>
                  <td>{c.displayName}</td>
                  <td className="mono">{c.podCidr}</td>
                  <td className="mono">{c.nodeCidr}</td>
                  <td><ApiServerList servers={c.apiServers} /></td>
                  <td><Chip strong={c.state === 'READY'}>{ONBOARD_STATE_LABEL[c.state]}</Chip></td>
                  {/*
                    这一格存在的理由与「未绑定」「未校验」同一条：一个看不见
                    的降级理由，等于操作者无法解释他看到的判定，也无法察觉
                    一次编辑把它清掉了。
                  */}
                  {/* CNI 是一个事实，不是判断：平台从不拿它决定要不要降级
                      （探测到第二平面就降级，不管 CNI 是什么）。它在这里
                      是为了让读的人判断得了那些第二平面对象是不是活的 ——
                      实测 Cilium 根本不实现 ANP，那种集群上的 ANP 是死的。 */}
                  <td>{cniLabel(c.cni)}</td>
                  <td><CCNPMark present={c.ccnpPresent} /></td>
                  {/*
                    与左边那一格 CNI 并排，因为它们会不一致，而不一致正是
                    要被看见的东西：声明了 ANP 却跑着 Cilium —— 实测 Cilium
                    1.19 完全不实现 ANP —— 那个声明是错的，平台会按一个
                    根本不生效的平面求值，把通着的连接判成不通。
                  */}
                  <td>
                    <span title={describeEnforcedPlanes(c).detail}
                      className={c.enforcedPlanes?.length ? undefined : 'text-ink-muted'}>
                      {describeEnforcedPlanes(c).text}
                    </span>
                  </td>
                  <td>
                    {c.git
                      ? (
                        <GitBindingCell
                          clusterId={c.id} git={c.git}
                          repo={repoOf(repos, c.git.repoId)} onChanged={onChanged}
                        />
                      )
                      : <span className="text-ink-muted">未绑定</span>}
                  </td>
                  <td>
                    <div className="flex gap-2">
                      <Button
                        onClick={() => setEditingId(editingId === c.id ? null : c.id)}
                        variant="secondary"
                      >
                        {editingId === c.id ? '收起' : '编辑'}
                      </Button>
                      {/* 推送式采集的机器身份：签发/查看/吊销这个集群的
                          agent token。与「编辑」并列而不是塞进编辑面板 ——
                          它是一条独立的接入操作，有自己的审计（design doc
                          2026-08-18 §3）。 */}
                      <Button
                        onClick={() => setAgentsId(agentsId === c.id ? null : c.id)}
                        variant="secondary"
                      >
                        {agentsId === c.id ? '收起' : 'agent'}
                      </Button>
                      {/* 下线用次要样式：它是这一行里唯一不可逆的动作，
                          而把它画成视觉上最重的那一个，等于邀请人去点。
                          拦住误点的是二次确认，不是颜色 —— 但也不该反过来
                          用颜色去吸引点击。 */}
                      <Button
                        onClick={() => offboard(c.id)}
                        disabled={busyId === c.id}
                        variant="secondary"
                      >
                        {busyId === c.id ? '下线中…' : '下线'}
                      </Button>
                    </div>
                  </td>
                </tr>
                {editingId === c.id && (
                  <tr>
                    <td colSpan={9} className="bg-sunken">
                      {/*
                        key 绑定集群 ID：换一个集群展开时必须重新播种，
                        复用同一份表单状态会把上一个集群的网段带进来。
                      */}
                      <EditClusterForm
                        key={c.id}
                        cluster={c}
                        onCancel={() => setEditingId(null)}
                        onSaved={() => { setEditingId(null); onChanged() }}
                      />
                      {/*
                        绑定表单与集群表单并列，各自是一个 <form>、各自
                        提交、各自报错。挤进同一个 form 就等于一次保存
                        写两个资源——而服务端的集群写路径已经不收 git，
                        混着的那一半会静默落空。

                        它刻意不在保存成功后收起：绑定保存会顺带跑一次
                        只读校验，那句回执正是操作者点保存最想知道的东西，
                        面板一收起就没地方显示它了。
                      */}
                      <GitBindingForm cluster={c} repos={repos} onChanged={onChanged} />
                    </td>
                  </tr>
                )}
                {agentsId === c.id && (
                  <tr>
                    <td colSpan={9} className="bg-sunken">
                      {/* key 绑集群 ID：换集群展开必须重挂载，否则上一个
                          集群刚签出的一次性 token 会残留在这个面板里。 */}
                      <AgentPanel key={c.id} clusterId={c.id} />
                    </td>
                  </tr>
                )}
              </Fragment>
            ))}
          </tbody>
        </TableCard>
        </>
      )}
    </Section>
  )
}

/**
 * 从仓库清单里按 ID 取一个仓库；清单还没到或这个 ID 不在清单里时给 undefined。
 *
 * 单独成函数是为了让「没查到」有一个明确的返回值，调用点必须处理它：
 * 绑定只存一个 repoId，而仓库清单本身可能这次没拉到、那个仓库也可能已被
 * 下线。这两种情形都不能显示成空白。
 */
/**
 * 一个集群的 agent 面板：查看、签发、吊销推送式采集的 token。
 *
 * **明文 token 只在签发那一次的响应里出现一次**（design doc 2026-08-18 §3、
 * 规范 §33）。它只活在这个组件的 state 里，随面板关闭一起消失 —— 不落
 * localStorage、不进 URL、列表里也永远不含它。丢了只能重签、吊销旧的。
 */
function AgentPanel({ clusterId }: { clusterId: string }) {
  const [refreshKey, setRefreshKey] = useState(0)
  const bump = () => setRefreshKey((k) => k + 1)
  const { data: agents, error, loading } = useResource(
    `agents:${clusterId}:${refreshKey}`,
    () => api.clusterAgents(clusterId),
  )

  const [issued, setIssued] = useState<IssuedAgentToken | null>(null)
  const [busy, setBusy] = useState(false)
  const [actionError, setActionError] = useState('')

  async function issue() {
    setActionError('')
    setBusy(true)
    try {
      const token = await api.issueClusterAgent(clusterId)
      // 明文只此一次。放进 state 当场展示，不写任何持久化的地方。
      setIssued(token)
      bump()
    } catch (e) {
      setActionError(e instanceof ApiError ? e.msg : '签发失败，请稍后重试')
    } finally {
      setBusy(false)
    }
  }

  async function revoke(agentId: string) {
    setActionError('')
    setBusy(true)
    try {
      await api.revokeClusterAgent(clusterId, agentId)
      // 吊销的可能正是刚签出、还显示在上面的那把：一并清掉，别让一把已
      // 作废的 token 继续摆在屏幕上像是还能用。
      if (issued?.agentId === agentId) setIssued(null)
      bump()
    } catch (e) {
      setActionError(e instanceof ApiError ? e.msg : '吊销失败，请稍后重试')
    } finally {
      setBusy(false)
    }
  }

  const active = agents ? activeAgentCount(agents) : 0

  return (
    <div className="p-3">
      <div className="mb-3 flex items-center justify-between gap-3">
        <span className="text-sm text-ink-2">
          推送式采集凭据{agents ? ` · ${active} 把可用 / 共 ${agents.length} 把` : ''}
        </span>
        <Button onClick={issue} disabled={busy} variant="primary">
          {busy ? '处理中…' : '签发新 token'}
        </Button>
      </div>

      {issued && <IssuedTokenCard token={issued} onDismiss={() => setIssued(null)} />}
      {actionError && <ErrorNotice>{actionError}</ErrorNotice>}

      {error ? (
        <p className="text-deny">{error}</p>
      ) : loading || !agents ? (
        <Skeleton rows={2} />
      ) : agents.length === 0 ? (
        <EmptyState
          message="这个集群还没有签发过任何 agent token。"
          detail="签发一把，装进被管集群里的 DaemonSet（见 docs/agent-daemonset-example.yaml），它就会把资产与流量推回来。平台自己从不持有那个集群的凭据。"
        />
      ) : (
        <TableCard>
          <thead>
            <tr>
              <th>agent ID</th><th>状态</th><th>签发人</th><th>签发于</th><th>上次连接</th><th>操作</th>
            </tr>
          </thead>
          <tbody>
            {agents.map((a) => (
              <tr key={a.agentId}>
                <td className="mono">{a.agentId}</td>
                <td><Chip strong={a.state === 'ACTIVE'}>{agentStateLabel(a.state)}</Chip></td>
                <td className="mono">{a.createdBy}</td>
                <td className="text-xs">{formatUtcTime(a.createdAt)}</td>
                <td className="text-xs">{lastSeenLabel(a)}</td>
                <td>
                  {isRevocable(a) ? (
                    <Button onClick={() => revoke(a.agentId)} disabled={busy} variant="secondary">
                      吊销
                    </Button>
                  ) : (
                    <span className="text-ink-muted text-xs">
                      {a.revokedAt ? `已于 ${formatUtcTime(a.revokedAt)} 吊销` : '—'}
                    </span>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </TableCard>
      )}
    </div>
  )
}

/**
 * 刚签发的一把 token —— **只显示这一次**。
 *
 * 用 <code> 让它可整段选中复制；不加「复制」以外的花样。旁边那句一次性告示
 * 是它存在的前提：关掉面板它就没了。
 */
function IssuedTokenCard({ token, onDismiss }: { token: IssuedAgentToken; onDismiss: () => void }) {
  return (
    <Card className="mb-3 border-line-strong p-3">
      <div className="mb-2 flex items-center justify-between gap-3">
        <span className="text-sm font-medium text-ink">新 token（agent ID {token.agentId}）</span>
        <Button onClick={onDismiss} variant="secondary">我已保存，隐藏</Button>
      </div>
      <code className="block w-full select-all break-all rounded-chip border border-line bg-surface px-3 py-2 font-mono text-sm text-ink">
        {token.token}
      </code>
      <p className="mt-2 mb-0 text-xs leading-relaxed text-ink-muted">{AGENT_TOKEN_ONCE_WARNING}</p>
    </Card>
  )
}

function repoOf(repos: GitRepo[] | null, repoId: string): GitRepo | undefined {
  return repos?.find((r) => r.repoId === repoId)
}

/**
 * Git 绑定这一格：绑到哪个仓库、路径在哪、**路径级**结论、校验时刻、重新校验。
 *
 * 挤在同一格而不是拆成几列，是因为它们说的是同一件事的几个侧面——一个
 * 「路径只读校验通过」脱离了它所在的仓库和它发生的时刻就没有意义。
 *
 * 仓库地址与分支在这里**只读**：它们属于仓库，改它们去仓库页
 * （design doc §3.2、§5）。两处都能改就是两个真相来源，而平台真正会去连
 * 的是哪一个，只能靠读代码才知道。
 *
 * 展示的结论只有路径级这一层。仓库级结论不摆在这一格 —— 一个「认证被拒绝」
 * 挨着 policyPath，读的人会以为那是关于路径的判断，然后去改一个根本没问题
 * 的路径（design doc §3.3）。
 *
 * 结论与时刻一律显示，不做「没问题就不说话」的省略：一格空白在这张表里
 * 会被读成「没什么要报告的」，而「从未校验过」与「校验通过」是相反的两
 * 件事实（同本节 description 里「未绑定」的理由）。
 */
function GitBindingCell({ clusterId, git, repo, onChanged }: {
  clusterId: string
  git: GitBinding
  repo: GitRepo | undefined
  onChanged: () => void
}) {
  const [busy, setBusy] = useState(false)
  const [outcome, setOutcome] = useState<VerifyOutcomeView | null>(null)
  const [error, setError] = useState('')
  const view = describePathVerifyStatus(git)

  async function reverify() {
    setBusy(true)
    setOutcome(null)
    setError('')
    try {
      const status = await api.verifyGitBinding(clusterId)
      // 上面那个徽章始终由服务端读模型驱动，响应里的结论不就地贴进去：
      // 服务端是唯一真相源，重新拉一次列表（同本页 refreshKey 的纪律）。
      //
      // 但「这一次发生了什么」必须单独说出来，否则未配置校验器时点一下
      // 「重新校验」界面毫无反应，操作者只能猜是按钮坏了还是结论没变。
      // describeOutcome 的注释里写了这条回执与徽章为什么会不一致。
      setOutcome(describePathVerifyOutcome(status))
      onChanged()
    } catch (err) {
      // 未绑定的集群这个端点回 404，但这一格只在已绑定时渲染，所以真正
      // 会撞上的是它指向的仓库不合今天的规则（比如库里存着的 https://
      // 地址，服务端会指名 SSH 形态作为理由）。原样展示后端的 msg，不
      // 收窄成一句「失败」——收窄掉的正是操作者据以行动的那句话，而这条
      // 路上该做的动作在仓库页，不在这里。
      setError(err instanceof ApiError ? err.msg : '重新校验失败，请稍后重试')
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="flex max-w-[340px] flex-col items-start gap-1">
      <span className="mono text-sm">{git.repoId}</span>
      <RepoReference repo={repo} repoId={git.repoId} />
      <span className="mono text-xs">路径 {git.policyPath}</span>
      <VerifyBadge view={view} />
      <span className="text-xs text-ink-muted">
        {view.checkedAt}
      </span>
      {/* 说明折起来。它必须在（「从未校验过」与「校验通过」是相反的两件
          事实），但摊在单元格里会把这一行撑到相邻行的六倍高，整张表就读不
          成一张表了。摘要仍然说结论 —— 折起来的说明与不存在的说明对读者
          是两回事。 */}
      <Disclosure summary={<span className="text-xs">这个结论是什么意思</span>}>
        <span className="text-xs text-ink-2">{view.detail}</span>
      </Disclosure>
      <Button
        type="button"
        onClick={reverify}
        disabled={busy}
        variant="secondary" className="mt-1"
      >
        {busy ? '校验中…' : '重新校验路径（只读）'}
      </Button>
      {outcome && <VerifyOutcomeNote outcome={outcome} />}
      <DriftCheck clusterId={clusterId} />
      {error && <ErrorNotice>{error}</ErrorNotice>}
    </div>
  )
}

/**
 * 「我们写进去的那份策略现在还在吗」。
 *
 * 与路径校验分开一格：那一格问的是「这个绑定指得对不对」，这一格问的是
 * 「上次下发的东西还在不在」——两者可以一个 OK 一个漂移，合成一句话就
 * 说不清该去修哪一个。
 *
 * 按需触发而不是进页面就查：每次检测都是一次真实的出站克隆。
 */
function DriftCheck({ clusterId }: { clusterId: string }) {
  const [result, setResult] = useState<DriftResult | null>(null)
  const [clusterResult, setClusterResult] = useState<ClusterDriftResult | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  async function check() {
    setBusy(true)
    setError('')
    try {
      const status = await api.gitBindingDrift(clusterId)
      setResult(status.driftResult)
      setClusterResult(status.clusterDriftResult)
    } catch (err) {
      // 请求本身失败与「查了、够不到仓库」是两件事：后者由服务端答
      // UNKNOWN，前者连服务端都没答上。两者都不得读成"一致"。
      setResult(null)
      setClusterResult(null)
      setError(err instanceof ApiError ? err.msg : '漂移检测失败，请稍后重试')
    } finally {
      setBusy(false)
    }
  }

  const view = result === null ? null : driftView(result)
  // 两个结论并排显示，不合成一个：仓库没被动过、但 controller 三天没同步的
  // 集群，上面那条是"一致"，下面这条才是"还没生效"（design doc 2026-08-25 §5）。
  const clusterView = clusterResult === null ? null : clusterDriftView(clusterResult)
  return (
    <>
      <Button
        type="button"
        onClick={check}
        disabled={busy}
        variant="secondary" className="mt-1"
      >
        {busy ? '检测中…' : '检测漂移（只读）'}
      </Button>
      {view && (
        <div style={{ fontSize: 'var(--text-xs)', marginTop: 2 }}>
          <div className="text-ink-muted">仓库 vs 平台最后写过的</div>
          <div style={{ color: DRIFT_TONE_COLOR[view.tone] }}>{view.label}</div>
          <div className="text-ink-2">{view.action}</div>
        </div>
      )}
      {clusterView && (
        <div style={{ fontSize: 'var(--text-xs)', marginTop: 4 }}>
          <div className="text-ink-muted">集群 vs 仓库（GitOps 落下去了吗）</div>
          <div style={{ color: DRIFT_TONE_COLOR[clusterView.tone] }}>{clusterView.label}</div>
          <div className="text-ink-2">{clusterView.action}</div>
        </div>
      )}
      {error && <ErrorNotice>{error}</ErrorNotice>}
    </>
  )
}

/** 四档轻重对应的颜色。 */
const DRIFT_TONE_COLOR: Record<string, string> = {
  ok: 'var(--verdict-allow)',
  warn: 'var(--verdict-unknown)',
  danger: 'var(--verdict-deny)',
  muted: 'var(--text-muted)',
}

/**
 * 绑定指向的那个仓库的地址与分支，只读。
 *
 * 查不到时不留空、也不假装没有这回事：绑定还指着这个 ID，而界面说不出它
 * 是什么，这本身就是要被看见的一件事——一格空白会被读成「这个绑定没有
 * 仓库」，而实际情况是平台仍然会去连某个我们此刻显示不出来的地方。
 */
function RepoReference({ repo, repoId }: { repo: GitRepo | undefined; repoId: string }) {
  if (!repo) {
    return (
      <span style={{ fontSize: 'var(--text-xs)', color: 'var(--verdict-unknown)' }}>
        仓库 {repoId} 不在当前仓库清单里（可能已下线，或清单这次没拉到）——
        地址与分支无法显示，去「策略仓库」页确认。
      </span>
    )
  }
  return (
    <span className="mono text-xs text-ink-muted">
      {repo.repoUrl}@{repo.branch}
    </span>
  )
}

/* ---------------------------------------------------------------------- */
/* 2. 集群表单：注册与编辑共用                                              */
/* ---------------------------------------------------------------------- */

/**
 * 集群表单的公共字段。
 *
 * 注册与编辑共用同一份字段与同一套校验（见 clusterForm.ts）：两份拷贝
 * 一定会漂移，而漂移的落点是策略下发路径——一个只在注册表单拦得住的
 * 半截 apiserver 清单，会在编辑表单被放进库里。
 *
 * **这里没有 Git 绑定的输入框**：绑定是一个有自己生命周期的资源，由
 * GitBindingForm 独立提交（design doc 2026-08-13 §7）。放回来的后果不是
 * 多几个输入框，而是服务端根本不收它——请求成功返回，绑定原封不动。
 *
 * mode 只影响一处：集群 ID 在编辑时只读（它是全平台的身份主键，改它
 * 不是改这个集群而是指向另一个集群）。接入状态两种模式下都没有控件：
 * 它由服务端根据实际采集到的数据推进，表单能提交它就等于允许把
 * 「还没有数据」标成「可以出推荐了」。
 */
function ClusterFields({ values, patch, mode }: {
  values: ClusterFormValues
  patch: (p: Partial<ClusterFormValues>) => void
  mode: 'create' | 'edit'
}) {
  function updateApiServerRow(i: number, rowPatch: Partial<ApiServerRow>) {
    patch({
      apiServerRows: values.apiServerRows.map((r, idx) => (idx === i ? { ...r, ...rowPatch } : r)),
    })
  }
  function addApiServerRow() {
    patch({ apiServerRows: [...values.apiServerRows, emptyApiServerRow()] })
  }
  function removeApiServerRow(i: number) {
    if (values.apiServerRows.length <= 1) return
    patch({ apiServerRows: values.apiServerRows.filter((_, idx) => idx !== i) })
  }

  return (
    <>
      <FormGrid>
        <TextField
          label={mode === 'edit' ? '集群 ID（不可修改）' : '集群 ID'}
          value={values.id}
          onChange={(v) => patch({ id: v })}
          required
          readOnly={mode === 'edit'}
          mono
        />
        <TextField label="显示名" value={values.displayName} onChange={(v) => patch({ displayName: v })} required />
        {/* 双栈集群的每个 Pod 有两个地址。只登记得下一个的话，走另一个
            协议族的连接会落进 EXTERNAL —— 平台把它当成出公网，于是生成一条
            ipBlock 规则而不是 selector 规则，放行面比实际需要的宽得多。
            placeholder 把这件事说出来，因为表单上看不出这个字段收多段。 */}
        <TextField
          label="Pod CIDR" value={values.podCidr}
          onChange={(v) => patch({ podCidr: v })} required mono
          placeholder="10.4.0.0/14  —— 双栈用逗号分隔：10.4.0.0/14, fd00:10:4::/56"
        />
        <TextField
          label="Node CIDR" value={values.nodeCidr}
          onChange={(v) => patch({ nodeCidr: v })} required mono
          placeholder="10.128.0.0/20  —— 双栈用逗号分隔"
        />
        {/*
          凭据引用只是一个**名字**：界面与主服务都不解析它，也永远不持有它
          指向的 kubeconfig —— 采集器是全平台唯一解析它的地方。这里能填的
          只有短名，正因如此这个输入框本身不构成一次凭据输入。

          非必填：没有登记凭据的集群仍然可以注册，只是采集器采不了它。
          但一旦填过，每次保存都必须原样带上 —— PUT 是整体替换，
          漏带一次就等于把它清空（见 clusterForm.ts 的同一段注释）。
        */}
        <TextField
          label="kubeconfig 引用"
          value={values.kubeconfigRef}
          onChange={(v) => patch({ kubeconfigRef: v })}
          mono
        />
      </FormGrid>

      {/*
        余下的登记项收进一段可展开区。

        它们全是可选的，而摊开之后这张表单有 1437px 高、21 个输入框，
        必填只占最上面 87px —— 一个只想先把集群登进来的人，要从 21 个框里
        自己判断"我最少要填什么"，还要滚过 16 个不填的框才够得着提交按钮
        （实测最后一个必填框到按钮 1350px）。spec §17.1 要的是「高密度信息
        先拆解再建立视觉秩序」，一条无分组的竖列正是没拆解。

        注册时默认收起，编辑时默认展开：注册的人多半还没有这些信息，
        而点开编辑的人是奔着其中某一项去的，收起只会多一次点击。

        **收起不等于不提交。** 这些字段照旧被 buildClusterWrite 原样带上 ——
        PUT 是整体替换，少带一项就是把它清空。收起的只有视觉。
      */}
      <Disclosure
        defaultOpen={mode === 'edit'}
        summary={(
          <span className="text-sm">
            其余登记项（可选）
            <span className="block text-xs text-ink-muted">
              apiserver、健康检查网段、metrics 抓取端、节点 agent、业务周期、
              系统命名空间、CNI 执行的第二平面 —— 都可以之后再补。
              已经填过的值即使收着也会照常保存。
            </span>
          </span>
        )}
      >
      <div className="p-3">

      {/*
        这一项不是技术开关，措辞也就不能写成技术开关。它声明的是「这个
        集群里还有别的东西在影响连通性」，而平台不求值 CCNP，因此凡是
        勾上的集群，回放判定一律降级为 DEGRADED——标签要说的是这个后果，
        不是「有没有装 Cilium」。

        表单里必须有它、且必须按现值预填：PUT 是整体替换，界面上不出现
        就等于每次编辑都把它清成 false，让一个本该降级的集群显示成正常
        判定。这是"看上去更有把握"的方向，也是最难被发现的方向。
      */}
      <label style={{
        display: 'flex', alignItems: 'flex-start', gap: 'var(--space-2)',
        fontSize: 'var(--text-sm)', marginBottom: 'var(--space-3)',
      }}>
        <Checkbox
          checked={values.ccnpPresent}
          onChange={(v) => patch({ ccnpPresent: v })}
          ariaLabel="集群启用了 CiliumClusterwideNetworkPolicy"
          className="mt-[3px]"
        />
        <span>
          该集群存在 CiliumClusterwideNetworkPolicy
          <span style={{ display: 'block', color: 'var(--text-muted)', fontSize: 'var(--text-xs)' }}>
            勾选后，这个集群的所有回放判定一律降级为「结论不可信（DEGRADED）」——
            平台不求值 CCNP，看不见它放行或拒绝了什么，因此给不出可信的结论。
            装了却不勾，平台会用一个它其实解释不了的模型给出笃定的结论。
          </span>
        </span>
      </label>

      <SubHeading>
        apiserver（可选，可添加多个 —— HA 控制面通常不止一个端点，
        漏填一个就是漏了一条 baseline 放行规则，后果是生产阻断而不是注册报错。
        每一行要么整行留空（会被忽略），要么 host / cidr / port 三项都填）
      </SubHeading>
      {values.apiServerRows.map((row, i) => (
        <div key={i} style={{
          display: 'flex', gap: 'var(--space-3)', alignItems: 'flex-end',
          marginBottom: 'var(--space-2)',
        }}>
          <div className="flex-1">
            <TextField label="host" value={row.host} onChange={(v) => updateApiServerRow(i, { host: v })} mono />
          </div>
          <div className="flex-1">
            <TextField label="cidr" value={row.cidr} onChange={(v) => updateApiServerRow(i, { cidr: v })} mono />
          </div>
          <div style={{ width: 110 }}>
            <TextField
              label="port" value={row.port} onChange={(v) => updateApiServerRow(i, { port: v })}
              mono placeholder="443"
            />
          </div>
          <Button
            type="button"
            variant="secondary"
            onClick={() => removeApiServerRow(i)}
            disabled={values.apiServerRows.length <= 1}
          >
            删除
          </Button>
        </div>
      ))}
      <Button type="button" onClick={addApiServerRow} variant="secondary">
        + 添加 apiserver
      </Button>

      <div className="mt-4">
        <SubHeading>健康检查网段（可选，每行一个 CIDR）</SubHeading>
        <textarea
          className="ctl w-full"
          aria-label="健康检查网段"
          value={values.healthChecks}
          onChange={(e) => patch({ healthChecks: e.target.value })}
          rows={3}
        />
      </div>

      <div className="mt-4">
        <SubHeading>metrics 抓取端（可选，每行一个）</SubHeading>
        <p className="mt-0 mb-[6px] text-xs text-ink-muted">
          格式 <code>命名空间&nbsp;&nbsp;标签键=值,标签键=值</code>，例如{' '}
          <code>monitoring&nbsp;&nbsp;app.kubernetes.io/name=prometheus</code>。
          填了它，被抓的工作负载才会拿到放行抓取端的 Baseline；不填就照旧报缺失
          —— 平台不会去猜谁是抓取端，猜错会生成一条选不中任何 Pod 的规则。
        </p>
        <textarea
          className="ctl w-full"
          aria-label="metrics 抓取端"
          value={values.metricsScrapers}
          onChange={(e) => patch({ metricsScrapers: e.target.value })}
          rows={3}
        />
      </div>

      <div className="mt-4">
        <SubHeading>节点级 agent（可选，每行一个）</SubHeading>
        <p className="mt-0 mb-[6px] text-xs text-ink-muted">
          格式 <code>命名空间&nbsp;&nbsp;app&nbsp;&nbsp;host|pod&nbsp;&nbsp;端口</code>，例如{' '}
          <code>logging&nbsp;&nbsp;filebeat&nbsp;&nbsp;host&nbsp;&nbsp;9200</code>。
          只填「会向工作负载建连接」的那些 —— 读文件的日志 agent（filebeat、promtail）
          不需要放行。端口只有你知道，平台不猜。
        </p>
        <textarea
          className="ctl w-full"
          aria-label="节点级 agent"
          value={values.nodeAgents}
          onChange={(e) => patch({ nodeAgents: e.target.value })}
          rows={3}
        />
      </div>

      <div className="mt-3">
        <SubHeading>或：声明这个集群没有需要放行的节点 agent</SubHeading>
        <p className="mt-0 mb-[6px] text-xs text-ink-muted">
          填了理由，NODE_AGENT 这一类会标为「不适用」而不再报缺失。
          它是一次会被记进审计的判断：判错的方向是监控在下发之后静默中断，
          而那要到事故发生时才显现。与上面那一栏互斥。
        </p>
        <input
          className="ctl w-full"
          aria-label="没有节点 agent 的理由"
          value={values.noNodeAgentsReason}
          onChange={(e) => patch({ noNodeAgentsReason: e.target.value })}
          placeholder="例如：本集群的 agent 只读文件，不向工作负载建连接"
        />
      </div>

      {/*
        业务周期。写回门禁拿它判断观测窗口够不够长 —— 不知道一轮有多长，
        "窗口没覆盖到的那一段流量"与"这条连接不存在"在数据里长得一模一样。

        两格必须同时填或同时留空，与服务端同一条规则。留空不是缺陷，是
        "还没有人回答过"，门禁据此拒绝出计划 —— 那正是它该做的。
      */}
      <div className="mt-3">
        <SubHeading>业务周期</SubHeading>
        <p className="mt-0 mb-[6px] text-xs text-ink-muted">
          这个集群看全一轮流量需要多久。写回门禁拿它判断观测窗口够不够长：
          一条每月月末才跑一次的连接，在一个七天的窗口里与「不存在」无法区分。
          时长与理由必须同时给出，或者都留空（表示还没有人回答过）。
        </p>
        <div className="flex gap-3 items-end">
          <div className="w-[180px]">
            <TextField
              label="秒"
              value={values.businessCycleSeconds}
              onChange={(v) => patch({ businessCycleSeconds: v })}
              mono
            />
          </div>
          <div className="flex-1">
            <TextField
              label="凭什么这么定"
              value={values.businessCycleReason}
              onChange={(v) => patch({ businessCycleReason: v })}
            />
          </div>
        </div>
      </div>

      {/*
        系统命名空间。默认不管 —— 一份下发到 kube-dns 的 default-deny 会让
        整个集群失去 DNS，这道保护的默认值必须是"不碰"。

        但它必须有一个入口：没有入口的保护是一堵没有门的墙，真正需要管控
        kube-system 的集群只能改库，而改库这件事不会留下理由。
      */}
      <div className="mt-3">
        <SubHeading>交给平台管理的系统命名空间（默认：一个都不管）</SubHeading>
        <p className="mt-0 mb-[6px] text-xs text-ink-muted">
          留空时，平台不为 kube-system / kube-public / kube-node-lease 生成任何候选策略。
          每行一个命名空间。列进来之后，平台会为其中每个 workload 生成 default-deny 候选 ——
          一份下发到 kube-dns 的 default-deny 会让整个集群失去域名解析，
          所以这一栏要求写明理由。
        </p>
        <textarea
          className="ctl w-full font-mono"
          aria-label="交给平台管理的系统命名空间"
          value={values.managedSystemNamespaces}
          onChange={(e) => patch({ managedSystemNamespaces: e.target.value })}
          rows={2}
          // 占位符不写成一个真的命名空间名。深色主题下灰字读起来像已填的值，
          // 而这一格的整个安全性建立在"空 = 不碰系统命名空间"上 —— 一个看起来
          // 已经填了 kube-system 的空输入框，方向恰好错在最危险的那一侧。
          placeholder="留空 = 一个都不管（默认）"
        />
        <input
          className="ctl w-full mt-2"
          aria-label="纳入系统命名空间的理由"
          value={values.managedSystemNamespacesReason}
          onChange={(e) => patch({ managedSystemNamespacesReason: e.target.value })}
          placeholder="例如：本集群的 kube-system 里跑着业务组件，已确认 kube-dns 有独立放行"
        />
      </div>

      {/*
        CNI 执行哪些第二平面。这是操作者的**事实声明**，不是开关 ——
        与上面那个 ccnpPresent 方向相反：那一项勾上是让平台更保守（降级），
        这一项勾上是让平台**开始按那个平面的语义求值**。

        声明错的方向因此是危险的：一个并不生效的平面被声明之后，平台会以为
        某条连接被它拦着，于是不生成放行规则 —— 下发之后那条连接才真的断。
        文案要说的是这个后果，理由那一格问的是"你怎么验证的"。
      */}
      <div className="mt-3">
        <SubHeading>CNI 真的会执行的第二策略平面（默认：一个都不解释）</SubHeading>
        <p className="mt-0 mb-[6px] text-xs text-ink-muted">
          留空时平台不按任何第二平面求值，探测到就整片降级 —— 保守且正确。
          勾上之后平台会按那个平面的语义求值，因此这是一句必须为真的断言：
          装了 CRD 不等于执行。声明一个并不生效的平面，平台会以为某条连接被它拦着、
          于是不生成放行规则，而那条连接会在下发之后才真的断。
        </p>
        {ENFORCED_PLANE_CHOICES.map((choice) => (
          <label
            key={choice.value}
            className="flex items-start gap-2 text-sm mb-2"
          >
            <Checkbox
              checked={values.enforcedPlanes.includes(choice.value)}
              onChange={(on) => patch({
                enforcedPlanes: on
                  ? [...values.enforcedPlanes, choice.value]
                  : values.enforcedPlanes.filter((p) => p !== choice.value),
              })}
              ariaLabel={`该集群的 CNI 执行 ${choice.label}`}
              className="mt-[3px]"
            />
            <span>
              {choice.label}
              <span className="block text-ink-muted text-xs">{choice.detail}</span>
            </span>
          </label>
        ))}
        <input
          className="ctl w-full mt-1"
          aria-label="声明这些策略平面生效的理由"
          value={values.enforcedPlanesReason}
          onChange={(e) => patch({ enforcedPlanesReason: e.target.value })}
          placeholder="例如：加一条 ANP Deny 后连接立刻断、删除后恢复，在本集群实测过"
        />
      </div>

      </div>
      </Disclosure>
    </>
  )
}

/* ---------------------------------------------------------------------- */
/* 2b. Git 绑定表单：独立资源，独立提交                                       */
/* ---------------------------------------------------------------------- */

/**
 * 一个集群的 Git 绑定：绑到一个已登记的仓库 / 改绑 / 解绑。
 *
 * 与集群表单彻底分开，两条路径互不相干（design doc 2026-08-13 §5、§7）：
 * 保存这里只发 PUT /clusters/{id}/git-binding，不发集群 PUT。顺手补一次
 * 集群写入不会报错、`tsc` 与 lint 也不会有意见 —— 它的后果是把集群表单里
 * **没有播种过**的一份状态写进库，比如把 ccnpPresent 清成 false，让一个
 * 本该降级的集群给出笃定的判定。
 *
 * 同样地，这里**不发仓库 PUT**：仓库地址、分支与凭据在这一屏只读展示，
 * 改它们去仓库页（design doc §3.2、§5）。在这里顺手改一次仓库，改的是一个
 * 可能还被别的集群绑着的共享资源，而操作者以为自己只动了这一个集群。
 *
 * 选仓库用下拉而不是让人手打 repoId：能手打就能打错，而一个指向不存在
 * 仓库的绑定，服务端只会回一句 404 —— 那句话读起来像是这个集群不存在。
 *
 * 解绑是一个按钮（DELETE），不是「把字段清空后保存」。上一轮那个
 * 「解除 Git 绑定」勾选框存在的唯一理由是：整体替换下「我清空了字段」与
 * 「我要解绑」提交结果相同、意图不同。现在解绑有了自己的动词，这个歧义
 * 不存在了，勾选框也就跟着消失。
 */
function GitBindingForm({ cluster, repos, onChanged }: {
  cluster: RegisteredCluster
  repos: GitRepo[] | null
  onChanged: () => void
}) {
  const current = cluster.git ?? null
  const [values, setValues] = useState<GitFormValues>(() => gitFormValuesOf(current))
  const [error, setError] = useState('')
  const [outcome, setOutcome] = useState<VerifyOutcomeView | null>(null)
  const [saved, setSaved] = useState(false)
  const [busy, setBusy] = useState<'' | 'save' | 'unbind'>('')

  const patch = (p: Partial<GitFormValues>) => setValues((v) => ({ ...v, ...p }))

  async function submit(e: FormEvent) {
    e.preventDefault()
    setError('')
    setOutcome(null)
    setSaved(false)

    const resolved = resolveGitBinding(values)
    if (!resolved.ok) {
      setError(resolved.error)
      return
    }

    setBusy('save')
    try {
      const status = await api.bindGitRepo(cluster.id, resolved.binding)
      // 「已保存」与校验回执分两句说：保存成功而校验没发生是完全正常的一种
      // 结果（校验失败也不阻止保存），只显示后者会让一次成功的保存读起来
      // 像失败了。存下来和可信是两件事，界面上也得是两句话。
      setSaved(true)
      // 保存会顺带跑一次**路径级**只读校验，这句回执说的是那一次校验；上面
      // 列表里的徽章仍然由服务端读模型驱动。两者可能不一致，理由见
      // describeOutcome 的注释。
      setOutcome(describePathVerifyOutcome(status))
      onChanged()
    } catch (err) {
      // 后端把拒绝的具体理由写进 msg：绑定前它会先校验目标仓库，而库里
      // 存着的 https:// 地址会被指名「不是 SSH 形态」。原样展示，不收窄成
      // 一句「绑定失败」——收窄掉的正是操作者据以行动的那句话，而这恰好是
      // repo-prod-asia-1 的现状：要修它得去仓库页，不是改这里的 policyPath。
      setError(err instanceof ApiError ? err.msg : '绑定失败，请稍后重试')
    } finally {
      setBusy('')
    }
  }

  async function unbind() {
    if (!window.confirm(
      `确认解除 ${cluster.id} 的 Git 绑定？该集群将不再有策略仓库，平台不会再向它下发策略。`,
    )) return
    setError('')
    setOutcome(null)
    setSaved(false)
    setBusy('unbind')
    try {
      await api.unbindGitRepo(cluster.id)
      // 表单跟着清空：解绑之后留着上一个仓库与路径，界面会同时显示
      // 「未绑定」和一份填好的表单，读起来像是还绑着。
      //
      // 这只是清空绑定表单。仓库本身不动：解绑不删仓库，那个仓库可能还被
      // 别的集群绑着，而删仓库有它自己的动词，在仓库页。
      setValues(blankGitValues())
      onChanged()
    } catch (err) {
      setError(err instanceof ApiError ? err.msg : '解绑失败，请稍后重试')
    } finally {
      setBusy('')
    }
  }

  return (
    <Card className="my-3 p-4">
      <form onSubmit={submit}>
        <SubHeading>
          {cluster.id} 的 Git 绑定 —— 这是一次独立提交，不会改动上面的登记信息，也不会改动仓库。
          选一个已登记的仓库并填 policyPath，两项都必填。
          仓库的地址、分支与凭据在这里只读：它们属于仓库，改它们请去「策略仓库」页 ——
          那是一个可能还被别的集群绑着的共享资源。
          保存会顺带做一次路径级只读校验，校验不通过不影响保存 —— 存下来和可信是两件事。
        </SubHeading>
        <FormGrid>
          <label className="block">
            <span style={fieldLabelStyle}>仓库</span>
            <Select
              value={values.repoId}
              ariaLabel={`${cluster.id} 绑定的策略仓库`}
              onChange={(v) => patch({ repoId: v })}
              options={[
                ['', repos === null ? '仓库清单加载中…' : '请选择仓库'],
                // 已绑定但仓库不在清单里时补一个占位项：否则下拉会自动落在
                // 「请选择仓库」上，看起来像这个集群从来没绑过东西，而一次
                // 「只想改路径」的保存会带着空 repoId 被服务端拒绝，理由读
                // 起来还与操作者做的事对不上。
                ...(values.repoId !== '' && !repoOf(repos, values.repoId)
                  ? [[values.repoId, `${values.repoId}（不在当前清单里）`] as [string, string]]
                  : []),
                ...(repos ?? []).map((r) => [r.repoId, r.repoId] as [string, string]),
              ]}
              style={{ width: '100%' }}
            />
          </label>
          <TextField
            label="policyPath" value={values.policyPath} mono
            placeholder={`clusters/${cluster.id}`}
            onChange={(v) => patch({ policyPath: v })}
          />
        </FormGrid>
        {/*
          选中仓库的地址与分支就在提交按钮上方只读展示：一个只显示 repoId
          的下拉，操作者无从判断自己选的是不是那个仓库，而选错的后果是策略
          被下发到另一个仓库去。
        */}
        <SelectedRepoDetail repo={repoOf(repos, values.repoId)} repoId={values.repoId} />
        <GitOutcome values={values} />

        {error && <ErrorNotice>{error}</ErrorNotice>}
        {saved && (
          <p role="status" style={{
            margin: 'var(--space-2) 0 0', fontSize: 'var(--text-xs)', color: 'var(--text-secondary)',
          }}>
            绑定已保存。保存与校验是两件事，下面这句说的是保存时那次只读校验。
          </p>
        )}
        {outcome && <VerifyOutcomeNote outcome={outcome} />}

        <div className="mt-3 flex gap-2">
          <Button type="submit" disabled={busy !== ''} variant="primary">
            {busy === 'save' ? '保存中…' : current ? '保存绑定' : '绑定仓库'}
          </Button>
          {/*
            未绑定时不渲染解绑按钮：一个点了会报 404 的按钮，读起来像是
            「这里本来有东西可以解除」。
          */}
          {current && (
            <Button type="button" onClick={unbind} disabled={busy !== ''} variant="secondary">
              {busy === 'unbind' ? '解绑中…' : '解除绑定'}
            </Button>
          )}
        </div>
      </form>
    </Card>
  )
}

/**
 * 选中仓库的地址、分支与凭据引用，只读。
 *
 * **这里刻意没有输入框**：它们属于仓库，本页只是把它们念出来
 * （design doc §3.2、§5）。给一个能改的输入框，服务端的 gitBindingPayload
 * 根本不收它 —— 请求返回成功，界面显示保存生效，而平台真正会去连的地址
 * 原封不动。两个真相来源比一个错的更难查。
 *
 * 也不展示仓库级校验结论：那是仓库页那一栏的事。把它摆在这张绑定表单里，
 * 与下面那句路径级回执挨在一起，两层结论会被读成一句话。
 */
function SelectedRepoDetail({ repo, repoId }: { repo: GitRepo | undefined; repoId: string }) {
  if (repoId === '') {
    return (
      <p className="m-0 text-xs text-ink-muted">
        还没有选择仓库。仓库地址与分支在选定之后显示 —— 它们只读，改它们去「策略仓库」页。
      </p>
    )
  }
  if (!repo) {
    return (
      <p style={{ margin: 0, fontSize: 'var(--text-xs)', color: 'var(--verdict-unknown)' }}>
        仓库 {repoId} 不在当前仓库清单里（可能已下线，或清单这次没拉到）——
        地址与分支无法显示。提交前请去「策略仓库」页确认它还在。
      </p>
    )
  }
  return (
    <p className="mono m-0 text-xs text-ink-muted">
      {repo.repoUrl}@{repo.branch}
      {' · 凭据 '}
      {repo.credentialRef === '' ? '未配置' : repo.credentialRef}
    </p>
  )
}

/**
 * 提交前把「这一次会把绑定改成什么」写出来。
 *
 * 两个控件的不同填法长得都一样，让结果只在提交后从列表里的一行文字
 * 体现，等于把「我以为我只改了路径」留到已经写进库之后才被发现。
 */
function GitOutcome({ values }: { values: GitFormValues }) {
  const resolution = resolveGitBinding(values)
  return (
    <p style={{
      margin: 'var(--space-2) 0 0', fontSize: 'var(--text-xs)',
      color: resolution.ok ? 'var(--text-secondary)' : 'var(--verdict-unknown)',
    }}>
      {resolution.ok ? resolution.summary : resolution.error}
    </p>
  )
}

function RegisterSection({ onCreated }: { onCreated: () => void }) {
  const [values, setValues] = useState<ClusterFormValues>(blankFormValues)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  const patch = (p: Partial<ClusterFormValues>) => setValues((v) => ({ ...v, ...p }))

  async function submit(e: FormEvent) {
    e.preventDefault()
    setError('')

    // 注册只写集群。绑定要等集群存在之后才能挂上去（PUT /git-binding 会先
    // 断言集群存在），所以它不在这张表单里——注册时还不知道仓库路径本就是常态。
    const built = buildClusterWrite(values)
    if (!built.ok) {
      setError(built.error)
      return
    }

    setBusy(true)
    try {
      await api.createCluster(built.body)
      setValues(blankFormValues())
      onCreated()
    } catch (err) {
      // 后端把校验失败的具体字段写进 msg（比如「podCIDR "10.20.0/14" 不是
      // 合法网段」）——原样展示，不要收窄成一句「请求参数不合法」，
      // 否则运维猜不出四类网段里到底是哪一类写错了。
      setError(err instanceof ApiError ? err.msg : '注册失败，请稍后重试')
    } finally {
      setBusy(false)
    }
  }

  return (
    <Section
      title="注册新集群"
      description="仅登记元数据；接入状态从「已登记」起步，不受本表单任何字段影响，包括你在这里可能填的任何值。Git 绑定不在这里：它是一个独立资源，集群注册完成后在上方列表里展开「编辑」单独绑定——注册时不知道仓库路径是常态。"
    >
      <Card className="p-4">
        <form onSubmit={submit}>
          <ClusterFields values={values} patch={patch} mode="create" />

          {error && <ErrorNotice>{error}</ErrorNotice>}

          <Button type="submit" disabled={busy} variant="primary" className="mt-3">
            {busy ? '提交中…' : '注册集群'}
          </Button>
        </form>
      </Card>
    </Section>
  )
}

/**
 * 编辑已注册集群。
 *
 * 表单从集群现值播种，一项都不能省：PUT 是整体替换，表单里空着的字段
 * 提交后就是库里空着的字段。一个只想改仓库地址的操作者不该因此丢掉
 * apiserver 清单——那少掉的是一条 control-plane 放行规则，事后表现为
 * 生产阻断，而不是提交时报错。
 *
 * 这条路径**不碰 Git 绑定**：绑定在下方的 GitBindingForm 里独立提交。
 * 服务端的集群写路径已经不接受 git，在这里带上它请求照样成功，绑定却
 * 原封不动 —— 界面显示保存生效，库里没有那回事。
 *
 * 已知限制：整体替换 + 无版本号意味着两个人同时编辑同一个集群时后写
 * 覆盖先写，且双方都不会收到提示。乐观锁需要在存储层引入版本列，不在
 * 本轮范围。
 */
function EditClusterForm({ cluster, onSaved, onCancel }: {
  cluster: RegisteredCluster
  onSaved: () => void
  onCancel: () => void
}) {
  const [values, setValues] = useState<ClusterFormValues>(() => formValuesOf(cluster))
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  const patch = (p: Partial<ClusterFormValues>) => setValues((v) => ({ ...v, ...p }))

  async function submit(e: FormEvent) {
    e.preventDefault()
    setError('')

    const built = buildClusterWrite(values)
    if (!built.ok) {
      setError(built.error)
      return
    }

    setBusy(true)
    try {
      await api.updateCluster(cluster.id, built.body)
      onSaved()
    } catch (err) {
      setError(err instanceof ApiError ? err.msg : '保存失败，请稍后重试')
    } finally {
      setBusy(false)
    }
  }

  return (
    <Card className="my-3 p-4">
      <form onSubmit={submit}>
        <SubHeading>
          编辑 {cluster.id}：提交会整体替换该集群的登记信息，表单里的每一项都会被写入，
          因此下面的值已按当前登记内容预填——清空一项就是把它从库里清空。
          接入状态不在此列，它由服务端推进；Git 绑定也不在此列，它在下面单独提交。
        </SubHeading>
        <ClusterFields values={values} patch={patch} mode="edit" />

        {error && <ErrorNotice>{error}</ErrorNotice>}

        <div className="mt-3 flex gap-2">
          <Button type="submit" disabled={busy} variant="primary">
            {busy ? '保存中…' : '保存修改'}
          </Button>
          <Button type="button" onClick={onCancel} variant="secondary">取消</Button>
        </div>
      </form>
    </Card>
  )
}

/* ---------------------------------------------------------------------- */
/* 3. 策略导入                                                             */
/* ---------------------------------------------------------------------- */

function ImportSection({ clusters, refreshKey, onChanged }: {
  clusters: RegisteredCluster[]
  refreshKey: number
  onChanged: () => void
}) {
  const [selected, setSelected] = useState('')
  const [role, setRole] = useState<ImportRole>('BASELINE_CURRENT')
  const [source, setSource] = useState<ImportSource>('PASTE')
  const [gitCommitSha, setGitCommitSha] = useState('')
  const [yaml, setYaml] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  const importsKey = selected ? `imports:${selected}:${refreshKey}` : ''
  const { data: imports, error: listError, loading } = useResource(
    importsKey,
    () => api.policyImports(selected),
  )

  async function submit(e: FormEvent) {
    e.preventDefault()
    setError('')
    if (!selected) {
      setError('请先选择集群。')
      return
    }
    // 空 YAML 走应用层提示，不靠 textarea 的 required 属性。
    //
    // 那个属性弹的是浏览器原生气泡，一句英文 Please fill out this field.，
    // 而这张表单其余每一条错误都是中文（上面那句「请先选择集群」就是）。
    // 同一次提交里出现两套语言两种样式，读的人会以为后一种是页面出错了。
    if (yaml.trim() === '') {
      setError('请粘贴要导入的 NetworkPolicy YAML。')
      return
    }
    // 来源与 commit 必须互相印证，与后端 registry.ValidatePolicyImport
    // 同一条规则。在这里先拦一道不是为了省一次请求，而是因为这个约束
    // 本身要在界面上说清楚：一条没有 commit 的 GIT 记录会被读成「与仓库
    // 一致的现状」，而轮 3 的漂移检测正是拿这个 commit 做基准。
    const sha = gitCommitSha.trim()
    if (source === 'GIT' && !/^[0-9a-fA-F]{40}$/.test(sha)) {
      setError('来源选择 GIT 时必须填写 40 位完整 commit SHA —— '
        + '没有 commit 就无法证明这份 YAML 与仓库里的内容一致。')
      return
    }
    setBusy(true)
    try {
      await api.createImport(selected, {
        role, source, yaml,
        // 非 GIT 来源一律不带 commit：一个不指向任何同步动作的 commit
        // 是一句凭空的溯源声明，后端会拒绝它。
        gitCommitSha: source === 'GIT' ? sha : '',
      })
      setYaml('')
      setGitCommitSha('')
      onChanged()
    } catch (err) {
      setError(err instanceof ApiError ? err.msg : '导入失败，请稍后重试')
    } finally {
      setBusy(false)
    }
  }

  async function remove(importId: string) {
    if (!selected) return
    if (!window.confirm('确认删除该导入记录？')) return
    try {
      await api.deleteImport(selected, importId)
      onChanged()
    } catch (err) {
      window.alert(err instanceof ApiError ? err.msg : '删除失败，请稍后重试')
    }
  }

  return (
    <Section
      title="策略导入"
      description="导入既有 NetworkPolicy YAML，作为现状基线或候选补充。本表单提交的记录一律标注「未经 Git 核对」——只有经漂移检测流水线比对过 commit 的记录才会显示已核对，该流水线不在本轮范围内。"
    >
      <Card style={{ padding: 'var(--space-4)', marginBottom: 'var(--space-3)' }}>
        <form onSubmit={submit}>
          <FormGrid>
            <label className="block">
              <span style={fieldLabelStyle}>集群</span>
              <Select
                value={selected}
                ariaLabel="导入目标集群"
                onChange={setSelected}
                options={[
                  ['', '请选择集群'],
                  ...clusters.map((c) => [c.id, `${c.id}（${ONBOARD_STATE_LABEL[c.state]}）`] as [string, string]),
                ]}
                style={{ width: '100%' }}
              />
            </label>
            <Field label="角色">
              <Select
                value={role}
                ariaLabel="导入角色"
                onChange={(v) => setRole(v as ImportRole)}
                options={Object.entries(IMPORT_ROLE_LABEL) as [string, string][]}
              />
            </Field>
            <Field label="来源">
              <Select
                value={source}
                ariaLabel="导入来源"
                onChange={(v) => setSource(v as ImportSource)}
                options={Object.entries(IMPORT_SOURCE_LABEL) as [string, string][]}
              />
            </Field>
            {/*
              commit 字段只在来源为 GIT 时出现，且此时必填。让它常驻会诱使
              人给一份粘贴的 YAML 填个 commit —— 那是一句没有东西支撑的
              溯源声明；而 GIT 却不给填的话，界面就把「未经核对」做成了
              GIT 来源的默认结局。
            */}
            {source === 'GIT' && (
              <TextField
                label="commit SHA（40 位，必填）"
                value={gitCommitSha}
                onChange={setGitCommitSha}
                mono
                placeholder="0123456789abcdef0123456789abcdef01234567"
              />
            )}
          </FormGrid>

          <SubHeading>NetworkPolicy YAML</SubHeading>
          {/*
            SubHeading 是一个标题，不是一个 label —— 屏幕阅读器不会把它读成
            这个输入框的名字，读到的只是"文本区域"。这一格里贴的是会成为
            现状基线的策略原文，读不出它是什么框，就分不清自己贴到了哪里。
          */}
          <textarea
            className="ctl w-full"
            aria-label="NetworkPolicy YAML"
            value={yaml}
            onChange={(e) => setYaml(e.target.value)}
            rows={10}
          />

          {error && <ErrorNotice>{error}</ErrorNotice>}

          <Button type="submit" disabled={busy} variant="primary" className="mt-3">
            {busy ? '提交中…' : '提交导入'}
          </Button>
        </form>
      </Card>

      {!selected ? (
        <EmptyState message="选择一个集群以查看它的导入清单。" detail="清单按集群独立维护，不跨集群合并展示。" />
      ) : listError ? (
        <p className="text-deny">{listError}</p>
      ) : loading || !imports ? (
        <Skeleton />
      ) : imports.length === 0 ? (
        <EmptyState message={`${selected} 尚无已导入的策略。`} detail="使用上方表单导入第一条。" />
      ) : (
        <TableCard>
          <thead>
            <tr>
              <th>namespace/name</th>
              <th>角色</th>
              <th>来源</th>
              <th>导入人</th>
              <th>导入时间</th>
              <th>Git 核对</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            {imports.map((it) => (
              <tr key={it.importId}>
                <td className="mono text-sm">{it.namespace}/{it.name}</td>
                <td><Chip>{IMPORT_ROLE_LABEL[it.role] ?? it.role}</Chip></td>
                <td><Chip>{IMPORT_SOURCE_LABEL[it.source] ?? it.source}</Chip></td>
                <td>{it.importedBy}</td>
                <td className="mono text-xs">{formatUtcTime(it.importedAt)}</td>
                <td><GitVerifiedMark item={it} /></td>
                <td>
                  <Button onClick={() => remove(it.importId)} variant="primary">删除</Button>
                </td>
              </tr>
            ))}
          </tbody>
        </TableCard>
      )}
    </Section>
  )
}

/**
 * 「未经 Git 核对」是对"当前状态可信程度"的事实陈述，不是次要信息——
 * 不得用灰字弱化，与 VerdictBadge 里 DEGRADED 用描边而非灰化是同一条
 * 纪律。已核对时展示 commit 前 7 位，使结论可核验而不是空喊一句"已核对"。
 */
function GitVerifiedMark({ item }: { item: PolicyImportItem }) {
  const verified = item.source === 'GIT' && item.gitCommitSha !== ''
  if (verified) {
    return <Chip strong>已用 Git 核对 · {item.gitCommitSha.slice(0, 7)}</Chip>
  }
  return (
    <span style={{
      display: 'inline-flex', alignItems: 'center', padding: '2px 8px',
      fontSize: 'var(--text-xs)', fontWeight: 500, borderRadius: 999,
      color: 'var(--verdict-unknown)',
      border: 'var(--degraded-stroke-width) solid var(--verdict-unknown)',
    }}>
      未经 Git 核对
    </span>
  )
}

/**
 * CCNP 这一格：装了就说判定被降级，没装就说没有，两种都写出来。
 *
 * 不做「有才显示、没有就留空」：空单元格会被读成「不知道」，而这一项
 * 恰恰是解释判定为什么降级的那个原因。用描边而非灰化，与 VerdictBadge
 * 处理 DEGRADED 同一条纪律——它是结论的一部分，不是次要元信息。
 */
function CCNPMark({ present }: { present: boolean }) {
  if (!present) {
    return <span style={{ color: 'var(--text-muted)', fontSize: 'var(--text-xs)' }}>无</span>
  }
  return (
    // whitespace-nowrap 不是排版偏好：这一格很窄，不加的话「有 · 判定降级」
    // 会被逐字折成一列单字，那种坏法在截图里一眼可见、在代码里看不出来。
    <span
      className="inline-flex items-center rounded-full px-2 py-[2px] text-xs font-medium whitespace-nowrap"
      style={{
        color: 'var(--verdict-unknown)',
        border: 'var(--degraded-stroke-width) solid var(--verdict-unknown)',
      }}
    >
      有 · 判定降级
    </span>
  )
}

/**
 * apiserver 列表：渲染全部条目而不是只取第一个。HA 控制面通常有多个
 * apiserver 端点，只展示一个会让运维误以为集群"已完整登记"，而平台
 * 实际只认识其中一个——baseline 推导依赖这份清单的完整性，漏一条
 * 后果是漏一条放行规则，事后表现为生产阻断而不是注册时的报错。
 */
function ApiServerList({ servers }: { servers?: APIServer[] | null }) {
  if (!servers || servers.length === 0) {
    return <span className="text-ink-muted">未配置</span>
  }
  return (
    <span className="mono" style={{
      display: 'flex', flexDirection: 'column', gap: 2, fontSize: 'var(--text-xs)',
    }}>
      {servers.map((s, i) => <span key={`${i}-${s.host}`}>{s.host}:{s.port}（{s.cidr}）</span>)}
    </span>
  )
}

/* ---------------------------------------------------------------------- */
/* 共享小件                                                                 */
/* ---------------------------------------------------------------------- */

function FormGrid({ children }: { children: ReactNode }) {
  return (
    <div style={{
      display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(200px, 1fr))',
      gap: 'var(--space-3)', marginBottom: 'var(--space-3)',
    }}>
      {children}
    </div>
  )
}

function SubHeading({ children }: { children: ReactNode }) {
  return (
    <div className="mb-2 text-xs text-ink-muted">
      {children}
    </div>
  )
}

const fieldLabelStyle: CSSProperties = {
  display: 'block', marginBottom: 4, fontSize: 'var(--text-xs)', color: 'var(--text-muted)',
}

function TextField({ label, value, onChange, required, mono, placeholder, readOnly, disabled }: {
  label: string
  value: string
  onChange: (v: string) => void
  required?: boolean
  mono?: boolean
  placeholder?: string
  readOnly?: boolean
  disabled?: boolean
}) {
  const inert = readOnly || disabled
  return (
    <label className="block">
      <span style={fieldLabelStyle}>
        {label}
        {/*
          必填要在填之前就看得出来，不能等提交之后才由报错告知。这张表单有
          21 个输入框、其中 4 个必填，标签一模一样时"我最少要填什么"只能靠
          点一次提交去问。

          用「必填」两个字而不是一个星号：星号要另有图例才读得懂，而这一格
          旁边没有图例。
        */}
        {required && (
          <span className="ml-1 text-xs font-normal text-ink-muted">必填</span>
        )}
      </span>
      <input
        className="ctl w-full"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        // 刻意**不设** required 属性。
        //
        // 它触发的是浏览器原生校验气泡，在一个全中文界面上弹出一句英文
        // Please fill out this field.，样式也与这套设计语言完全不搭；而且
        // 一次只提示第一个空框，要一个一个修。这张表单其余每一条错误都是
        // 中文应用层提示（buildClusterWrite / 服务端），必填也走同一条 ——
        // 一套提示，不是两套。
        placeholder={placeholder}
        readOnly={readOnly}
        disabled={disabled}
        style={{
          width: '100%',
          fontFamily: mono ? 'var(--mono)' : undefined,
          // 不可编辑的输入框必须看得出不可编辑：一个外观正常却打不进字的
          // 输入框会被读成界面卡住，而不是"这一项不能改"。
          background: inert ? 'var(--surface-sunken)' : undefined,
          color: inert ? 'var(--text-muted)' : undefined,
        }}
      />
    </label>
  )
}






/**
 * cniLabel 渲染集群的网络插件。
 *
 * **缺席与 UNKNOWN 都显示成「未认出」，不猜。** 老响应里没有这个键，
 * 而一个猜出来的 CNI 会让人据此判断"那些第二平面对象是死的、不用管"——
 * 猜错的方向是把一个真的在执行的平面当成死的。
 */
function cniLabel(cni: CNI | undefined): string {
  switch (cni) {
    case 'CILIUM':
      return 'Cilium'
    case 'CALICO':
      return 'Calico'
    default:
      return '未认出'
  }
}
