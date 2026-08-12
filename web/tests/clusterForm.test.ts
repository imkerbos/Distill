import test from 'node:test'
import assert from 'node:assert/strict'

import {
  blankFormValues, buildClusterWrite, formValuesOf, resolveGitBinding,
} from '../src/pages/clusterForm.ts'
import type { GitBinding, RegisteredCluster } from '../src/api/types.ts'

const binding: GitBinding = {
  repoUrl: 'https://gitlab.example.com/net/policies.git',
  branch: 'main',
  policyPath: 'clusters/prod-asia-1',
  credentialRef: 'git-token',
  lastWrittenCommit: '0123456789abcdef0123456789abcdef01234567',
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
