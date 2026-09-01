import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import test from 'node:test'
import assert from 'node:assert/strict'

import {
  ALL_REPO_VERIFY_RESULTS, blankRepoValues, describeRepoVerifyOutcome, describeRepoVerifyStatus,
  repoFormValuesOf, resolveGitRepo,
} from '../src/pages/gitRepoForm.ts'
import { ALL_PATH_VERIFY_RESULTS, describePathVerifyStatus } from '../src/pages/clusterForm.ts'
import type { GitBinding, GitRepo, RepoVerifyResult } from '../src/api/types.ts'

/** dev 库里那一条的真实形态：https:// 地址，从未校验过，凭据未配置。 */
const repo: GitRepo = {
  repoId: 'repo-prod-asia-1',
  repoUrl: 'https://gitlab.example.com/net/policies.git',
  branch: 'main',
  credentialRef: '',
  verifyResult: 'NOT_VERIFIED',
}

/** 造一个只在校验两列上不同的仓库，其余字段与 repo 相同。 */
function verified(result: RepoVerifyResult, at?: string): GitRepo {
  return { ...repo, verifyResult: result, verifiedAt: at }
}

/* ---------------------------------------------------------------------- */
/* 1. 仓库提交体                                                            */
/* ---------------------------------------------------------------------- */

/**
 * 播种只取可写字段：平台自产的两项一旦进了表单，下一个人就会提交它们。
 *
 * privateKey 也在这里，但它**永远是空的**：它读不回来（服务端不回显，
 * GitRepo 类型上没有这个字段），播种成任何非空值都是凭空造的。空在提交时
 * 表示"不动现有凭据"，语义正好对得上。
 */
test('仓库表单只播种可写字段，且私钥恒为空', () => {
  assert.deepEqual(repoFormValuesOf(verified('AUTH_FAILED', '2026-08-13T09:30:00Z')), {
    repoId: 'repo-prod-asia-1',
    repoUrl: 'https://gitlab.example.com/net/policies.git',
    branch: 'main',
    credentialRef: '',
    privateKey: '',
  })
})

/**
 * 提交体里只有四个字段：没有 verifyResult，也没有 verifiedAt。
 *
 * 它们是平台对仓库可信度的判断，由服务端在校验后自己写入。一个能由调用方
 * 提交的结论可以被填成 OK，于是「已校验通过」这句话再也无法被证伪 ——
 * 与 verdict / confidence 必须分列同一条纪律。
 *
 * 断言整个键集合而不是逐个 `hasOwn(...) === false`：逐个断言只挡住已经
 * 想到的那两个拼法。
 */
test('仓库提交体只有四个可写字段', () => {
  const resolved = resolveGitRepo(repoFormValuesOf(verified('OK', '2026-08-13T09:30:00Z')))
  assert.equal(resolved.ok, true)
  if (!resolved.ok) return
  assert.deepEqual(Object.keys(resolved.repo).sort(),
    ['branch', 'credentialRef', 'repoId', 'repoUrl'],
    '仓库提交体多出了字段：平台自产的结论一旦能被客户端设定，就无法被证伪')
  assert.equal(resolved.repo.repoUrl, 'https://gitlab.example.com/net/policies.git')
})

/**
 * 三项必填各自缺席都要被拦住，且报错点名缺的是哪一项。
 *
 * 三个方向都跑：只测一个方向时，一个「只检查 repoId」的实现同样是绿的。
 */
test('repoId / repoUrl / branch 缺任意一项都被拦住，且点名缺的那一项', () => {
  for (const field of ['repoId', 'repoUrl', 'branch'] as const) {
    const values = repoFormValuesOf(repo)
    values[field] = '   '
    const resolved = resolveGitRepo(values)
    assert.equal(resolved.ok, false, `${field} 留空却通过了折算`)
    if (resolved.ok) continue
    assert.match(resolved.error, new RegExp(field), `报错没有点名缺的是 ${field}`)
  }
})

/**
 * credentialRef 可留空。
 *
 * 服务端的 ValidateGitRepo 允许它为空（仓库可以先登记、凭据稍后再配），
 * 前端不得比它更严：一条只在界面上成立的门槛，表现为「后端明明收得下、
 * 界面却不让提交」，而排查的人会去查后端。
 */
test('credentialRef 可留空，不算缺一项', () => {
  const values = blankRepoValues()
  values.repoId = 'repo-prod-eu-1'
  values.repoUrl = 'ssh://git@gitlab.example.com/net/policies.git'
  values.branch = 'main'

  const resolved = resolveGitRepo(values)
  assert.equal(resolved.ok, true)
  if (!resolved.ok) return
  assert.equal(resolved.repo.credentialRef, '')
  assert.match(resolved.summary, /repo-prod-eu-1/)
})

/**
 * 前端不复述 SSH 形态那条规则。
 *
 * 库里那条 https:// 记录必须能走到服务端，由它给出那句点名后果的拒绝理由
 * （「认证方法与传输对不上，一次拨号都不会发生，失败却会被报成仓库不可达」）。
 * 在这里先拦一道的后果不是多一层保护 —— 前端不是安全边界（规范 §34），
 * 它只会让操作者看到一句本地复述的、迟早与服务端漂开的话。
 */
test('https:// 地址由前端放行，交给服务端给出它的拒绝理由', () => {
  const resolved = resolveGitRepo(repoFormValuesOf(repo))
  assert.equal(resolved.ok, true,
    '前端自己复述了 SSH 形态规则：这条规则的准确措辞只有服务端有，复述必然漂开')
})

/* ---------------------------------------------------------------------- */
/* 2. 仓库级校验结论的展示形态                                                */
/* ---------------------------------------------------------------------- */

/**
 * 六个取值必须各有非空文案。
 *
 * ALL_REPO_VERIFY_RESULTS 从 `Record<RepoVerifyResult, string>` 的键推导，
 * 所以后端新增第七个取值时 `tsc` 先报错；这条测试守的是另一半 —— 已登记的
 * 取值不许有人把文案改空。一格空白会被读成「这个仓库没问题」。
 *
 * 数字写死成 6 也是一条断言：PATH_MISSING 不属于这一层，谁把它塞进这张表，
 * 这一行会红（design doc §3.3）。
 */
test('六个仓库级结论各有非空文案，且互不相同', () => {
  assert.equal(ALL_REPO_VERIFY_RESULTS.length, 6)

  const labels = new Set<string>()
  for (const result of ALL_REPO_VERIFY_RESULTS) {
    const view = describeRepoVerifyStatus(verified(result))
    assert.notEqual(view.label.trim(), '', `${result} 的结论文案为空`)
    assert.notEqual(view.detail.trim(), '', `${result} 的说明文案为空`)
    assert.notEqual(view.label, result, `${result} 直接把枚举原样显示了`)
    labels.add(view.label)
  }
  assert.equal(labels.size, 6, '有两个结论共用了同一句文案，界面上分不开')
})

/**
 * 「未校验」与「校验通过」是相反的两件事实，必须一眼可分。
 *
 * 语气分档单独断言而不是只比文案：颜色和描边由 tone 决定，两个不同的
 * 标签配同一种画法，扫一眼表格时仍然分不出来。
 */
test('仓库级 NOT_VERIFIED 与 OK 在文案与语气上都可分，且都不是空白', () => {
  const notVerified = describeRepoVerifyStatus(verified('NOT_VERIFIED'))
  const ok = describeRepoVerifyStatus(verified('OK', '2026-08-12T15:20:15Z'))

  assert.notEqual(notVerified.label.trim(), '')
  assert.notEqual(notVerified.label, ok.label)
  assert.equal(notVerified.tone, 'unverified')
  assert.equal(ok.tone, 'ok')
  assert.notEqual(notVerified.tone, ok.tone)
})

/**
 * 整张文案表里不出现「写」这个字。
 *
 * 与路径级那条同源：只读校验得不出任何与写有关的结论，所以正着说
 * （可以写入）和反着说（不可写入）都不该出现。逐字禁用比逐句 review
 * 可靠 —— 「可写」「能写入」「不可写」是同一个错误的三种拼法。
 *
 * 这一条只管仓库级这一张表；路径级那一张由 clusterForm 的同名测试守。
 * 两层各守各的：一条测试只跑一个枚举，另一层的文案改坏了它不会红。
 */
test('仓库级文案里不出现「写」——只读校验得不出与写有关的结论', () => {
  for (const result of ALL_REPO_VERIFY_RESULTS) {
    const view = describeRepoVerifyStatus(verified(result))
    assert.equal(view.label.includes('写'), false, `${result} 的结论文案谈到了写`)
    assert.equal(view.detail.includes('写'), false, `${result} 的说明文案谈到了写`)
  }
})

/**
 * 平台侧配置错与仓库侧权限错要送不同的人去修不同的系统，文案必须让人
 * 一眼看出该找谁（design doc §3.2）。
 */
test('CREDENTIAL_UNRESOLVED 与 AUTH_FAILED 分别点名平台侧与仓库侧', () => {
  const unresolved = describeRepoVerifyStatus(verified('CREDENTIAL_UNRESOLVED'))
  const authFailed = describeRepoVerifyStatus(verified('AUTH_FAILED'))

  assert.notEqual(unresolved.label, authFailed.label)
  assert.match(unresolved.detail, /平台/)
  assert.match(authFailed.detail, /仓库/)
  assert.equal(unresolved.detail.includes('仓库'), false,
    '说明里同时提到两侧，读者仍然不知道该找谁')
})

/** verifiedAt 必须读成一个过去的时刻，不是当前状态（design doc §3.4）。 */
test('仓库级 verifiedAt 明示为历史时刻，缺失时也不留空', () => {
  const checked = describeRepoVerifyStatus(verified('OK', '2026-08-09T01:02:03Z'))
  assert.match(checked.checkedAt, /2026-08-09 01:02:03 UTC/)
  assert.match(checked.checkedAt, /上次校验/)
  assert.match(checked.checkedAt, /不代表此刻的状态/)

  const never = describeRepoVerifyStatus(verified('NOT_VERIFIED'))
  assert.notEqual(never.checkedAt.trim(), '')
  assert.match(never.checkedAt, /从未校验/)
})

/**
 * 界面还不认识的取值一律按未校验处置，不透出原始码、不留空。
 *
 * 失败方向朝「未确认」关，不朝「可信」开 —— 与后端 RepoVerifyResult.Valid
 * 的收窄同一条纪律。这里也覆盖了「路径级取值被错发到仓库这一层」。
 */
test('仓库级未登记的结论收窄成未校验，不显示成空白或裸枚举', () => {
  const view = describeRepoVerifyStatus(verified('PROBABLY_FINE' as RepoVerifyResult))
  assert.notEqual(view.label.trim(), '')
  assert.notEqual(view.label, 'PROBABLY_FINE')
  assert.equal(view.tone, 'unverified')

  const pathLevel = describeRepoVerifyStatus(verified('PATH_MISSING' as RepoVerifyResult))
  assert.equal(pathLevel.tone, 'unverified')
  assert.equal(pathLevel.label.includes('路径'), false,
    '路径级结论在仓库这一层被照原样念了出来')

  const outcome = describeRepoVerifyOutcome({
    verifyResult: 'PROBABLY_FINE' as RepoVerifyResult, verifiedAt: '2026-08-13T09:30:00Z',
  })
  assert.equal(outcome.tone, 'unverified')
  assert.equal(outcome.message.includes('PROBABLY_FINE'), false)
})

/** 回执与徽章共用同一套文案表：两处对同一个结论说不同的话，读者只能挑一个信。 */
test('仓库级回执文案与列表徽章同源，不各写一套', () => {
  for (const result of ALL_REPO_VERIFY_RESULTS) {
    const badge = describeRepoVerifyStatus(verified(result, '2026-08-13T09:30:00Z'))
    const outcome = describeRepoVerifyOutcome({
      verifyResult: result, verifiedAt: '2026-08-13T09:30:00Z',
    })
    assert.equal(outcome.message.includes(badge.label), true,
      `${result} 的回执没有用列表那一套文案`)
    assert.equal(outcome.tone, badge.tone, `${result} 的回执与徽章语气不一致`)
    assert.equal(outcome.message.includes('写'), false,
      `${result} 的回执谈到了写：只读校验得不出与写有关的结论`)
  }
})

/** 未配置校验器时服务端什么都不落库，回执必须说「这次没有发生校验」。 */
test('无时间戳的仓库级响应读作「这次没有发生校验」', () => {
  const outcome = describeRepoVerifyOutcome({ verifyResult: 'NOT_VERIFIED' })
  assert.equal(outcome.happened, false)
  assert.match(outcome.message, /没有发生校验/)
  assert.match(outcome.message, /库里原有的结论/)
  assert.equal(outcome.tone, 'unverified')

  assert.equal(describeRepoVerifyOutcome({ verifyResult: 'OK', verifiedAt: null }).happened, false,
    '带着 OK 却没有时刻的响应被当成了一次真的校验：那个 OK 没有发生过')
})

/* ---------------------------------------------------------------------- */
/* 3. 两层结论在屏幕上必须分得开                                              */
/* ---------------------------------------------------------------------- */

/**
 * 两层共用 NOT_VERIFIED 与 OK 两个枚举值，但九句文案必须两两不同。
 *
 * 这一条守的是「结论渲染错了层，屏幕上看不出来」这个形状：如果两层的
 * NOT_VERIFIED 都写成「未校验」，那么把仓库级结论摆到 policyPath 那一格上
 * 是完全无法被肉眼发现的，而下一步操作者会去改一个根本没问题的路径。
 *
 * 只测「文案不同」不测「渲染对了」—— 后者组件挂不起来，由两页各自的源码
 * 断言兜。这条测试**不**能证明界面接线正确，只能保证接错时看得出来。
 */
test('两层的文案两两不同，接错层时肉眼看得出来', () => {
  const nothing: GitBinding = {
    repoId: 'repo-prod-asia-1', policyPath: 'clusters/prod-asia-1',
    lastWrittenCommit: '', verifyResult: 'NOT_VERIFIED',
  }
  const repoLabels = ALL_REPO_VERIFY_RESULTS.map((r) => describeRepoVerifyStatus(verified(r)).label)
  const pathLabels = ALL_PATH_VERIFY_RESULTS.map(
    (r) => describePathVerifyStatus({ ...nothing, verifyResult: r }).label,
  )

  for (const label of repoLabels) {
    assert.equal(pathLabels.includes(label), false,
      `「${label}」两层都在用：结论渲染错了层，屏幕上看不出来`)
  }
  assert.equal(new Set([...repoLabels, ...pathLabels]).size, 9,
    '两层加起来应当有九句互不相同的文案')
})

/* ---------------------------------------------------------------------- */
/* 4. 仓库页确实按这些规则接线                                                */
/* ---------------------------------------------------------------------- */

const PAGE_SRC = readFileSync(new URL('../src/pages/GitReposPage.tsx', import.meta.url), 'utf8')

/**
 * 上面所有断言管的都是纯函数的返回值。没有任何一条能证明界面真的按这些
 * 规则接线 —— 这个项目上一次前端缺陷正是这个形状：dry-run 磁贴和它下面
 * 的表格报着两份不同的数字，四道门禁全绿。
 *
 * `node --test` 的类型擦除读不了 JSX，组件没法在这里挂载，所以退而求其次
 * 断言源码。**这是文本级的绑定，不是渲染级的**：它抓得住「这一页调用了
 * 路径级的折算函数」，抓不住「把那次调用挪进一个本页调用的 helper 里」，
 * 也抓不住「调用了但把结果渲染到看不见的地方」。这个局限是真的，
 * `tsc` / lint / build 三道门禁一道都覆盖不到它，不该假装它们能。
 */
test('仓库页只渲染仓库级结论，且五个动作都接上了', () => {
  assert.match(PAGE_SRC, /describeRepoVerifyStatus\(/,
    '这一格没有调用 describeRepoVerifyStatus，上面那一整组文案断言就管不到界面')
  assert.match(PAGE_SRC, /describeRepoVerifyOutcome\(/,
    '没有调用 describeRepoVerifyOutcome：响应与刷新的不一致会被界面当成一个新结论')
  assert.equal(PAGE_SRC.includes('describePathVerify'), false,
    '仓库页渲染了路径级结论：policyPath 属于绑定，一个仓库可被多个集群按不同路径绑定')
  assert.equal(PAGE_SRC.includes('clusterForm'), false,
    '仓库页 import 了路径级的文案模块：两层结论必须各自留在各自的页面上')

  for (const call of [
    'api.gitRepos(', 'api.createGitRepo(', 'api.updateGitRepo(',
    'api.deleteGitRepo(', 'api.verifyGitRepo(',
  ]) {
    assert.equal(PAGE_SRC.includes(call), true, `仓库页没有接上 ${call}`)
  }
  // 仓库页不碰集群，也不碰绑定：绑定是集群页的事，在这里改它等于让一次
  // 仓库清理顺手动了某个集群的策略下发路径。
  for (const call of ['api.bindGitRepo', 'api.unbindGitRepo', 'api.updateCluster']) {
    assert.equal(PAGE_SRC.includes(call), false, `仓库页调用了 ${call}`)
  }
})

/**
 * 删除被拒的理由必须原样展示，不能收窄成一句「删除失败」。
 *
 * 服务端对「仍被绑定」给的是一个业务失败，msg 里写着该先去解除绑定。
 * 收窄掉的正是操作者据以行动的那句话 —— 而级联删除更糟：它会静默解除
 * 某个集群的策略下发路径（design doc §4）。
 */
test('服务端的拒绝理由原样展示，不收窄成通用失败', () => {
  // 每一处 catch 都必须走 ApiError.msg。数一数 catch 的数量，再数一数
  // err.msg 的数量：少一处就说明有一条路径把理由吞掉了。
  const catches = PAGE_SRC.match(/catch \(err\)/g) ?? []
  const surfaced = PAGE_SRC.match(/err instanceof ApiError \? err\.msg/g) ?? []
  assert.equal(catches.length > 0, true, '仓库页没有任何错误处理')
  assert.equal(surfaced.length, catches.length,
    '有 catch 分支没有展示服务端的 msg：被吞掉的正是操作者据以行动的那句话')

  // 删除那条路径尤其要留在屏幕上，不能用一弹就消失的 alert：那句话是
  // 「先去解除某个集群的绑定」，读完还得照着做。
  assert.match(PAGE_SRC, /setActionError\(err instanceof ApiError \? err\.msg/,
    '删除被拒的理由没有留在页面上')
  assert.equal(PAGE_SRC.includes('window.alert'), false,
    '用 alert 展示拒绝理由：一弹就消失的提示，操作者照着做不了')
})

/** 只读校验的禁用词同样管到这一页组件里另写的字符串。 */
test('仓库页里不出现与写有关的结论措辞', () => {
  for (const banned of ['可以写入', '不可写入', '可写入', '不可写']) {
    assert.equal(PAGE_SRC.includes(banned), false,
      `GitReposPage 里出现了「${banned}」：只读校验得不出与写有关的结论`)
  }
})

/* ---------------------------------------------------------------------- */
/* SSH 私钥：只入不出                                                        */
/* ---------------------------------------------------------------------- */

// 一把真的 PEM 私钥长什么样：多行，靠换行分隔。内容不必能解析——
// 这一层不解析它，服务端才解析（前端不是安全边界）。
const PEM = '-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1r\nZXktdjEAAA==\n-----END OPENSSH PRIVATE KEY-----'

// **空私钥不带上请求体。**
//
// 缺省表示"不动现有凭据"，而带一个空串上去是"把凭据清成空"。一个只想改
// 分支的人不该因此把钥匙清掉——那之后写回会报"凭据取不到"，而他改的是分支。
test('空私钥不出现在写入体里', () => {
  const r = resolveGitRepo({
    repoId: 'uat', repoUrl: 'git@h.example.com:i/u.git', branch: 'main',
    credentialRef: '', privateKey: '',
  })
  assert.equal(r.ok, true)
  if (!r.ok) return
  assert.equal('privateKey' in r.repo, false, '空私钥被当成"清空凭据"提交了')
})

// 非空时带上去，且**原样带**。
//
// PEM 的换行是内容的一部分，顺手规范化会把一把好钥匙改成解析不了的那种，
// 而失败要到写回那一刻才显形，报的是"仓库不可达"。
test('私钥原样提交，换行不被整形', () => {
  const r = resolveGitRepo({
    repoId: 'uat', repoUrl: 'git@h.example.com:i/u.git', branch: 'main',
    credentialRef: '', privateKey: `  ${PEM}  `,
  })
  assert.equal(r.ok, true)
  if (!r.ok) return
  assert.equal(r.repo.privateKey, PEM, '私钥内容被改动了')
  assert.equal(r.repo.privateKey?.split('\n').length, 4, '换行被吃掉了')
})

// 提交前那句回执要说出"这一次会存一把新钥匙"。
//
// 换凭据与改地址在界面上长得一样，而前者的影响面大得多。
test('带私钥时回执说明会保存新凭据', () => {
  const withKey = resolveGitRepo({
    repoId: 'uat', repoUrl: 'git@h.example.com:i/u.git', branch: 'main',
    credentialRef: '', privateKey: PEM,
  })
  const without = resolveGitRepo({
    repoId: 'uat', repoUrl: 'git@h.example.com:i/u.git', branch: 'main',
    credentialRef: '', privateKey: '',
  })
  assert.equal(withKey.ok && withKey.summary.includes('新的 SSH 私钥'), true)
  assert.equal(without.ok && without.summary.includes('新的 SSH 私钥'), false)
})

// **编辑既有仓库时私钥永远从空开始。**
//
// 它读不回来（服务端不回显，GitRepo 类型上也没有这个字段），所以播种成
// 任何非空值都是凭空造的。空在提交时表示"不动"，语义正好对得上。
test('编辑表单不播种私钥', () => {
  const seeded = repoFormValuesOf({
    repoId: 'uat', repoUrl: 'git@h.example.com:i/u.git', branch: 'main',
    credentialRef: 'uat', verifyResult: 'OK', verifiedAt: null,
  } as never)
  assert.equal(seeded.privateKey, '', '编辑表单里带出了一个私钥')
})

// 界面上必须写着"公钥装到仓库、私钥粘到这里"，而且要点名 Deploy keys。
//
// 这一步最容易错的是把**错误的东西**粘进来——UAT 上第一次填的就是
// Deploy token。服务端会拒，但拒绝信息说不出该去哪儿拿对的那一份。
test('私钥输入旁边有可照做的引导', () => {
  const src = readFileSync(join(process.cwd(), 'src/pages/GitReposPage.tsx'), 'utf8')
  for (const must of ['ssh-keygen', 'Deploy keys', '.pub', '写权限']) {
    assert.ok(src.includes(must), `引导里没有提到 ${must}`)
  }
  assert.ok(
    src.includes('不是 Deploy tokens'),
    '没有点明 Deploy keys 与 Deploy tokens 的区别 —— 这正是第一次填错的地方',
  )
})
