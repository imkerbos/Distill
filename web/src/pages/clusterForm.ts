import type {
  APIServer, ClusterWrite, GitBinding, GitBindingWrite, RegisteredCluster,
} from '../api/types'

/**
 * 集群表单的纯逻辑层：从已注册集群播种表单、把表单折算成一份写入体。
 *
 * 单独成文件而不是留在 ClustersPage.tsx 里，是因为注册与编辑用的是
 * 同一套规则（Git 绑定全有或全无、apiserver 每行全有或全无），而两份
 * 拷贝一定会漂移——漂移的落点是策略下发路径：一个只在注册表单拦得住
 * 的半截绑定，会在编辑表单被放进库里，然后在轮 3 变成一次指向不存在
 * 路径的写入。这里不 import 任何 React，测试可以直接跑它。
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

export interface GitFormValues {
  repoUrl: string
  branch: string
  policyPath: string
  credentialRef: string
}

/**
 * Git 绑定四个字段在表单里的合法组合只有两种：全空，或 repoUrl/branch/
 * policyPath 三项全填（credentialRef 可选，但只要它非空就已经表达了
 * "这是一处真实绑定"的意图，此时同样要求三项必填齐全）——否则
 * credentialRef 会在三项检查之外被静默丢弃，成为唯一录入了值却从不
 * 出现在提交请求里的字段。
 */
const REQUIRED_GIT_FIELDS: readonly ['repoUrl', 'branch', 'policyPath'] = ['repoUrl', 'branch', 'policyPath']

/** 表单的全部可编辑值。编辑与注册共用一套形状，只在 mode 上分叉。 */
export interface ClusterFormValues {
  id: string
  displayName: string
  podCidr: string
  nodeCidr: string
  apiServerRows: ApiServerRow[]
  /** 健康检查网段，每行一个 CIDR。 */
  healthChecks: string
  git: GitFormValues
  /** 操作者显式要求解除 Git 绑定；见 resolveGitBinding 的注释。 */
  clearGit: boolean
}

const emptyGitValues = (): GitFormValues => ({ repoUrl: '', branch: '', policyPath: '', credentialRef: '' })

/** 注册表单的初始值：全空。 */
export function blankFormValues(): ClusterFormValues {
  return {
    id: '', displayName: '', podCidr: '', nodeCidr: '',
    apiServerRows: [emptyApiServerRow()], healthChecks: '',
    git: emptyGitValues(), clearGit: false,
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
    // 至少留一行空行，否则界面上没有任何可填的输入框，"添加"按钮成了唯一入口。
    apiServerRows: rows.length > 0 ? rows : [emptyApiServerRow()],
    healthChecks: (c.healthCheckSources ?? []).join('\n'),
    git: c.git
      ? {
        repoUrl: c.git.repoUrl, branch: c.git.branch,
        policyPath: c.git.policyPath, credentialRef: c.git.credentialRef,
      }
      : emptyGitValues(),
    clearGit: false,
  }
}

export type GitResolution =
  | { ok: true; git: GitBindingWrite | null; summary: string }
  | { ok: false; error: string }

/**
 * 把 Git 表单折算成提交用的绑定，或给出拒绝理由。
 *
 * 编辑路径上多出一个注册路径没有的问题："四个字段都空着"到底是
 * "本来就没绑定"还是"我要解除绑定"。在整体替换的语义下这两者提交
 * 结果相同，但意图完全不同——把清空字段直接当成解除，等于让一次
 * 误删输入框内容静默切断集群与策略仓库的关联，而界面上不会有任何
 * 一步要求确认。因此解除必须由 clearRequested 这个显式动作表达；
 * 已有绑定却把字段清空又没勾选，是一个要求操作者澄清的错误，不是
 * 一个可以替他猜的默认值。
 */
export function resolveGitBinding(
  values: GitFormValues,
  ctx: { current: GitBinding | null; clearRequested: boolean },
): GitResolution {
  if (ctx.clearRequested) {
    return {
      ok: true,
      git: null,
      summary: ctx.current
        ? `提交后：解除 Git 绑定（当前为 ${ctx.current.repoUrl}@${ctx.current.branch}）`
        : '提交后：不绑定 Git 仓库',
    }
  }

  const anyFilled = Object.values(values).some((v) => v.trim() !== '')
  if (!anyFilled) {
    if (ctx.current) {
      return {
        ok: false,
        error: '要解除 Git 绑定请勾选「解除 Git 绑定」——把四个字段清空不等于'
          + '解除：整体替换下两者提交结果相同，但一次误删输入框内容会因此'
          + '静默切断集群与策略仓库的关联。',
      }
    }
    return { ok: true, git: null, summary: '提交后：不绑定 Git 仓库' }
  }

  const missing = REQUIRED_GIT_FIELDS.filter((k) => values[k].trim() === '')
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
    git: {
      repoUrl, branch, policyPath,
      credentialRef: values.credentialRef.trim(),
    },
    summary: `提交后：绑定到 ${repoUrl}@${branch}，路径 ${policyPath}`,
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
 * 把表单折算成一份完整的写入体。
 *
 * current 是编辑目标当前的 Git 绑定（注册时为 null），只用于回答
 * "把字段清空是不是要解除绑定"这一个问题，以及据此写出提交前的结果
 * 预告。它不参与任何服务端会自行推导的字段。
 *
 * 接入状态与漂移基准 lastWrittenCommit 都不在这里出现也不接受输入：
 * 前者由服务端根据实际采集到的数据推进，后者是平台对 Git 仓库现状的
 * 断言 —— 表单能提交它们，就等于允许把"还没有数据"标成"可以出推荐了"、
 * 把"无漂移"变成一句无法被证伪的话。
 */
export function buildClusterWrite(
  values: ClusterFormValues,
  current: GitBinding | null,
): BuildResult {
  const git = resolveGitBinding(values.git, { current, clearRequested: values.clearGit })
  if (!git.ok) return { ok: false, error: git.error }

  const servers = resolveApiServers(values.apiServerRows)
  if (!servers.ok) return { ok: false, error: servers.error }

  return {
    ok: true,
    summary: git.summary,
    body: {
      id: values.id.trim(),
      displayName: values.displayName.trim(),
      podCidr: values.podCidr.trim(),
      nodeCidr: values.nodeCidr.trim(),
      apiServers: servers.apiServers,
      healthCheckSources: values.healthChecks.split('\n').map((s) => s.trim()).filter(Boolean),
      git: git.git,
    },
  }
}
