import type { GitRepo, GitRepoWrite, RepoVerifyResult, RepoVerifyStatus } from '../api/types'
import {
  describeOutcome, describeVerdict,
  type VerdictCopy, type VerifyOutcomeView, type VerifyStatusView,
} from './verifyView.ts'

/**
 * 策略仓库表单的纯逻辑层：从仓库现值播种表单、把表单折算成写入体、
 * 把服务端的**仓库级**结论折算成可渲染的形态。
 *
 * 与 clusterForm.ts 分列而不是合成一份「Git 表单逻辑」：仓库与绑定是两个
 * 资源，两层结论是两个各自封闭的枚举。合成一份的第一个后果就是有人把
 * 仓库级的 AUTH_FAILED 查进路径级那张表 —— 那句话会显示在 policyPath
 * 那一格上，读的人会去改一个根本没问题的路径（design doc §3.3）。
 *
 * 这里不 import 任何 React，测试可以直接跑它。
 */

/** 仓库表单的可编辑值。 */
export interface RepoFormValues {
  repoId: string
  repoUrl: string
  branch: string
  credentialRef: string
  /**
   * SSH 私钥（PEM），**只入不出**。
   *
   * 编辑既有仓库时它永远从空开始，而不是从库里播种：库里根本读不回来
   * （服务端不回显，GitRepo 上也没有这个字段）。空表示"不动现有凭据"，
   * 于是一个只想改分支的人不会顺手把钥匙清掉。
   */
  privateKey: string
}

/**
 * repoId / repoUrl / branch 三项必填，credentialRef 可选。
 *
 * credentialRef 可以为空是服务端的规则（ValidateGitRepo）：仓库可以先登记
 * 下来，凭据稍后再配。这里照着它，不另立一条更严的门槛 —— 前端比后端更严
 * 的字段规则，表现为一个「后端明明收得下、界面却不让提交」的死角。
 */
const REQUIRED_REPO_FIELDS: readonly ['repoId', 'repoUrl', 'branch'] = ['repoId', 'repoUrl', 'branch']

/** 新建表单的初始值：全空。 */
export const blankRepoValues = (): RepoFormValues =>
  ({ repoId: '', repoUrl: '', branch: '', credentialRef: '', privateKey: '' })

/**
 * 用仓库现值播种编辑表单。
 *
 * 只播种四个可写字段：verifyResult / verifiedAt 是平台自己产生的结论，
 * 一旦进了表单状态，下一个人就会顺手把它们提交上去——而一个能由调用方
 * 提交的 OK 再也无法被证伪。
 *
 * 每一项都必须播种：PUT 是整体替换，表单里空着的字段提交后就是库里空着的
 * 字段。一个只想改分支的操作者不该因此清掉 credentialRef —— 少掉的是平台
 * 取凭据的那个引用，事后表现为下一次校验报「凭据取不到」。
 */
export function repoFormValuesOf(r: GitRepo): RepoFormValues {
  return {
    repoId: r.repoId, repoUrl: r.repoUrl, branch: r.branch, credentialRef: r.credentialRef,
    // 私钥不播种，也播种不了：它读不回来。空在提交时表示"不动现有凭据"
    // ——与其余字段"空就是清空"的语义相反，因此这一条单独写在这里。
    privateKey: '',
  }
}

export type RepoResolution =
  | { ok: true; repo: GitRepoWrite; summary: string }
  | { ok: false; error: string }

/**
 * 把仓库表单折算成一份写入体，或给出拒绝理由。
 *
 * **这里刻意不判断 repoUrl 是不是 SSH 形态**：那条规则由服务端的
 * ValidateGitRepo 判定，它给出的那句话（点名 https:// 会导致一句假的
 * 「仓库不可达」结论）比任何前端复述都更准，而复述一份必然与它漂开。
 * 前端不是安全边界（规范 §34）—— 这里只挡住「填了一半」这种连请求都
 * 不值得发的情形，其余交给服务端，并原样展示它的拒绝理由。
 */
export function resolveGitRepo(values: RepoFormValues): RepoResolution {
  const missing = REQUIRED_REPO_FIELDS.filter((k) => values[k].trim() === '')
  if (missing.length > 0) {
    return {
      ok: false,
      error: `仓库缺少：${missing.join('、')}。repoId / repoUrl / branch 三项必填，`
        + 'credentialRef 可留空（仓库可以先登记下来，凭据稍后再配）。',
    }
  }
  const repoId = values.repoId.trim()
  const repoUrl = values.repoUrl.trim()
  const branch = values.branch.trim()
  // 私钥只 trim 首尾空白，**不做任何其它整形**：PEM 的换行是内容的一部分，
  // 顺手规范化会把一把好钥匙改成解析不了的那种，而失败要到写回才显形。
  const privateKey = values.privateKey.trim()
  const repo: GitRepoWrite = {
    repoId, repoUrl, branch, credentialRef: values.credentialRef.trim(),
  }
  // 空不带上去：缺省表示"不动现有凭据"，而带一个空串上去是"把凭据清成空"。
  if (privateKey !== '') {
    repo.privateKey = privateKey
  }
  return {
    ok: true,
    repo,
    summary: privateKey === ''
      ? `提交后：仓库 ${repoId} 指向 ${repoUrl}@${branch}`
      : `提交后：仓库 ${repoId} 指向 ${repoUrl}@${branch}，并保存一把新的 SSH 私钥`,
  }
}

/* ---------------------------------------------------------------------- */
/* 仓库级校验结论的展示形态                                                   */
/* ---------------------------------------------------------------------- */

/**
 * 仓库级结论的全部文案。
 *
 * 每一句都点名「仓库」这一层：这六个取值与路径级那三个共用 NOT_VERIFIED
 * 与 OK 两个枚举值，文案若也一样，把仓库级结论渲染到路径级那一格上（或者
 * 反过来）在界面上就完全看不出来 —— 而那正是上一次前端缺陷的形状：组件
 * 接错了数据源，四道门禁全绿。
 *
 * CREDENTIAL_UNRESOLVED 与 AUTH_FAILED 的文字必须让人一眼看出该找谁：
 * 前者是平台侧配置错，后者是仓库侧权限错，处置人不是同一个人，读者把
 * 两者混在一起就会去敲错人的门（design doc §3.2）。
 *
 * OK 这一条的措辞是本表最容易出事的地方：只读校验没有向仓库放过任何
 * 东西，所以它证明不了平台能往那个仓库放东西。整张表里不出现「写」这
 * 个字，正是为了让这条约束有一个能被测试抓住的形状——见测试里那条禁用词
 * 断言。
 */
const REPO_VERIFY_COPY: VerdictCopy<RepoVerifyResult> = {
  label: {
    NOT_VERIFIED: '仓库未校验',
    OK: '仓库只读校验通过',
    CREDENTIAL_UNRESOLVED: '凭据取不到',
    AUTH_FAILED: '认证被拒绝',
    REPO_UNREACHABLE: '仓库不可达',
    BRANCH_MISSING: '分支不存在',
  },
  detail: {
    NOT_VERIFIED: '这个仓库从未被校验过。没查过不等于查过没问题，两者是相反的事实。',
    OK: '凭据可解析、仓库可达、认证通过、分支存在。只读校验能得出的结论到此为止：'
      + '它没有向仓库提交过任何内容，因此也证明不了平台能往这个仓库提交。',
    CREDENTIAL_UNRESOLVED: 'credentialRef 在凭据服务里解析不到，是平台侧的配置问题，找平台管理员。',
    AUTH_FAILED: '凭据取到了，但仓库拒绝了这次连接，是仓库侧的权限问题，找仓库管理员。',
    REPO_UNREACHABLE: '网络、地址或超时导致没能到达仓库，分支都还没来得及查。',
    BRANCH_MISSING: '仓库可达、认证通过，但这个分支在仓库里找不到。',
  },
  tone: {
    // NOT_VERIFIED 单独一档，不并进 bad 也不并进 ok：它在界面上必须与 OK
    // 一眼可分，而与「查了没过」也不是一回事——前者要去按一次校验，后者
    // 要去修一个系统。
    NOT_VERIFIED: 'unverified',
    OK: 'ok',
    CREDENTIAL_UNRESOLVED: 'bad',
    AUTH_FAILED: 'bad',
    REPO_UNREACHABLE: 'bad',
    BRANCH_MISSING: 'bad',
  },
}

/**
 * 全部已登记的仓库级取值。
 *
 * 从文案表的键推导而不是另写一份字面量数组：文案表的键类型是
 * `Record<RepoVerifyResult, string>`，`tsc` 已经保证它一个不多一个不少，
 * 再抄一份只会多出一处可以漂移的地方。
 */
export const ALL_REPO_VERIFY_RESULTS = Object.keys(REPO_VERIFY_COPY.label) as RepoVerifyResult[]

/**
 * 把一个仓库的**仓库级**校验结论折算成可渲染的形态。
 *
 * 参数是 GitRepo 而不是一个裸枚举：绑定拿不出 GitRepo，于是「把仓库级
 * 结论渲染到路径级那一格」在只有绑定在手的地方根本编译不过。两边都在
 * 作用域里的地方（集群页那一段要只读展示仓库地址）挡不住，那一处由测试
 * 兜——见 clusterForm 测试里的组件源码断言，以及它对自身局限的说明。
 */
export function describeRepoVerifyStatus(repo: GitRepo): VerifyStatusView {
  return describeVerdict(REPO_VERIFY_COPY, repo.verifyResult, repo.verifiedAt)
}

/** 把一次仓库级校验请求的响应折算成一句回执。 */
export function describeRepoVerifyOutcome(status: RepoVerifyStatus): VerifyOutcomeView {
  return describeOutcome(REPO_VERIFY_COPY, status)
}
