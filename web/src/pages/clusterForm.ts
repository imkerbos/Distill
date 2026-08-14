import type {
  APIServer, ClusterWrite, GitBinding, GitBindingWrite, RegisteredCluster,
  VerifyResult, VerifyStatus,
} from '../api/types'

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

/** Git 绑定表单的可编辑值。四项与 GitBindingWrite 一一对应，不多不少。 */
export interface GitFormValues {
  repoUrl: string
  branch: string
  policyPath: string
  credentialRef: string
}

/**
 * repoUrl / branch / policyPath 三项必填，credentialRef 可选。
 *
 * 绑定表单只有一个动作（提交 = 绑定或改绑），所以这里不再有"全空"这个
 * 合法组合：想解除绑定按「解除绑定」，那是一次 DELETE。上一轮为了在
 * 整体替换下区分"我清空了字段"与"我要解绑"而设的那套消歧义逻辑，随它
 * 的成因一起消失（design doc 2026-08-13 §7）。
 */
const REQUIRED_GIT_FIELDS: readonly ['repoUrl', 'branch', 'policyPath'] = ['repoUrl', 'branch', 'policyPath']

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
export const blankGitValues = (): GitFormValues =>
  ({ repoUrl: '', branch: '', policyPath: '', credentialRef: '' })

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
 * 只播种四个可写字段：verifyResult / verifiedAt / lastWrittenCommit 是
 * 平台自己产生的东西，一旦进了表单状态，下一个人就会顺手把它们提交上去。
 */
export function gitFormValuesOf(git: GitBinding | null | undefined): GitFormValues {
  if (!git) return blankGitValues()
  return {
    repoUrl: git.repoUrl, branch: git.branch,
    policyPath: git.policyPath, credentialRef: git.credentialRef,
  }
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
 * 是什么"，"四个字段都空着"也不再有歧义：它就是三项必填没填。
 *
 * 三项必填在填了任意一项（含 credentialRef）时才报"缺少"，一项都没填
 * 时报"请填写"：前者说的是一份填了一半的绑定，credentialRef 单独填写
 * 同样触发，否则它录入的值会在三项检查之外被静默丢弃。
 */
export function resolveGitBinding(values: GitFormValues): GitResolution {
  const missing = REQUIRED_GIT_FIELDS.filter((k) => values[k].trim() === '')
  if (missing.length === REQUIRED_GIT_FIELDS.length && values.credentialRef.trim() === '') {
    return {
      ok: false,
      error: '请填写 repoUrl / branch / policyPath。要解除这个集群的 Git 绑定，'
        + '请用「解除绑定」——把输入框清空不会解除任何东西。',
    }
  }
  if (missing.length > 0) {
    return {
      ok: false,
      error: `Git 绑定缺少：${missing.join('、')}。repoUrl / branch / policyPath 三项在你填写了 `
        + `Git 绑定的任意一项（含 credentialRef）时都是必需的，否则已填的值不会被保存。`,
    }
  }

  const repoUrl = values.repoUrl.trim()
  const branch = values.branch.trim()
  const policyPath = values.policyPath.trim()
  // 提交体里没有 lastWrittenCommit：漂移基准是平台自己的断言，由服务端
  // 从库里的现值推导（同一仓库同一分支沿用，改指向则归零）。客户端即使
  // 带上它也不会被采纳——一个能被调用方设定的基准可以被调成与仓库现状
  // 一致，于是"无漂移"这句话再也无法被证伪。
  return {
    ok: true,
    binding: {
      repoUrl, branch, policyPath,
      credentialRef: values.credentialRef.trim(),
    },
    summary: `提交后：绑定到 ${repoUrl}@${branch}，路径 ${policyPath}`,
  }
}

/* ---------------------------------------------------------------------- */
/* Git 绑定校验结论的展示形态                                                */
/* ---------------------------------------------------------------------- */

/**
 * 结论的语气分档。只有三档，且刻意不做成"分数"：
 * 「没查过」与「查了、没通过」不是程度差别，是不同性质的事实。
 */
export type VerifyTone = 'ok' | 'bad' | 'unverified'

export interface VerifyStatusView {
  /** 结论本身，一律非空。 */
  label: string
  /** 一句话说明这个结论意味着什么、该找谁。 */
  detail: string
  tone: VerifyTone
  /** 校验时刻的说明，已明示是历史事实而非当前状态。 */
  checkedAt: string
}

/**
 * 校验结论的中文标签。
 *
 * 键类型是封闭枚举而不是 string：后端新增一个取值却忘了在这里补文案，
 * `tsc` 会直接报错，而不是让界面渲染出一个空白的结论列（同 PolicyPage 的
 * WORKLOAD_EXCLUSION_REASON_LABEL）。这里比那处更要紧一档——空白的排除
 * 原因只是少了一条解释，空白的校验结论会被读成「这个绑定没问题」。
 *
 * PATH_MISSING 说的是「路径不存在」，不是「不可写入」：校验只做只读操作，
 * 它查的是路径在不在，从来没试过往那里放东西（design doc §3.1）。
 */
const VERIFY_RESULT_LABEL: Record<VerifyResult, string> = {
  NOT_VERIFIED: '未校验',
  OK: '只读校验通过',
  CREDENTIAL_UNRESOLVED: '凭据取不到',
  AUTH_FAILED: '认证被拒绝',
  REPO_UNREACHABLE: '仓库不可达',
  BRANCH_MISSING: '分支不存在',
  PATH_MISSING: '路径不存在',
}

/**
 * 每条结论的处置说明。
 *
 * CREDENTIAL_UNRESOLVED 与 AUTH_FAILED 的文字必须让人一眼看出该找谁：
 * 前者是平台侧配置错，后者是仓库侧权限错，处置人不是同一个人，读者把
 * 两者混在一起就会去敲错人的门（design doc §3.2）。
 *
 * OK 这一条的措辞是本页最容易出事的地方：只读校验没有向仓库放过任何
 * 东西，所以它证明不了平台能往那个路径放东西。整张表里不出现「写」这
 * 个字，正是为了让这条约束有一个能被测试抓住的形状——见 clusterForm
 * 测试里那条禁用词断言。
 */
const VERIFY_RESULT_DETAIL: Record<VerifyResult, string> = {
  NOT_VERIFIED: '这个绑定从未被校验过。没查过不等于查过没问题，两者是相反的事实。',
  OK: '仓库可达、认证通过，分支与路径都存在。只读校验能得出的结论到此为止：'
    + '它没有向仓库提交过任何内容，因此也证明不了平台能往这个路径提交。',
  CREDENTIAL_UNRESOLVED: 'credentialRef 在凭据服务里解析不到，是平台侧的配置问题，找平台管理员。',
  AUTH_FAILED: '凭据取到了，但仓库拒绝了这次连接，是仓库侧的权限问题，找仓库管理员。',
  REPO_UNREACHABLE: '网络、地址或超时导致没能到达仓库，分支与路径都还没来得及查。',
  BRANCH_MISSING: '仓库可达、认证通过，但这个分支在仓库里找不到。',
  PATH_MISSING: 'policyPath 在该分支上找不到。校验只做只读查询，查的是路径在不在。',
}

/**
 * 结论到语气的映射。
 *
 * NOT_VERIFIED 单独一档，不并进 bad 也不并进 ok：它在界面上必须与 OK
 * 一眼可分，而与「查了没过」也不是一回事——前者要去按一次校验，后者
 * 要去修一个系统。
 */
const VERIFY_RESULT_TONE: Record<VerifyResult, VerifyTone> = {
  NOT_VERIFIED: 'unverified',
  OK: 'ok',
  CREDENTIAL_UNRESOLVED: 'bad',
  AUTH_FAILED: 'bad',
  REPO_UNREACHABLE: 'bad',
  BRANCH_MISSING: 'bad',
  PATH_MISSING: 'bad',
}

/**
 * 全部已登记的结论取值。
 *
 * 从文案表的键推导而不是另写一份字面量数组：文案表的键类型是
 * `Record<VerifyResult, string>`，`tsc` 已经保证它一个不多一个不少，
 * 再抄一份只会多出一处可以漂移的地方。
 */
export const ALL_VERIFY_RESULTS = Object.keys(VERIFY_RESULT_LABEL) as VerifyResult[]

/**
 * 把一个 RFC3339 时刻格式化成 UTC 展示串。
 *
 * 解析不出来时原样返回而不是抛错：一个格式意外的时间戳该让那一格显示
 * 得难看，不该让整张表白屏——同一张表里还有别的集群的登记信息要看。
 */
export function formatUtcTime(iso: string): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return d.toISOString().replace('T', ' ').replace(/\.\d+Z$/, ' UTC')
}

/**
 * 把校验时刻写成一句明确的历史陈述。
 *
 * 不能只甩一个时间戳：一个孤零零的时间挨着「只读校验通过」，读起来就是
 * 「现在是通过的」。轮 4 写回前必须重新校验，拿几天前的结论当此刻的状态
 * 正是 design doc §3.4 禁止的那件事，界面上的措辞是第一道拦截。
 */
function describeCheckedAt(at: string | null | undefined): string {
  if (!at) return '从未校验过'
  return `上次校验于 ${formatUtcTime(at)} —— 这是当时的结论，不代表此刻的状态`
}

/**
 * 把一个绑定的校验结论折算成可直接渲染的形态。
 *
 * 单独成函数而不是在组件里就地查表，是为了让这段判断能被测试直接跑：
 * 组件是 .tsx，`node --test` 的类型擦除读不了 JSX，留在组件里就等于
 * 这套文案永远没有测试。
 *
 * 未登记的取值不按原样透出，也不留空，而是收窄成「未校验」的语气：
 * 结论字段是封闭枚举，一个界面还不认识的值只能说明文案没跟上，此时
 * 失败方向朝「未确认」关，不朝「可信」开（与后端 VerifyResult.Valid
 * 的收窄同一条纪律）。
 */
export function describeVerifyStatus(git: GitBinding): VerifyStatusView {
  const result = git.verifyResult
  if (!Object.hasOwn(VERIFY_RESULT_LABEL, result)) {
    return {
      label: '结论无法识别',
      detail: `服务端给出了本界面还不认识的结论「${String(result)}」。`
        + '在文案补齐之前一律按未校验处置：不认识的结论不是通过了的结论。',
      tone: 'unverified',
      checkedAt: describeCheckedAt(git.verifiedAt),
    }
  }
  return {
    label: VERIFY_RESULT_LABEL[result],
    detail: VERIFY_RESULT_DETAIL[result],
    tone: VERIFY_RESULT_TONE[result],
    checkedAt: describeCheckedAt(git.verifiedAt),
  }
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
/* 一次校验请求的回执                                                        */
/* ---------------------------------------------------------------------- */

export interface VerifyOutcomeView {
  /** 这次请求是否真的发生了一次校验。 */
  happened: boolean
  /** 给操作者看的一句话回执，一律非空。 */
  message: string
  tone: VerifyTone
}

/**
 * 把保存/重校验端点的响应折算成一句回执。
 *
 * 要处理的是一个会让人读错的形状：未配置校验器时，服务端返回
 * `NOT_VERIFIED` 且 `verifiedAt` 缺席，并且**刻意什么都不落库** ——
 * 写一个时间戳就等于宣称某时某刻校验过一次，而那件事没有发生
 * （internal/httpapi/gitverify_handler.go）。
 *
 * 于是响应会与重新加载后看到的东西不一致：响应说"从未校验"，库里那行
 * 却还留着更早的结论。两者都没错，它们回答的是不同的问题 —— 响应说的是
 * "这一次发生了什么"，列表说的是"库里现在记着什么"。
 *
 * 处置：`verifiedAt` 缺席一律读作"这次没有发生校验"，回执明说这一点，
 * 且**不把 NOT_VERIFIED 当成一个新结论贴到界面上**。把它渲染成一个崭新的
 * 「未校验」，操作者会以为自己刚刚把结论刷掉了，而下一次刷新页面它又变
 * 回旧结论 —— 一个自己会变回去的界面，比一个说"什么都没发生"的界面更
 * 难被信任。列表那一格照旧由服务端的读模型驱动，不受这条回执影响。
 */
export function describeVerifyOutcome(status: VerifyStatus): VerifyOutcomeView {
  if (!status.verifiedAt) {
    return {
      happened: false,
      // 措辞要同时对两种情形成立：库里已有一个更早的结论（重校验），以及
      // 库里本来就没有结论（刚绑上）。说成「仍是上一次校验的结论」在后者
      // 是假的——那里从来没有过一次校验。
      message: '这次没有发生校验：平台未配置校验器（或校验器给出了未登记的结论），'
        + '服务端因此没有记下任何结论——列表里那一格是库里原有的结论，不是刚刚得出的。',
      tone: 'unverified',
    }
  }
  const label = Object.hasOwn(VERIFY_RESULT_LABEL, status.verifyResult)
    ? VERIFY_RESULT_LABEL[status.verifyResult]
    : '结论无法识别'
  const tone = Object.hasOwn(VERIFY_RESULT_TONE, status.verifyResult)
    ? VERIFY_RESULT_TONE[status.verifyResult]
    : 'unverified'
  return {
    happened: true,
    message: `校验于 ${formatUtcTime(status.verifiedAt)} 完成：${label}。`,
    tone,
  }
}
