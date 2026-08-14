import type {
  APIServer, ClusterWrite, GitBinding, GitBindingWrite, PathVerifyResult,
  PathVerifyStatus, RegisteredCluster,
} from '../api/types'
import {
  describeOutcome, describeVerdict,
  type VerdictCopy, type VerifyOutcomeView, type VerifyStatusView,
} from './verifyView.ts'

/**
 * 集群表单与 Git 绑定表单的纯逻辑层：从已注册集群播种表单、把表单折算
 * 成写入体、把服务端的校验结论折算成可渲染的形态。
 *
 * 单独成文件而不是留在 ClustersPage.tsx 里，是因为注册与编辑用的是
 * 同一套规则（apiserver 每行全有或全无），而两份拷贝一定会漂移——漂移
 * 的落点是策略下发路径。这里不 import 任何 React，测试可以直接跑它。
 *
 * 两份表单折算成两份互不相干的写入体，对应两个端点：集群写路径不碰
 * 绑定，绑定写路径不碰集群。这条分界在类型上就成立（ClusterWrite 没有
 * git 的位置），不靠调用点自觉。
 */

/**
 * apiserver 表单行的本地形态：port 保持字符串，交给用户在提交前编辑；提交时才转数字。
 * 三个字段的初始值都是空串（port 不预填 "443"，只作为 placeholder 提示）——
 * 让"这一行完全没被碰过"在数据层面等价于"三个字段都是空串"，提交时才能
 * 用同一条"全空则忽略、有一项非空则三项必填"的规则处理，不用另开一条
 * "port 的默认值不算真的填了"的特例。
 */
export interface ApiServerRow { host: string; cidr: string; port: string }
export const emptyApiServerRow = (): ApiServerRow => ({ host: '', cidr: '', port: '' })
const REQUIRED_APISERVER_FIELDS: readonly ['host', 'cidr', 'port'] = ['host', 'cidr', 'port']

/**
 * Git 绑定表单的可编辑值。两项与 GitBindingWrite 一一对应，不多不少。
 *
 * 仓库地址、分支与凭据**不在这里**：它们属于仓库，改它们去仓库页
 * （design doc 2026-08-13 §3.2、§5）。放在这里的后果不是多两个输入框——
 * 服务端的 gitBindingPayload 根本不收它们，操作者改完地址、请求返回成功、
 * 界面显示保存生效，而平台真正会去连的仓库原封不动。
 */
export interface GitFormValues {
  repoId: string
  policyPath: string
}

/**
 * 两项都必填。
 *
 * 绑定表单只有一个动作（提交 = 绑定或改绑），所以这里不再有"全空"这个
 * 合法组合：想解除绑定按「解除绑定」，那是一次 DELETE。上一轮为了在
 * 整体替换下区分"我清空了字段"与"我要解绑"而设的那套消歧义逻辑，随它
 * 的成因一起消失（design doc 2026-08-13 §7）。
 */
const REQUIRED_GIT_FIELDS: readonly ['repoId', 'policyPath'] = ['repoId', 'policyPath']

/**
 * 集群表单的全部可编辑值。编辑与注册共用一套形状，只在 mode 上分叉。
 *
 * **不含 Git 绑定**：绑定由 GitFormValues 独立承载、独立提交。两者混在
 * 一份表单状态里，就等于一次保存同时写两个资源——而服务端已经不再从
 * 集群写路径接受绑定，混着的那一半会静默落空。
 */
export interface ClusterFormValues {
  id: string
  displayName: string
  podCidr: string
  nodeCidr: string
  /**
   * 集群里另有 CiliumClusterwideNetworkPolicy 在生效。
   *
   * 它不是一个"技术开关"，而是一句会改变判定结论的事实声明：置真会让
   * 该集群的回放判定整体降级为 DEGRADED（后端 replay.WithCCNPPresent）。
   * 表单必须携带并预填它——PUT 是整体替换，不带就等于每次编辑都悄悄
   * 把它清成 false，让平台显得比它应该的样子更有把握。
   */
  ccnpPresent: boolean
  apiServerRows: ApiServerRow[]
  /** 健康检查网段，每行一个 CIDR。 */
  healthChecks: string
}

/** 绑定表单的初始值：全空，对应"这个集群还没有绑定"。 */
export const blankGitValues = (): GitFormValues => ({ repoId: '', policyPath: '' })

/** 注册表单的初始值：全空。 */
export function blankFormValues(): ClusterFormValues {
  return {
    id: '', displayName: '', podCidr: '', nodeCidr: '', ccnpPresent: false,
    apiServerRows: [emptyApiServerRow()], healthChecks: '',
  }
}

/**
 * 用集群现值播种编辑表单。
 *
 * 每一项都必须播种，一项都不能省：PUT 是整体替换，表单里空着的字段
 * 提交后就是库里空着的字段。漏播 apiServers 的后果不是"少改了一处"，
 * 而是一个只想改仓库地址的操作者顺手清空了 apiserver 清单，进而少掉
 * 一条 control-plane 放行规则——事后表现为生产阻断，而不是提交时报错。
 */
export function formValuesOf(c: RegisteredCluster): ClusterFormValues {
  const rows = (c.apiServers ?? []).map((s) => ({
    host: s.host, cidr: s.cidr, port: String(s.port),
  }))
  return {
    id: c.id,
    displayName: c.displayName,
    podCidr: c.podCidr,
    nodeCidr: c.nodeCidr,
    // 与其余字段同一条纪律，但后果的方向不同：漏播网段会让判定用错的
    // 网段回答，漏播这一项会让一个本该降级的集群显示成正常判定——
    // 前者是错误，后者是"看上去更有把握的错误"，更难被发现。
    ccnpPresent: c.ccnpPresent,
    // 至少留一行空行，否则界面上没有任何可填的输入框，"添加"按钮成了唯一入口。
    apiServerRows: rows.length > 0 ? rows : [emptyApiServerRow()],
    healthChecks: (c.healthCheckSources ?? []).join('\n'),
  }
}

/**
 * 用现有绑定播种绑定表单；未绑定时给一份空值。
 *
 * 只播种两个可写字段：verifyResult / verifiedAt / lastWrittenCommit 是
 * 平台自己产生的东西，一旦进了表单状态，下一个人就会顺手把它们提交上去。
 */
export function gitFormValuesOf(git: GitBinding | null | undefined): GitFormValues {
  if (!git) return blankGitValues()
  return { repoId: git.repoId, policyPath: git.policyPath }
}

export type GitResolution =
  | { ok: true; binding: GitBindingWrite; summary: string }
  | { ok: false; error: string }

/**
 * 把 Git 表单折算成一份绑定写入体，或给出拒绝理由。
 *
 * 这里只有一个去向：绑定或改绑。解除不由它表达 —— 那是 DELETE
 * /clusters/{id}/git-binding，界面上是一个单独的按钮
 * （design doc 2026-08-13 §5、§7）。因此本函数不再需要知道"当前绑定
 * 是什么"，"两个字段都空着"也不再有歧义：它就是两项必填没填。
 *
 * **折算不出仓库地址**：提交体里只有 repoId 与 policyPath，平台去连哪个
 * 地址由那个 repoId 指向的仓库说了算。想改地址去仓库页 —— 从这里带一份
 * 地址过去，服务端不收，界面却会显示保存生效。
 */
export function resolveGitBinding(values: GitFormValues): GitResolution {
  const missing = REQUIRED_GIT_FIELDS.filter((k) => values[k].trim() === '')
  if (missing.length === REQUIRED_GIT_FIELDS.length) {
    return {
      ok: false,
      error: '请选择一个已登记的仓库并填写 policyPath。要解除这个集群的 Git 绑定，'
        + '请用「解除绑定」——把输入框清空不会解除任何东西。',
    }
  }
  if (missing.length > 0) {
    return {
      ok: false,
      error: `Git 绑定缺少：${missing.join('、')}。repoId 与 policyPath 两项必须同时给出，`
        + `否则已填的值不会被保存。仓库地址、分支与凭据属于仓库，改它们去仓库页。`,
    }
  }

  const repoId = values.repoId.trim()
  const policyPath = values.policyPath.trim()
  // 提交体里没有 lastWrittenCommit：漂移基准是平台自己的断言，由服务端
  // 从库里的现值推导。客户端即使带上它也不会被采纳——一个能被调用方设定
  // 的基准可以被调成与仓库现状一致，于是"无漂移"这句话再也无法被证伪。
  return {
    ok: true,
    binding: { repoId, policyPath },
    summary: `提交后：绑定到仓库 ${repoId}，路径 ${policyPath}`,
  }
}

/* ---------------------------------------------------------------------- */
/* 路径级校验结论的展示形态                                                   */
/* ---------------------------------------------------------------------- */

/**
 * 路径级结论的全部文案。
 *
 * 只有三个取值：仓库级那四个失败取值不在这一层，它们在 gitRepoForm.ts
 * （design doc §3.3）。这一层只回答一个问题 —— policyPath 在仓库的那个
 * 分支上是否存在。
 *
 * 每一句都点名「路径」这一层，且与仓库级那张表逐条不同：两层共用
 * NOT_VERIFIED 与 OK 两个枚举值，文案若也一样，把仓库级结论渲染到这一格
 * 上在界面上就完全看不出来 —— 而那正是上一次前端缺陷的形状。
 *
 * PATH_MISSING 说的是「路径不存在」，不是「不可写入」：校验只做只读操作，
 * 它查的是路径在不在，从来没试过往那里放东西（design doc §3.1）。
 *
 * NOT_VERIFIED 的说明必须把「仓库级没通过」这种情形也说到：路径级以仓库级
 * 为前提，仓库都没连上时这一层只会是未校验，而操作者看到的若只有「从未
 * 校验过」，就会以为是自己忘了点按钮。
 *
 * OK 这一条的措辞是本表最容易出事的地方：只读校验没有向仓库放过任何东西，
 * 所以它证明不了平台能往那个路径放东西。整张表里不出现「写」这个字，正是
 * 为了让这条约束有一个能被测试抓住的形状——见测试里那条禁用词断言。
 */
const PATH_VERIFY_COPY: VerdictCopy<PathVerifyResult> = {
  label: {
    NOT_VERIFIED: '路径未校验',
    OK: '路径只读校验通过',
    PATH_MISSING: '路径不存在',
  },
  detail: {
    NOT_VERIFIED: '这个路径从未被校验过，或者仓库那一层此刻没有通过——路径级以仓库级为前提，'
      + '仓库都没到达过时不会给出关于路径的结论。没查过不等于查过没问题，两者是相反的事实。',
    OK: 'policyPath 在该仓库的那个分支上存在。只读校验能得出的结论到此为止：'
      + '它没有向仓库提交过任何内容，因此也证明不了平台能往这个路径提交。',
    PATH_MISSING: '仓库那一层已经通过，但 policyPath 在该分支上找不到。'
      + '校验只做只读查询，查的是路径在不在。',
  },
  tone: {
    // NOT_VERIFIED 单独一档，不并进 bad 也不并进 ok：它在界面上必须与 OK
    // 一眼可分，而与「查了没过」也不是一回事——前者要去按一次校验（或先把
    // 仓库那一层修好），后者要去改 policyPath。
    NOT_VERIFIED: 'unverified',
    OK: 'ok',
    PATH_MISSING: 'bad',
  },
}

/**
 * 全部已登记的路径级取值。
 *
 * 从文案表的键推导而不是另写一份字面量数组：文案表的键类型是
 * `Record<PathVerifyResult, string>`，`tsc` 已经保证它一个不多一个不少，
 * 再抄一份只会多出一处可以漂移的地方。
 */
export const ALL_PATH_VERIFY_RESULTS = Object.keys(PATH_VERIFY_COPY.label) as PathVerifyResult[]

/**
 * 把一个绑定的**路径级**校验结论折算成可渲染的形态。
 *
 * 参数是 GitBinding 而不是一个裸枚举：仓库页手上只有 GitRepo，于是「把
 * 路径级结论渲染到仓库那一格」在那里根本编译不过。集群页上两者都在作用域
 * 里（那一段要只读展示仓库地址），那一处挡不住 —— 由测试里的组件源码断言
 * 兜一层，见它对自身局限的说明。
 */
export function describePathVerifyStatus(git: GitBinding): VerifyStatusView {
  return describeVerdict(PATH_VERIFY_COPY, git.verifyResult, git.verifiedAt)
}

export type ApiServerResolution =
  | { ok: true; apiServers: APIServer[] }
  | { ok: false; error: string }

/**
 * 每一行独立按"全空则忽略、有一项非空则三项必填"判断——与 Git 绑定
 * 同一条纪律，且不能只查 host：只查 host 会把"填了 cidr/port 但漏了
 * host"的行放过去，静默丢弃已经打进去的字符。行号用界面上看到的序号
 * （从 1 开始，过滤前的下标），不用提交前过滤之后的下标——否则一旦
 * 前面有整行空白被跳过，报错里的行号就和操作者盯着的表单对不上。
 */
export function resolveApiServers(rows: ApiServerRow[]): ApiServerResolution {
  const apiServers: APIServer[] = []
  const errors: string[] = []
  rows.forEach((r, idx) => {
    const anyFilled = REQUIRED_APISERVER_FIELDS.some((k) => r[k].trim() !== '')
    if (!anyFilled) return
    const missing = REQUIRED_APISERVER_FIELDS.filter((k) => r[k].trim() === '')
    if (missing.length > 0) {
      errors.push(`apiserver 第 ${idx + 1} 行缺少：${missing.join('、')}`)
      return
    }
    apiServers.push({ host: r.host.trim(), cidr: r.cidr.trim(), port: Number(r.port) || 0 })
  })
  if (errors.length > 0) {
    return {
      ok: false,
      error: `${errors.join('；')}。每一行 host / cidr / port 要么三项都填，要么整行留空`
        + `（留空的行会被忽略，不会提交），否则已填的值不会被保存。`,
    }
  }
  return { ok: true, apiServers }
}

export type BuildResult =
  | { ok: true; body: ClusterWrite; summary: string }
  | { ok: false; error: string }

/**
 * 把集群表单折算成一份完整的写入体。
 *
 * **提交体里没有 git**：绑定是一个有自己生命周期的资源，由绑定表单
 * 独立提交（design doc 2026-08-13 §5、§7）。服务端的 clusterPayload
 * 已经不再接受它 —— 在这里塞一个 git 字段，请求照样返回成功，绑定却
 * 原封不动，而界面会显示这次保存生效了。
 *
 * 接入状态同样不在这里出现也不接受输入：它由服务端根据实际采集到的
 * 数据推进，表单能提交它就等于允许把"还没有数据"标成"可以出推荐了"。
 *
 * ccnpPresent 相反，必须在这里且必须原样带上：它是操作者声明的一个
 * 事实，没有任何自动探测在维护它，而 PUT 是整体替换 —— 漏带一次，
 * 一个本该整体降级为 DEGRADED 的集群就会给出笃定的判定。
 */
export function buildClusterWrite(values: ClusterFormValues): BuildResult {
  const servers = resolveApiServers(values.apiServerRows)
  if (!servers.ok) return { ok: false, error: servers.error }

  return {
    ok: true,
    summary: `提交后：整体替换 ${values.id.trim()} 的登记信息（不含 Git 绑定）`,
    body: {
      id: values.id.trim(),
      displayName: values.displayName.trim(),
      podCidr: values.podCidr.trim(),
      nodeCidr: values.nodeCidr.trim(),
      ccnpPresent: values.ccnpPresent,
      apiServers: servers.apiServers,
      healthCheckSources: values.healthChecks.split('\n').map((s) => s.trim()).filter(Boolean),
    },
  }
}

/* ---------------------------------------------------------------------- */
/* 一次路径级校验请求的回执                                                   */
/* ---------------------------------------------------------------------- */

/**
 * 把绑定保存/路径级重校验端点的响应折算成一句回执。
 *
 * 与仓库级那条（describeRepoVerifyOutcome）共用同一段逻辑、各用各的文案表：
 * 「这次到底有没有发生一次校验」两层的判断完全一样，说法却必须分得开。
 */
export function describePathVerifyOutcome(status: PathVerifyStatus): VerifyOutcomeView {
  return describeOutcome(PATH_VERIFY_COPY, status)
}
