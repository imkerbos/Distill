import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import test from 'node:test'
import assert from 'node:assert/strict'

import {
  ALL_PATH_VERIFY_RESULTS, blankFormValues, blankGitValues, buildClusterWrite,
  describePathVerifyOutcome, describePathVerifyStatus, formValuesOf, gitFormValuesOf,
  resolveGitBinding, parseScraperLines, scraperToLine,
} from '../src/pages/clusterForm.ts'
import { formatUtcTime } from '../src/pages/verifyView.ts'
import type { GitBinding, PathVerifyResult, RegisteredCluster } from '../src/api/types.ts'

const binding: GitBinding = {
  repoId: 'repo-prod-asia-1',
  policyPath: 'clusters/prod-asia-1',
  lastWrittenCommit: '0123456789abcdef0123456789abcdef01234567',
  verifyResult: 'NOT_VERIFIED',
}

/** 造一个只在校验两列上不同的绑定，其余字段与 binding 相同。 */
function verified(result: PathVerifyResult, at?: string): GitBinding {
  return { ...binding, verifyResult: result, verifiedAt: at }
}

function bound(): RegisteredCluster {
  return {
    id: 'prod-asia-1', displayName: '亚太生产', podCidr: '10.4.0.0/14',
    // 真值而非 false：一个恒为 false 的夹具让「这一项被原样提交」与
    // 「这一项被清成 false」两种实现给出同一个结果，断言就什么也没证明。
    nodeCidr: '10.128.0.0/20', ccnpPresent: true, state: 'READY',
    apiServers: [{ host: '10.9.0.2', cidr: '10.9.0.0/28', port: 443 }],
    healthCheckSources: ['35.191.0.0/16', '130.211.0.0/22'],
    git: binding,
  }
}

function unbound(): RegisteredCluster {
  return {
    id: 'prod-eu-1', displayName: '欧洲生产', podCidr: '10.8.0.0/14',
    nodeCidr: '10.132.0.0/20', ccnpPresent: false, state: 'READY',
    apiServers: [{ host: '10.10.0.2', cidr: '10.10.0.0/28', port: 443 }],
    healthCheckSources: ['35.191.0.0/16'],
  }
}

const CLUSTER_WRITE_KEYS = [
  // metricsScrapers 2026-08-18 加入：它与 healthCheckSources 同类 —— 一份
  // 观测不出来、只能由人登记的 Baseline 依据。
  'apiServers', 'ccnpPresent', 'displayName', 'healthCheckSources', 'id', 'kubeconfigRef',
  'metricsScrapers', 'nodeCidr', 'podCidr',
]

/* ---------------------------------------------------------------------- */
/* 1. 两个资源，两份提交体                                                    */
/* ---------------------------------------------------------------------- */

/**
 * 集群提交体里没有 git —— 一个键都没有。
 *
 * 服务端的 clusterPayload 已经不接受 git（internal/httpapi/cluster_handler.go）。
 * 在这里带上它的后果不是多一个被忽略的字段，而是：操作者在集群表单里改了
 * 仓库地址、请求返回成功、界面显示保存生效，而库里的绑定原封不动 —— 直到
 * 某天有人发现这个集群的策略一直下发到旧仓库去了。
 *
 * 断言整个键集合而不是 `hasOwn(body, 'git') === false`：后者只挡住 `git`
 * 这一个拼法，`gitBinding` / `repo` 换个名字照样溜进去，而服务端同样会
 * 静默丢弃它们。
 */
test('集群提交体里没有 git', () => {
  const cluster = bound()
  const built = buildClusterWrite(formValuesOf(cluster))
  assert.equal(built.ok, true)
  if (!built.ok) return

  assert.deepEqual(Object.keys(built.body).sort(), CLUSTER_WRITE_KEYS,
    '集群提交体的字段集合变了：绑定必须走它自己的端点，集群写路径不碰它')
})

/**
 * 解绑不经由集群提交体表达。
 *
 * 上一轮「把四个字段清空」与「我要解绑」在整体替换下提交结果相同，所以
 * 需要一个勾选框来消歧义。现在解绑是 DELETE /clusters/{id}/git-binding，
 * 成因消失，那个勾选框也就必须消失 —— 留着它等于留着一条能从集群写路径
 * 影响绑定的表达方式，而那条路径服务端根本不看。
 *
 * 这条从三个方向堵：表单值里没有 clearGit、集群提交体不因绑定而变、
 * 绑定折算函数永远给不出一个「空绑定」。
 */
test('解绑不经由集群提交体表达', () => {
  const values = formValuesOf(bound())
  assert.equal(Object.hasOwn(values, 'clearGit'), false,
    '集群表单里还留着解绑开关：解绑已经是 DELETE，它不该有任何集群侧的表达')
  assert.equal(Object.hasOwn(values, 'git'), false,
    '集群表单里还带着绑定字段：两个资源混在一份表单状态里，一次保存会写两处')

  // 已绑定与未绑定两个集群，集群提交体的形状必须完全一样：绑定的有无
  // 不该在集群写路径上留下任何痕迹。
  const fromBound = buildClusterWrite(formValuesOf(bound()))
  const fromUnbound = buildClusterWrite(formValuesOf(unbound()))
  assert.equal(fromBound.ok && fromUnbound.ok, true)
  if (!fromBound.ok || !fromUnbound.ok) return
  assert.deepEqual(Object.keys(fromBound.body).sort(), Object.keys(fromUnbound.body).sort())

  // 绑定表单交不出「空绑定」：清空输入框得到的是一句拒绝，不是一次解绑。
  const cleared = resolveGitBinding(blankGitValues())
  assert.equal(cleared.ok, false)
  if (cleared.ok) return
  assert.match(cleared.error, /解除绑定/, '拒绝时没有告诉操作者解绑该走哪里')
})

/* ---------------------------------------------------------------------- */
/* 2. 集群提交体：整体替换下每一项都得原样带上                                 */
/* ---------------------------------------------------------------------- */

/**
 * PUT 整体替换，表单里没有的字段提交后就是库里没有的字段。一个只想改
 * 显示名的操作者，绝不该因此丢掉 apiserver 清单或健康检查网段 —— 少掉的
 * 是 baseline 里的一条放行规则，事后表现为生产阻断，而不是提交时报错。
 */
test('编辑只改显示名时，未触碰的字段原样提交', () => {
  const cluster = bound()
  const values = formValuesOf(cluster)
  values.displayName = '亚太生产（新）'

  const built = buildClusterWrite(values)
  assert.equal(built.ok, true)
  if (!built.ok) return

  assert.equal(built.body.podCidr, '10.4.0.0/14')
  assert.equal(built.body.nodeCidr, '10.128.0.0/20')
  assert.equal(built.body.displayName, '亚太生产（新）')
  assert.deepEqual(built.body.apiServers, [{ host: '10.9.0.2', cidr: '10.9.0.0/28', port: 443 }])
  assert.deepEqual(built.body.healthCheckSources, ['35.191.0.0/16', '130.211.0.0/22'])
  // 这一项与上面几项同等要紧，方向却更危险：网段被清空会让判定用错的
  // 网段回答，而它被清成 false 会让一个本该整体降级为 DEGRADED 的集群
  // 给出笃定的判定——平台因此显得比它应该的样子更有把握。
  assert.equal(built.body.ccnpPresent, true,
    'ccnpPresent 没有被原样提交：一次与它无关的编辑会把该集群的判定降级悄悄关掉')
})

/**
 * kubeconfigRef 必须被播种并原样提交。
 *
 * 与 ccnpPresent 同一条纪律，后果又不同：PUT 是整体替换，漏播它的结果
 * 不是"改不了凭据"，而是**一个只想改显示名的操作者顺手清空了凭据引用**，
 * 于是采集器此后再也连不上这个集群 —— 而这件事要到下一次采集才暴露，
 * 表现成"这个集群没有采集记录"。
 *
 * 后端那一层已经有对应的用例（TestUpdateClusterWritesTheKubeconfigReference）；
 * 表单这一层单独钉，是因为界面才是操作者真正走的那条路径。
 */
test('kubeconfigRef 被播种并原样提交，不因一次无关编辑而清空', () => {
  const cluster = bound()
  cluster.kubeconfigRef = 'prod-asia-1-kubeconfig'

  const values = formValuesOf(cluster)
  assert.equal(values.kubeconfigRef, 'prod-asia-1-kubeconfig',
    '编辑表单没有播种凭据引用 —— 打开表单那一刻它就已经被清空了')

  values.displayName = '亚太生产（新）'
  const built = buildClusterWrite(values)
  assert.equal(built.ok, true)
  if (!built.ok) return
  assert.equal(built.body.kubeconfigRef, 'prod-asia-1-kubeconfig',
    '一次与凭据无关的编辑清空了 kubeconfigRef —— 采集器此后连不上这个集群')
})

/**
 * 反面：没有凭据引用的集群提交出来是空串，不是别的什么。
 *
 * 少了这条，一个把 kubeconfigRef 写死成某个常量的实现同样能让上一条通过。
 */
test('没有登记凭据的集群提交空串', () => {
  const cluster = bound()
  delete (cluster as { kubeconfigRef?: string }).kubeconfigRef

  const built = buildClusterWrite(formValuesOf(cluster))
  assert.equal(built.ok, true)
  if (!built.ok) return
  assert.equal(built.body.kubeconfigRef, '')
})

/** 表单上必须真的有这个输入框，否则接口通了也没人填得进去。 */
test('集群表单上有 kubeconfigRef 输入框', () => {
  const page = readFileSync(new URL('../src/pages/ClustersPage.tsx', import.meta.url), 'utf8')
  assert.match(page, /kubeconfigRef/,
    'ClustersPage 上没有凭据引用的输入框 —— 接口能收，界面上却填不了')
})

/**
 * 上一条只能证明 true 活了下来。单靠它，一个把 ccnpPresent 写死成 true
 * 的实现同样是绿的——而那会让所有集群永久降级，方向相反、一样是错的。
 * 这一条走另外两个方向：现值为 false 时提交 false，操作者勾上时提交 true。
 * 两条合起来才说明这一项确实是"跟着表单走"，而不是被某个常量顶替。
 */
test('ccnpPresent 双向跟随：现值为假就提交假，勾上就提交真', () => {
  const cluster = bound()
  cluster.ccnpPresent = false
  const values = formValuesOf(cluster)
  assert.equal(values.ccnpPresent, false, '播种时没有读取集群现值')

  const asIs = buildClusterWrite(values)
  assert.equal(asIs.ok, true)
  if (!asIs.ok) return
  assert.equal(asIs.body.ccnpPresent, false)

  // 操作者在集群里装上了 Cilium：这一项必须改得动，否则它就成了一个
  // 只能看不能改的事实，而"集群里装上 CCNP"是随时会发生的事。
  values.ccnpPresent = true
  const toggled = buildClusterWrite(values)
  assert.equal(toggled.ok, true)
  if (!toggled.ok) return
  assert.equal(toggled.body.ccnpPresent, true)
})

/** 注册表单从"没有 CCNP"起步，且这一项确实出现在提交体里。 */
test('注册表单默认不声明 CCNP，但字段必须出现在提交体里', () => {
  const values = blankFormValues()
  values.id = 'prod-us-1'
  values.displayName = '美西生产'
  values.podCidr = '10.16.0.0/14'
  values.nodeCidr = '10.140.0.0/20'

  const built = buildClusterWrite(values)
  assert.equal(built.ok, true)
  if (!built.ok) return
  assert.equal(Object.hasOwn(built.body, 'ccnpPresent'), true,
    '字段缺席时服务端按 false 落库，等于替操作者做了一个他没做过的声明')
  assert.equal(built.body.ccnpPresent, false)
})

/**
 * apiserver 行号用界面上看到的序号（从 1 开始、过滤前的下标）：一旦
 * 前面有整行空白被跳过，过滤后的下标就和操作者盯着的表单对不上。
 */
test('apiserver 半填的行按界面序号报错', () => {
  const values = blankFormValues()
  values.apiServerRows = [
    { host: '', cidr: '', port: '' },
    { host: '', cidr: '10.9.0.0/28', port: '443' },
  ]

  const built = buildClusterWrite(values)
  assert.equal(built.ok, false)
  if (built.ok) return
  assert.match(built.error, /第 2 行/)
  assert.match(built.error, /host/)
})

/* ---------------------------------------------------------------------- */
/* 3. 绑定提交体                                                            */
/* ---------------------------------------------------------------------- */

/** 播种只取两个可写字段：平台自产的三项一旦进了表单，下一个人就会提交它们。 */
test('绑定表单只播种两个可写字段', () => {
  assert.deepEqual(gitFormValuesOf(binding), {
    repoId: 'repo-prod-asia-1',
    policyPath: 'clusters/prod-asia-1',
  })
  // 未绑定集群（prod-eu-1 的真实形态）给一份空值，而不是抛错或留 undefined。
  assert.deepEqual(gitFormValuesOf(unbound().git), blankGitValues())
})

/**
 * 提交体里只有两个字段：没有 lastWrittenCommit、没有校验结论，也**没有
 * 仓库的地址、分支与凭据**。
 *
 * 前三者是平台自产的东西，但凡有一个能由客户端提供，对应的那句话就再也
 * 无法被证伪。后三者是另一回事：它们属于仓库，服务端的 gitBindingPayload
 * 根本不收它们（internal/httpapi/gitbinding_handler.go）—— 带上去请求照样
 * 成功，界面显示保存生效，而平台真正会去连的地址原封不动。
 *
 * 断言整个键集合而不是逐个 `hasOwn(...) === false`：逐个断言只挡住已经
 * 想到的那几个拼法，`repo` / `gitRepo` 换个名字照样溜进去。
 */
test('绑定提交体只有 repoId 与 policyPath', () => {
  const resolved = resolveGitBinding(gitFormValuesOf(binding))
  assert.equal(resolved.ok, true)
  if (!resolved.ok) return
  assert.deepEqual(Object.keys(resolved.binding).sort(), ['policyPath', 'repoId'],
    '绑定提交体多出了字段：仓库属性走仓库端点，平台自产的结论与基准根本不该由客户端给')
  assert.equal(resolved.binding.repoId, 'repo-prod-asia-1')
  assert.equal(resolved.binding.policyPath, 'clusters/prod-asia-1')
})

/**
 * 两项各自缺席都要被拦住，且报错点名缺的是哪一项。
 *
 * 双向跑：只填 repoId 与只填 policyPath 都是「填了一半的绑定」。只测一个
 * 方向，一个「只检查 repoId」的实现同样是绿的 —— 而那会让一次少填路径的
 * 保存被服务端按空 policyPath 落库，此后策略写去仓库根目录。
 */
test('repoId 与 policyPath 缺任意一项都被拦住，且点名缺的那一项', () => {
  const onlyRepo = blankGitValues()
  onlyRepo.repoId = 'repo-prod-asia-1'
  const a = resolveGitBinding(onlyRepo)
  assert.equal(a.ok, false)
  if (a.ok) return
  assert.match(a.error, /policyPath/)

  const onlyPath = blankGitValues()
  onlyPath.policyPath = 'clusters/prod-eu-1'
  const b = resolveGitBinding(onlyPath)
  assert.equal(b.ok, false)
  if (b.ok) return
  assert.match(b.error, /repoId/)
})

/** 未绑定集群补上绑定：这是本轮的动机场景，prod-eu-1 的真实形态。 */
test('未绑定集群可以绑到一个已登记的仓库', () => {
  const values = gitFormValuesOf(unbound().git)
  values.repoId = 'repo-prod-asia-1'
  values.policyPath = 'clusters/prod-eu-1'

  const resolved = resolveGitBinding(values)
  assert.equal(resolved.ok, true)
  if (!resolved.ok) return
  assert.equal(resolved.binding.repoId, 'repo-prod-asia-1')
  assert.equal(resolved.binding.policyPath, 'clusters/prod-eu-1')
  assert.match(resolved.summary, /clusters\/prod-eu-1/)
  assert.match(resolved.summary, /repo-prod-asia-1/)
})

/* ---------------------------------------------------------------------- */
/* 4. 一次校验请求的回执                                                     */
/* ---------------------------------------------------------------------- */

/**
 * 未配置校验器时，服务端返回 NOT_VERIFIED + 无时间戳，并且**刻意什么都
 * 不落库**（internal/httpapi/gitverify_handler.go）。于是响应会与刷新后
 * 看到的东西不一致：响应说「从未校验」，库里那行还留着更早的结论。
 *
 * 界面必须把这一次读成「什么都没发生」，而不是一个崭新的 NOT_VERIFIED ——
 * 后者会让操作者以为自己刚把结论刷掉了，而下一次刷新它又变回旧结论。
 * 一个自己会变回去的界面，比一个说「什么都没发生」的界面更难被信任。
 */
test('无时间戳的响应读作「这次没有发生校验」，不当成新结论', () => {
  const outcome = describePathVerifyOutcome({ verifyResult: 'NOT_VERIFIED' })
  assert.equal(outcome.happened, false)
  assert.notEqual(outcome.message.trim(), '', '什么都不说，操作者只能猜是按钮坏了')
  assert.match(outcome.message, /没有发生校验/)
  // 必须点明列表里那一格显示的是库里原有的结论，否则「响应与刷新不一致」
  // 这件事仍然要靠操作者自己想明白。
  assert.match(outcome.message, /库里原有的结论/)
  assert.equal(outcome.tone, 'unverified')

  // verifiedAt 显式为 null（后端 omitempty 之外的另一种可能）同样处理。
  assert.equal(describePathVerifyOutcome({ verifyResult: 'OK', verifiedAt: null }).happened, false,
    '带着 OK 却没有时刻的响应被当成了一次真的校验：那个 OK 没有发生过')
})

/** 真的发生了校验时，回执点名结论与时刻，且读得出是刚刚那一次。 */
test('带时间戳的响应读作一次真实发生的校验', () => {
  const outcome = describePathVerifyOutcome({
    verifyResult: 'PATH_MISSING', verifiedAt: '2026-08-13T09:30:00Z',
  })
  assert.equal(outcome.happened, true)
  assert.match(outcome.message, /2026-08-13 09:30:00 UTC/)
  assert.match(outcome.message, /路径不存在/)
  assert.equal(outcome.tone, 'bad')

  const ok = describePathVerifyOutcome({ verifyResult: 'OK', verifiedAt: '2026-08-13T09:30:00Z' })
  assert.equal(ok.tone, 'ok')
  assert.notEqual(ok.message, outcome.message)
})

/** 回执与徽章共用同一套文案表：两处对同一个结论说不同的话，读者只能挑一个信。 */
test('回执文案与列表徽章同源，不各写一套', () => {
  for (const result of ALL_PATH_VERIFY_RESULTS) {
    const badge = describePathVerifyStatus(verified(result, '2026-08-13T09:30:00Z'))
    const outcome = describePathVerifyOutcome({
      verifyResult: result, verifiedAt: '2026-08-13T09:30:00Z',
    })
    assert.equal(outcome.message.includes(badge.label), true,
      `${result} 的回执没有用列表那一套文案`)
    assert.equal(outcome.tone, badge.tone, `${result} 的回执与徽章语气不一致`)
    assert.equal(outcome.message.includes('写'), false,
      `${result} 的回执谈到了写：只读校验得不出与写有关的结论`)
  }
})

/* ---------------------------------------------------------------------- */
/* 5. 路径级校验结论的展示形态                                                */
/* ---------------------------------------------------------------------- */

/**
 * 三个取值必须各有非空文案。
 *
 * ALL_PATH_VERIFY_RESULTS 从 `Record<PathVerifyResult, string>` 的键推导，
 * 所以后端在这一层新增第四个取值时 `tsc` 先报错；这条测试守的是另一半 ——
 * 已登记的取值不许有人把文案改空。一格空白在这张表里会被读成「这条路径
 * 没问题」。
 *
 * 数字写死成 3 也是一条断言：仓库级那四个失败取值不属于这一层，谁把
 * AUTH_FAILED 之类塞进这张表，这一行会红（design doc §3.3）。
 */
test('三个路径级结论各有非空文案，且互不相同', () => {
  assert.equal(ALL_PATH_VERIFY_RESULTS.length, 3)

  const labels = new Set<string>()
  for (const result of ALL_PATH_VERIFY_RESULTS) {
    const view = describePathVerifyStatus(verified(result))
    assert.notEqual(view.label.trim(), '', `${result} 的结论文案为空`)
    assert.notEqual(view.detail.trim(), '', `${result} 的说明文案为空`)
    assert.notEqual(view.label, result, `${result} 直接把枚举原样显示了`)
    labels.add(view.label)
  }
  assert.equal(labels.size, 3, '有两个结论共用了同一句文案，界面上分不开')
})

/**
 * 「未校验」与「校验通过」是相反的两件事实，必须一眼可分。
 *
 * 语气分档单独断言而不是只比文案：颜色和描边由 tone 决定，两个不同的
 * 标签配同一种画法，扫一眼表格时仍然分不出来。
 */
test('NOT_VERIFIED 与 OK 在文案与语气上都可分，且都不是空白', () => {
  const notVerified = describePathVerifyStatus(verified('NOT_VERIFIED'))
  const ok = describePathVerifyStatus(verified('OK', '2026-08-12T15:20:15Z'))

  assert.notEqual(notVerified.label.trim(), '')
  assert.notEqual(notVerified.label, ok.label)
  assert.equal(notVerified.tone, 'unverified')
  assert.equal(ok.tone, 'ok')
  assert.notEqual(notVerified.tone, ok.tone)
})

/**
 * PATH_MISSING 说的是「路径不存在」。
 *
 * 校验只做只读查询，它查的是路径在不在，从来没试过往那里放东西，所以
 * 它给不出「不可写入」这个结论（design doc §3.1）。
 */
test('PATH_MISSING 的文案说路径不存在', () => {
  const view = describePathVerifyStatus(verified('PATH_MISSING'))
  assert.equal(view.label, '路径不存在')
})

/**
 * 整张文案表里不出现「写」这个字。
 *
 * 这是把 design doc §3.1 那条约束做成一个能被测试抓住的形状：只读校验
 * 得不出任何与写有关的结论，所以正着说（可以写入）和反着说（不可写入）
 * 都不该出现。逐字禁用比逐句 review 可靠 —— 「可写」「能写入」「不可
 * 写」是同一个错误的三种拼法，禁字把三种一起挡住。
 *
 * 覆盖面要说清楚：这条只管路径级这一张表。仓库级那一张由
 * gitRepoForm 的同名测试守，两层各守各的 —— 一条测试只跑一个枚举，
 * 另一层的文案改坏了它不会红。
 */
test('路径级文案里不出现「写」——只读校验得不出与写有关的结论', () => {
  for (const result of ALL_PATH_VERIFY_RESULTS) {
    const view = describePathVerifyStatus(verified(result))
    assert.equal(view.label.includes('写'), false, `${result} 的结论文案谈到了写`)
    assert.equal(view.detail.includes('写'), false, `${result} 的说明文案谈到了写`)
  }
})

/**
 * 路径级的 NOT_VERIFIED 必须把「仓库那一层没通过」也说到。
 *
 * 路径级以仓库级为前提：仓库都没到达过时，这一层只会是 NOT_VERIFIED
 * （design doc §3.3）。文案若只说「从未校验过」，操作者会以为是自己忘了
 * 点按钮，然后反复点一个永远不会变的按钮 —— 该修的东西在仓库页。
 */
test('路径级 NOT_VERIFIED 说明里点出「仓库那一层」这条前提', () => {
  const view = describePathVerifyStatus(verified('NOT_VERIFIED'))
  assert.match(view.detail, /仓库/)
  assert.match(view.detail, /前提/)
})

/**
 * verifiedAt 必须读成一个过去的时刻，不是当前状态。
 *
 * 一个孤零零的时间戳挨着「只读校验通过」，读起来就是「现在是通过的」。
 * 轮 4 写回前必须重新校验，拿几天前的结论当此刻的状态正是 design doc
 * §3.4 禁止的那件事。
 */
test('verifiedAt 明示为历史时刻，缺失时也不留空', () => {
  const checked = describePathVerifyStatus(verified('OK', '2026-08-09T01:02:03Z'))
  assert.match(checked.checkedAt, /2026-08-09 01:02:03 UTC/)
  assert.match(checked.checkedAt, /上次校验/)
  assert.match(checked.checkedAt, /不代表此刻的状态/)

  const never = describePathVerifyStatus(verified('NOT_VERIFIED'))
  assert.notEqual(never.checkedAt.trim(), '')
  assert.match(never.checkedAt, /从未校验/)
})

/**
 * 界面还不认识的取值一律按未校验处置，不透出原始码、不留空。
 *
 * 失败方向朝「未确认」关，不朝「可信」开 —— 与后端 BindingVerifyResult.Valid
 * 的收窄同一条纪律。`as PathVerifyResult` 是刻意的：这里模拟的正是运行时
 * 收到一个类型系统没预料到的值，**包括仓库级那四个取值被错发到这一层**。
 */
test('未登记的结论收窄成未校验，不显示成空白或裸枚举', () => {
  const view = describePathVerifyStatus(verified('PROBABLY_FINE' as PathVerifyResult))
  const ok = describePathVerifyStatus(verified('OK'))

  assert.notEqual(view.label.trim(), '')
  assert.notEqual(view.label, 'PROBABLY_FINE')
  assert.notEqual(view.label, ok.label)
  assert.equal(view.tone, 'unverified')

  // 仓库级的失败取值落到这一层同样收窄，不会借着「两层共用一套文案」
  // 混过去 —— 一个显示在 policyPath 旁边的「认证被拒绝」会把人送去改
  // 一个根本没问题的路径。
  const repoLevel = describePathVerifyStatus(verified('AUTH_FAILED' as PathVerifyResult))
  assert.equal(repoLevel.tone, 'unverified')
  assert.equal(repoLevel.label.includes('认证'), false,
    '仓库级结论在路径级这一层被照原样念了出来')

  // 回执走同一条收窄：一个界面还不认识的结论不是通过了的结论。
  const outcome = describePathVerifyOutcome({
    verifyResult: 'PROBABLY_FINE' as PathVerifyResult, verifiedAt: '2026-08-13T09:30:00Z',
  })
  assert.equal(outcome.tone, 'unverified')
  assert.equal(outcome.message.includes('PROBABLY_FINE'), false)
})

test('时间戳解析不出来时原样返回，不让一张表白屏', () => {
  assert.equal(formatUtcTime('not-a-time'), 'not-a-time')
  assert.equal(formatUtcTime('2026-08-12T15:20:15.123456Z'), '2026-08-12 15:20:15 UTC')
})

/* ---------------------------------------------------------------------- */
/* 6. 组件确实按这些规则接线                                                  */
/* ---------------------------------------------------------------------- */

const PAGE_SRC = readFileSync(new URL('../src/pages/ClustersPage.tsx', import.meta.url), 'utf8')

/**
 * 从 ClustersPage.tsx 里截出一个顶层组件的源码。
 *
 * 顶层函数的收尾大括号是这个文件里唯一顶格出现的 `}`（组件内部的一切
 * 都有缩进），所以从 `function X(` 截到下一个 `\n}\n` 就是它的函数体。
 * 这个办法很土，但它换来的是一条"这个组件调用了谁"的断言 —— 见下面
 * 那条测试对自身局限的说明。
 */
function componentSource(name: string): string {
  const start = PAGE_SRC.indexOf(`function ${name}(`)
  assert.notEqual(start, -1, `ClustersPage.tsx 里找不到组件 ${name}`)
  const end = PAGE_SRC.indexOf('\n}\n', start)
  assert.notEqual(end, -1, `${name} 的函数体没有正常收尾，截取失败`)
  return PAGE_SRC.slice(start, end)
}

/**
 * 上面所有断言管的都是纯函数的返回值。没有任何一条能证明界面真的按这些
 * 规则接线 —— 这个项目上一次前端缺陷正是这个形状：dry-run 磁贴和它下面
 * 的表格报着两份不同的数字，四道门禁全绿。
 *
 * `node --test` 的类型擦除读不了 JSX，组件没法在这里挂载，所以退而求其次
 * 断言源码。**这是文本级的绑定，不是渲染级的**：它抓得住「有人在绑定表单
 * 里顺手发一次集群 PUT」，抓不住「把这次调用挪进一个被绑定表单调用的
 * helper 里」，也抓不住「调用了但把结果渲染到看不见的地方」。这个局限是
 * 真的，`tsc` / lint / build 三道门禁一道都覆盖不到它，不该假装它们能。
 */
test('绑定表单只打绑定端点，不顺手重写集群、也不顺手改仓库', () => {
  const src = componentSource('GitBindingForm')

  assert.match(src, /api\.bindGitRepo\(/, '绑定表单没有调用绑定端点')
  assert.match(src, /api\.unbindGitRepo\(/, '解绑没有走 DELETE 端点')
  // 顺手补一次集群写入不会报错、类型也对 —— 它的后果是把集群表单里没有
  // 播种过的一份状态写进库，比如把 ccnpPresent 清成 false。
  assert.equal(src.includes('api.updateCluster'), false,
    '绑定表单顺带发了一次集群 PUT：改绑定不该重写集群，被重写的字段没有一个是这张表单播种过的')
  assert.equal(src.includes('api.createCluster'), false, '绑定表单调用了集群创建端点')
  assert.equal(src.includes('buildClusterWrite'), false, '绑定表单折算了一份集群提交体')
  // 同一条纪律的另一半：仓库是一个可能还被别的集群绑着的共享资源，从这张
  // 表单改它，操作者以为自己只动了这一个集群（design doc §3.2、§5）。
  for (const call of ['api.updateGitRepo', 'api.createGitRepo', 'api.deleteGitRepo']) {
    assert.equal(src.includes(call), false,
      `绑定表单调用了 ${call}：仓库属性只在仓库页改，这里是只读展示`)
  }
  assert.equal(src.includes('resolveGitRepo'), false, '绑定表单折算了一份仓库提交体')
})

/** 反向：集群表单只打集群端点，不顺手改绑定、也不顺手改仓库。 */
test('集群表单只打集群端点，不顺手改绑定或仓库', () => {
  for (const name of ['EditClusterForm', 'RegisterSection']) {
    const src = componentSource(name)
    assert.match(src, /buildClusterWrite\(/, `${name} 没有走集群提交体的折算`)
    assert.equal(src.includes('api.bindGitRepo'), false, `${name} 顺带写了一次绑定`)
    assert.equal(src.includes('api.unbindGitRepo'), false, `${name} 顺带解了一次绑`)
    assert.equal(src.includes('resolveGitBinding'), false, `${name} 折算了一份绑定提交体`)
    assert.equal(src.includes('api.updateGitRepo'), false, `${name} 顺带改了一次仓库`)
  }
})

/** 那个勾选框及其消歧义逻辑必须整个消失，不是留着不用。 */
test('「解除 Git 绑定」勾选框已删除，DELETE 是唯一解绑路径', () => {
  assert.equal(PAGE_SRC.includes('clearGit'), false,
    '解绑开关的状态还在：它的成因（整体替换下清空与解绑不可分）已经消失')
  assert.equal(PAGE_SRC.includes('解除 Git 绑定（'), false, '解绑勾选框还在界面上')
})

/**
 * 整个集群页只渲染路径级结论，一处仓库级都没有。
 *
 * 这一条守的是本轮最容易出的那个错：仓库对象在这一页是在作用域里的
 * （绑定那一格要只读展示它的地址与分支），所以把 describeRepoVerifyStatus
 * 换到那一格上**类型是通的**，`tsc` / lint / build 三道门禁一道都不会红。
 * 后果是一个「认证被拒绝」显示在 policyPath 旁边，读的人去改一个根本没
 * 问题的路径（design doc §3.3）。
 *
 * **这是文本级的绑定，不是渲染级的**：它抓得住「这一页 import 了仓库级
 * 的折算函数」，抓不住「把那次调用挪进一个本页调用的 helper 里」。这个
 * 局限是真的，不该假装编译器覆盖得到它。
 */
test('集群页不渲染任何仓库级结论', () => {
  assert.equal(PAGE_SRC.includes('describeRepoVerify'), false,
    '集群页调用了仓库级的折算函数：仓库级结论摆在 policyPath 旁边会被读成关于路径的判断')
  assert.equal(PAGE_SRC.includes('gitRepoForm'), false,
    '集群页 import 了仓库级的文案模块：两层结论必须各自留在各自的页面上')
})

/** 列表与回执确实经由这一套纯函数渲染，且 ccnpPresent 两侧都没被顺手删掉。 */
test('集群列表与校验回执确实经由这些纯函数渲染', () => {
  assert.match(PAGE_SRC, /describePathVerifyStatus\(/,
    '这一格没有调用 describePathVerifyStatus，上面那一整组文案断言就管不到界面')
  assert.match(PAGE_SRC, /describePathVerifyOutcome\(/,
    '没有调用 describePathVerifyOutcome：响应与刷新的不一致会被界面当成一个新结论')
  assert.match(PAGE_SRC, /<GitBindingCell\b/)
  assert.match(PAGE_SRC, /<GitBindingForm\b/, '绑定表单没有被挂上去，界面上根本改不了绑定')
  // 绑定只存一个 repoId，仓库地址要从仓库清单里查出来只读展示。不拉这份
  // 清单，界面就只剩一个光秃秃的 ID，操作者无从确认自己绑的是哪个仓库。
  assert.match(PAGE_SRC, /api\.gitRepos\(/,
    '集群页没有拉仓库清单：绑定那一格显示不出仓库地址，选仓库的下拉也是空的')
  assert.match(PAGE_SRC, /repo\.repoUrl/,
    '仓库地址没有被只读展示：只显示 repoId 时，选错仓库的后果是策略下发到别处')
  // 表单与列表都必须碰到 ccnpPresent：看不见的降级理由，操作者既解释不了
  // 眼前的判定，也察觉不到一次编辑把它清掉了。
  assert.match(PAGE_SRC, /values\.ccnpPresent/,
    '编辑/注册表单没有这一项：整体替换会把它清成 false')
  assert.match(PAGE_SRC, /c\.ccnpPresent/,
    '列表里看不到这一项：一个看不见的字段被清掉时没有人会发现')
  for (const banned of ['可以写入', '不可写入', '可写入', '不可写']) {
    assert.equal(PAGE_SRC.includes(banned), false,
      `ClustersPage 里出现了「${banned}」：只读校验得不出与写有关的结论`)
  }
})

test('抓取端一行解析成 namespace 与标签', () => {
  const got = parseScraperLines('monitoring  app.kubernetes.io/name=prometheus,release=kps')
  assert.deepEqual(got, [{
    namespace: 'monitoring',
    labels: { 'app.kubernetes.io/name': 'prometheus', release: 'kps' },
  }])
})

test('解析不出标签的行整行丢弃，不做局部挽救', () => {
  // 一个只解析出 namespace、标签为空的抓取端会被服务端拒（空 podSelector
  // 放行整个命名空间），而在这里半途挽救只会让那次拒绝的成因离输入更远。
  for (const line of ['monitoring', 'monitoring  ', 'monitoring  =prometheus', 'monitoring  app=']) {
    assert.deepEqual(parseScraperLines(line), [], `line ${JSON.stringify(line)} should be dropped`)
  }
})

test('抓取端往返：渲染回去的一行要能再解析回来', () => {
  // 编辑表单是拿现值预填的。渲染与解析对不上，改一次别的字段就会顺手把
  // 抓取端登记改坏，而页面上看不出来。
  const original = { namespace: 'monitoring', labels: { b: '2', a: '1' } }
  assert.deepEqual(parseScraperLines(scraperToLine(original)), [original])
})

test('提交体带上抓取端', () => {
  const values = { ...blankFormValues(), id: 'c1', displayName: 'C', podCidr: '10.4.0.0/14',
    nodeCidr: '10.128.0.0/20', metricsScrapers: 'monitoring  app=prometheus' }
  const req = buildClusterWrite(values)
  assert.deepEqual((req.body as Record<string, unknown>).metricsScrapers,
    [{ namespace: 'monitoring', labels: { app: 'prometheus' } }])
})

test('页面真的把这个字段渲染出来了', () => {
  const page = readFileSync(
    join(import.meta.dirname, '..', 'src', 'pages', 'ClustersPage.tsx'), 'utf8')
  assert.ok(page.includes('metricsScrapers'),
    'ClustersPage 没有渲染 metrics 抓取端：运维填不了，那一类 Baseline 永远缺失')
})
