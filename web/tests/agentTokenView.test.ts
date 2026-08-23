import test from 'node:test'
import assert from 'node:assert/strict'

import {
  AGENT_TOKEN_ONCE_WARNING, activeAgentCount, agentStateLabel, isRevocable, lastSeenLabel,
} from '../src/pages/agentTokenView.ts'
import type { ClusterAgent } from '../src/api/types.ts'

function agent(over: Partial<ClusterAgent> = {}): ClusterAgent {
  return {
    agentId: 'a1', state: 'ACTIVE', createdBy: 'admin',
    createdAt: '2026-08-20T00:00:00Z', ...over,
  }
}

test('两个状态各有各的话，且互不相同', () => {
  assert.notEqual(agentStateLabel('ACTIVE'), agentStateLabel('REVOKED'))
  assert.notEqual(agentStateLabel('ACTIVE').trim(), '')
  // 未登记取值不静默渲染成空标签。
  assert.match(agentStateLabel('BOGUS' as ClusterAgent['state']), /未登记/)
})

// 只有 ACTIVE 可吊销：给一把已吊销的再画吊销按钮，点了是空操作，会让人
// 以为自己改变了什么。
test('已吊销的 token 不再提供吊销', () => {
  assert.equal(isRevocable(agent({ state: 'ACTIVE' })), true)
  assert.equal(isRevocable(agent({ state: 'REVOKED' })), false)
})

// 活的把数单独算：一把忘了吊销的 token 只有靠这个数字对不上才被发现。
test('活跃计数只数 ACTIVE，不含历史吊销', () => {
  const list = [agent({ state: 'ACTIVE' }), agent({ state: 'REVOKED' }), agent({ state: 'ACTIVE' })]
  assert.equal(activeAgentCount(list), 2)
  assert.equal(activeAgentCount([]), 0)
})

// 「从未连接」与某个真实时刻是两件事：前者是签了没部署，后者是曾在跑。
test('从未连接的 token 不显示成某个时刻', () => {
  assert.equal(lastSeenLabel(agent({ lastSeenAt: undefined })), '从未连接')
  assert.notEqual(lastSeenLabel(agent({ lastSeenAt: '2026-08-21T03:04:05Z' })), '从未连接')
})

// 一次性告示必须说清「关掉就没了」，否则等于没警告。
test('一次性 token 告示说清关掉后拿不回来', () => {
  assert.match(AGENT_TOKEN_ONCE_WARNING, /只显示这一次|无法再查看|再也/)
  assert.match(AGENT_TOKEN_ONCE_WARNING, /重签|吊销/)
})
