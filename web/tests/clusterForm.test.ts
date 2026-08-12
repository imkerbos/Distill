import { readFileSync } from 'node:fs'
import test from 'node:test'
import assert from 'node:assert/strict'

import {
  ALL_VERIFY_RESULTS, blankFormValues, buildClusterWrite, describeVerifyStatus,
  formatUtcTime, formValuesOf, resolveGitBinding,
} from '../src/pages/clusterForm.ts'
import type { GitBinding, RegisteredCluster, VerifyResult } from '../src/api/types.ts'

const binding: GitBinding = {
  repoUrl: 'https://gitlab.example.com/net/policies.git',
  branch: 'main',
  policyPath: 'clusters/prod-asia-1',
  credentialRef: 'git-token',
  lastWrittenCommit: '0123456789abcdef0123456789abcdef01234567',
  verifyResult: 'NOT_VERIFIED',
}

/** 造一个只在校验两列上不同的绑定，其余字段与 binding 相同。 */
function verified(result: VerifyResult, at?: string): GitBinding {
  return { ...binding, verifyResult: result, verifiedAt: at }
}

function bound(): RegisteredCluster {
  return {
    id: 'prod-asia-1', displayName: '亚太生产', podCidr: '10.4.0.0/14',
    nodeCidr: '10.128.0.0/20', ccnpPresent: false, state: 'READY',
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

/**
 * 这是本轮最重要的一条：PUT 整体替换，表单里没有的字段提交后就是库里
 * 没有的字段。一个只想改仓库地址的操作者，绝不该因此丢掉 apiserver 清单
 * 或健康检查网段 —— 少掉的是 baseline 里的一条放行规则，事后表现为
 * 生产阻断，而不是提交时报错。
 *
 * 断言落在「提交体」上而不是「表单显示了什么」：真正被写进库的是前者。
 */
test('编辑只改仓库地址时，未触碰的字段原样提交', () => {
  const cluster = bound()
  const values = formValuesOf(cluster)
  values.git.repoUrl = 'https://gitlab.example.com/net/policies-v2.git'

  const built = buildClusterWrite(values, cluster.git ?? null)
  assert.equal(built.ok, true)
  if (!built.ok) return

  assert.equal(built.body.podCidr, '10.4.0.0/14')
  assert.equal(built.body.nodeCidr, '10.128.0.0/20')
  assert.equal(built.body.displayName, '亚太生产')
  assert.deepEqual(built.body.apiServers, [{ host: '10.9.0.2', cidr: '10.9.0.0/28', port: 443 }])
  assert.deepEqual(built.body.healthCheckSources, ['35.191.0.0/16', '130.211.0.0/22'])
  assert.equal(built.body.git?.repoUrl, 'https://gitlab.example.com/net/policies-v2.git')
})

/**
 * 「四个字段都空着」在编辑一个已绑定集群时是有歧义的：可能是要解除，
 * 也可能是误删了输入框内容。整体替换让这两种意图提交结果相同，所以
 * 必须在提交前把它们分开 —— 拒绝并要求勾选，而不是替操作者猜。
 */
test('已绑定集群清空四个字段不等于解除，必须拒绝', () => {
  const cluster = bound()
  const values = formValuesOf(cluster)
  values.git = { repoUrl: '', branch: '', policyPath: '', credentialRef: '' }

  const built = buildClusterWrite(values, cluster.git ?? null)
  assert.equal(built.ok, false)
  if (built.ok) return
  assert.match(built.error, /解除 Git 绑定/)
})

test('勾选解除后提交 null，且文案点名当前绑定的去向', () => {
  const cluster = bound()
  const values = formValuesOf(cluster)
  values.clearGit = true

  const resolution = resolveGitBinding(values.git, { current: binding, clearRequested: true })
  assert.equal(resolution.ok, true)
  if (!resolution.ok) return
  assert.equal(resolution.git, null)
  assert.match(resolution.summary, /解除 Git 绑定/)
  assert.match(resolution.summary, /gitlab\.example\.com/)

  const built = buildClusterWrite(values, cluster.git ?? null)
  assert.equal(built.ok, true)
  if (!built.ok) return
  // null 而非 undefined：字段缺席读起来像"这次不谈这件事"，
  // 在整体替换语义下含糊的那一种不该出现在写路径上。
  assert.equal(built.body.git, null)
})

/**
 * credentialRef 单独填写同样触发三项必填。它是唯一一个不在必填清单里
 * 的字段，若不由「任意一项非空」触发检查，它录入的值会被静默丢弃。
 */
test('只填 credentialRef 也要求 repoUrl / branch / policyPath', () => {
  const values = blankFormValues()
  values.git.credentialRef = 'git-token'

  const built = buildClusterWrite(values, null)
  assert.equal(built.ok, false)
  if (built.ok) return
  assert.match(built.error, /repoUrl/)
  assert.match(built.error, /branch/)
  assert.match(built.error, /policyPath/)
})

/**
 * 提交体里不带 lastWrittenCommit —— 一个字节都不带。
 *
 * 它是平台对「我最近一次往这个仓库写了什么」的断言，漂移检测拿它与 Git
 * 现状比对；能被客户端设定的基准可以被调成与仓库现状一致，于是「无漂移」
 * 这句话再也无法被证伪。基准由服务端从库里的现值推导（同一仓库同一分支
 * 沿用，改指向则归零），这里唯一要守的是「客户端不参与」。
 *
 * 断言的是键不存在，而不是值等于空串：把库里的 SHA 原样回填也会让
 * 「值等于某个东西」的断言通过，而那正是要禁止的行为。
 */
test('提交体不含 lastWrittenCommit：漂移基准不由客户端提供', () => {
  const cluster = bound()
  const values = formValuesOf(cluster)
  values.git.policyPath = 'clusters/prod-asia-1/net'

  const built = buildClusterWrite(values, binding)
  assert.equal(built.ok, true)
  if (!built.ok) return
  assert.notEqual(built.body.git, null)
  assert.equal(Object.hasOwn(built.body.git as object, 'lastWrittenCommit'), false,
    '客户端一旦能带上这个字段，伪造的基准就有了入口')
  // 其余四项照常提交：credentialRef 是操作者填写的引用，本就该由调用方给。
  assert.equal(built.body.git?.credentialRef, 'git-token')
  assert.equal(built.body.git?.policyPath, 'clusters/prod-asia-1/net')
})

/** 未绑定集群补上绑定：这是本轮的动机场景，prod-eu-1 的真实形态。 */
test('未绑定集群可以补上绑定，且不受「清空即解除」的拦截', () => {
  const cluster = unbound()
  const values = formValuesOf(cluster)
  assert.deepEqual(values.git, { repoUrl: '', branch: '', policyPath: '', credentialRef: '' })

  values.git = {
    repoUrl: 'https://gitlab.example.com/net/policies.git',
    branch: 'main', policyPath: 'clusters/prod-eu-1', credentialRef: '',
  }
  const built = buildClusterWrite(values, null)
  assert.equal(built.ok, true)
  if (!built.ok) return
  assert.equal(built.body.git?.policyPath, 'clusters/prod-eu-1')
  assert.deepEqual(built.body.apiServers, [{ host: '10.10.0.2', cidr: '10.10.0.0/28', port: 443 }])
})

/**
 * 校验结论与校验时刻都不是可提交项。
 *
 * 它们是平台对绑定可信度的判断，由服务端在保存时自己校验后写入。一个
 * 能由调用方提交的结论可以被填成 OK，于是「已校验通过」这句话再也无法
 * 被证伪 —— 与 lastWrittenCommit 同一条理由，且后果更直接：轮 4 写回前
 * 会看这个结论。
 *
 * 断言的是键不存在，而不是值等于某个东西：把库里的现值原样回填同样能
 * 让「值等于 NOT_VERIFIED」的断言通过，而那正是要禁止的行为。
 */
test('提交体不含 verifyResult / verifiedAt：校验结论不由客户端提供', () => {
  const cluster = bound()
  cluster.git = verified('OK', '2026-08-12T15:20:15Z')
  const values = formValuesOf(cluster)

  const built = buildClusterWrite(values, cluster.git)
  assert.equal(built.ok, true)
  if (!built.ok) return
  assert.notEqual(built.body.git, null)
  const git = built.body.git as object
  assert.equal(Object.hasOwn(git, 'verifyResult'), false,
    '客户端一旦能带上结论，就能把一个没校验过的绑定标成 OK')
  assert.equal(Object.hasOwn(git, 'verifiedAt'), false,
    '客户端一旦能带上时刻，一个伪造的"刚刚校验过"就有了入口')
})

/**
 * 七个取值必须各有非空文案。
 *
 * ALL_VERIFY_RESULTS 从 `Record<VerifyResult, string>` 的键推导，所以
 * 后端新增第八个取值时 `tsc` 先报错；这条测试守的是另一半 —— 已登记的
 * 取值不许有人把文案改空。一格空白在这张表里会被读成「这个绑定没问题」。
 */
test('七个校验结论各有非空文案，且互不相同', () => {
  assert.equal(ALL_VERIFY_RESULTS.length, 7)

  const labels = new Set<string>()
  for (const result of ALL_VERIFY_RESULTS) {
    const view = describeVerifyStatus(verified(result))
    assert.notEqual(view.label.trim(), '', `${result} 的结论文案为空`)
    assert.notEqual(view.detail.trim(), '', `${result} 的说明文案为空`)
    assert.notEqual(view.label, result, `${result} 直接把枚举原样显示了`)
    labels.add(view.label)
  }
  assert.equal(labels.size, 7, '有两个结论共用了同一句文案，界面上分不开')
})

/**
 * 「未校验」与「校验通过」是相反的两件事实，必须一眼可分。
 *
 * 语气分档单独断言而不是只比文案：颜色和描边由 tone 决定，两个不同的
 * 标签配同一种画法，扫一眼表格时仍然分不出来。
 */
test('NOT_VERIFIED 与 OK 在文案与语气上都可分，且都不是空白', () => {
  const notVerified = describeVerifyStatus(verified('NOT_VERIFIED'))
  const ok = describeVerifyStatus(verified('OK', '2026-08-12T15:20:15Z'))

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
  const view = describeVerifyStatus(verified('PATH_MISSING'))
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
 * 覆盖面要说清楚：这条只管 describeVerifyStatus 返回的文案。组件里
 * 另写的字符串不在它的射程内，那部分由下面那条源码断言兜一层。
 */
test('校验文案里不出现「写」——只读校验得不出与写有关的结论', () => {
  for (const result of ALL_VERIFY_RESULTS) {
    const view = describeVerifyStatus(verified(result))
    assert.equal(view.label.includes('写'), false, `${result} 的结论文案谈到了写`)
    assert.equal(view.detail.includes('写'), false, `${result} 的说明文案谈到了写`)
  }
})

/**
 * 平台侧配置错与仓库侧权限错要送不同的人去修不同的系统，文案必须让人
 * 一眼看出该找谁（design doc §3.2）。
 */
test('CREDENTIAL_UNRESOLVED 与 AUTH_FAILED 分别点名平台侧与仓库侧', () => {
  const unresolved = describeVerifyStatus(verified('CREDENTIAL_UNRESOLVED'))
  const authFailed = describeVerifyStatus(verified('AUTH_FAILED'))

  assert.notEqual(unresolved.label, authFailed.label)
  assert.match(unresolved.detail, /平台/)
  assert.match(authFailed.detail, /仓库/)
  assert.equal(unresolved.detail.includes('仓库'), false,
    '说明里同时提到两侧，读者仍然不知道该找谁')
})

/**
 * verifiedAt 必须读成一个过去的时刻，不是当前状态。
 *
 * 一个孤零零的时间戳挨着「只读校验通过」，读起来就是「现在是通过的」。
 * 轮 4 写回前必须重新校验，拿几天前的结论当此刻的状态正是 design doc
 * §3.4 禁止的那件事。
 */
test('verifiedAt 明示为历史时刻，缺失时也不留空', () => {
  const checked = describeVerifyStatus(verified('OK', '2026-08-09T01:02:03Z'))
  assert.match(checked.checkedAt, /2026-08-09 01:02:03 UTC/)
  assert.match(checked.checkedAt, /上次校验/)
  assert.match(checked.checkedAt, /不代表此刻的状态/)

  const never = describeVerifyStatus(verified('NOT_VERIFIED'))
  assert.notEqual(never.checkedAt.trim(), '')
  assert.match(never.checkedAt, /从未校验/)
})

/**
 * 界面还不认识的取值一律按未校验处置，不透出原始码、不留空。
 *
 * 失败方向朝「未确认」关，不朝「可信」开 —— 与后端 VerifyResult.Valid
 * 的收窄同一条纪律。`as VerifyResult` 是刻意的：这里模拟的正是运行时
 * 收到一个类型系统没预料到的值。
 */
test('未登记的结论收窄成未校验，不显示成空白或裸枚举', () => {
  const view = describeVerifyStatus(verified('PROBABLY_FINE' as VerifyResult))
  const ok = describeVerifyStatus(verified('OK'))

  assert.notEqual(view.label.trim(), '')
  assert.notEqual(view.label, 'PROBABLY_FINE')
  assert.notEqual(view.label, ok.label)
  assert.equal(view.tone, 'unverified')
})

test('时间戳解析不出来时原样返回，不让一张表白屏', () => {
  assert.equal(formatUtcTime('not-a-time'), 'not-a-time')
  assert.equal(formatUtcTime('2026-08-12T15:20:15.123456Z'), '2026-08-12 15:20:15 UTC')
})

/**
 * 上面所有断言管的都是 describeVerifyStatus 的返回值。没有任何一条能
 * 证明列表那一格真的把它渲染出来了 —— 这个项目上一次前端缺陷正是这个
 * 形状：dry-run 磁贴和它下面的表格报着两份不同的数字，四道门禁全绿。
 *
 * `node --test` 的类型擦除读不了 JSX，组件没法在这里挂载，所以退而求
 * 其次断言源码：那一格调用了 describeVerifyStatus，而不是自己另写一套
 * 文案。这是文本级的绑定，不是渲染级的 —— 它抓得住「有人绕过这个函数
 * 自己写死一句话」，抓不住「调用了但把 label 渲染到了看不见的地方」。
 * 这个局限是真的，不该假装门禁覆盖了它。
 */
test('集群列表确实经由 describeVerifyStatus 渲染校验结论', () => {
  const src = readFileSync(new URL('../src/pages/ClustersPage.tsx', import.meta.url), 'utf8')

  assert.match(src, /describeVerifyStatus\(/,
    '这一格没有调用 describeVerifyStatus，上面那一整组文案断言就管不到界面')
  assert.match(src, /<GitBindingCell\b/)
  for (const banned of ['可以写入', '不可写入', '可写入', '不可写']) {
    assert.equal(src.includes(banned), false,
      `ClustersPage 里出现了「${banned}」：只读校验得不出与写有关的结论`)
  }
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

  const built = buildClusterWrite(values, null)
  assert.equal(built.ok, false)
  if (built.ok) return
  assert.match(built.error, /第 2 行/)
  assert.match(built.error, /host/)
})
