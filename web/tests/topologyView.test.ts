import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { join } from 'node:path'

import {
  NO_TRAFFIC_NOTICE, edgeCountLabel, showsGraph, trafficNotice,
} from '../src/pages/topologyView.ts'
import type { Topology } from '../src/api/types.ts'

const src = (...parts: string[]) =>
  readFileSync(join(import.meta.dirname, '..', 'src', ...parts), 'utf8')

const PAGE_SOURCE = src('pages', 'TopologyPage.tsx')

function topo(over: Partial<Topology> = {}): Topology {
  return {
    nodes: [{
      id: 'c/shop', cluster: 'c', namespace: 'shop', workload: '',
      podCount: 3, hasPolicy: false, foreign: false,
    } as Topology['nodes'][number]],
    edges: [],
    unplaceableFlowCount: 0,
    level: 'namespace',
    trafficObserved: false,
    ...over,
  }
}

test('没有观测时给出提示，而不是让一张空图自己说话', () => {
  assert.equal(trafficNotice(topo()), NO_TRAFFIC_NOTICE)
  // 那句话必须说清是"没看过"，不是"没有"。
  assert.match(NO_TRAFFIC_NOTICE, /还没看过|没有看过|还没有任何流量观测/)
})

test('有观测时不提示', () => {
  assert.equal(trafficNotice(topo({ trafficObserved: true })), null)
})

test('没有观测时「边」不显示成 0', () => {
  // 0 是一次计数的结果，而我们没有数过。显示 0 会被读成
  // "这个集群有 0 条通信"。
  assert.notEqual(edgeCountLabel(topo()), '0')
  assert.equal(edgeCountLabel(topo()), '尚未观测')
  assert.equal(edgeCountLabel(topo({ trafficObserved: true })), '0')
})

test('没有观测时不画那张图', () => {
  // 没有边的图是一堆互不相连的点，看起来像一个"结构清晰、没有耦合"的集群。
  assert.equal(showsGraph(topo()), false)
  assert.equal(showsGraph(topo({ trafficObserved: true })), false)
  assert.equal(showsGraph(topo({
    trafficObserved: true,
    edges: [{ source: 'a', target: 'b' } as Topology['edges'][number]],
  })), true)
})

test('页面真的用了这三样，而不是自己另写一套判断', () => {
  // 没有 DOM 测试设施，只能钉住源码引用。抓不到"调用了但渲染在看不见的
  // 地方"，但抓得住"从没被调用"—— 而后者是这份改动最可能的失效方式。
  for (const symbol of ['trafficNotice', 'edgeCountLabel', 'showsGraph']) {
    assert.ok(PAGE_SOURCE.includes(symbol),
      `TopologyPage 没有引用 ${symbol}：空图会被画成一张干净的图`)
  }
  // 页面不得自己再数一遍边：那样这三个函数就成了摆设。
  assert.ok(!/topo\.edges\.length\}/.test(PAGE_SOURCE),
    'TopologyPage 直接渲染了 topo.edges.length，绕开了 edgeCountLabel')
})
