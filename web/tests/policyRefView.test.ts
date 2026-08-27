import test from 'node:test'
import assert from 'node:assert/strict'

import { isAdminPlane, policyRefView } from '../src/pages/policyRefView.ts'

/**
 * 三种前缀必须分得开。
 *
 * 判定里那一栏同时装着 NetworkPolicy 的 `namespace/name` 与两种集群级策略。
 * 不区分的话，一条 ANP 的命中读起来像一条 anp 命名空间里的 NetworkPolicy ——
 * 而一个操作者看到 DENY 却以为是自己命名空间里的策略拦的，会去改一份根本
 * 不起作用的 YAML。
 */
test('三个平面各自认得出来', () => {
  assert.equal(policyRefView('payment/deny-all').plane, 'NETWORK_POLICY')
  assert.equal(policyRefView('anp/tenant-iso').plane, 'ADMIN_NETWORK_POLICY')
  assert.equal(policyRefView('banp/default').plane, 'BASELINE_ADMIN_NETWORK_POLICY')
})

/** 前缀要去掉：屏幕上显示的是策略名，不是内部引用。 */
test('显示名去掉前缀', () => {
  assert.equal(policyRefView('anp/tenant-iso').name, 'tenant-iso')
  assert.equal(policyRefView('banp/default').name, 'default')
  // NetworkPolicy 的 namespace/name 是它完整的名字，不动它。
  assert.equal(policyRefView('payment/deny-all').name, 'payment/deny-all')
})

/**
 * 认不出前缀时按 NetworkPolicy 处理。
 *
 * 那是这一栏历史上唯一的形态。倒过来猜（当成管理面策略）会让一条普通
 * NetworkPolicy 被说成"压过了别的策略"。
 */
test('认不出的形状按 NetworkPolicy 处理', () => {
  for (const ref of ['', 'something', 'a/b/c', 'anpx/y']) {
    assert.equal(policyRefView(ref).plane, 'NETWORK_POLICY', `ref = ${JSON.stringify(ref)}`)
    assert.equal(isAdminPlane(ref), false)
  }
})

/**
 * 两种管理面策略与 NetworkPolicy 的次序关系必须说出来，而且方向相反 ——
 * ANP 在它之前、压过它；BANP 在它之后兜底。说反了，操作者会照着错的那半
 * 去动策略。
 */
test('次序关系说得出来，且两者方向相反', () => {
  const anp = policyRefView('anp/x')
  const banp = policyRefView('banp/default')
  assert.match(anp.precedence, /之前/)
  assert.match(banp.precedence, /没有被任何 NetworkPolicy 选中/)
  assert.ok(!anp.precedence.includes('兜底'), 'ANP 不是兜底，它压过 NetworkPolicy')
  assert.ok(isAdminPlane('anp/x') && isAdminPlane('banp/default'))
})
