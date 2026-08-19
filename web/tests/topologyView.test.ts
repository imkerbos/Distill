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
      id: 'c/shop', cluster: 'c', namespace: 'shop',
      inMesh: false, podCount: 3, unmanagedPodCount: 0, hasPolicy: false, foreign: false,
    }],
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

/* ---------------------------------------------------------------------- */
/* 图的可读性                                                              */
/* ---------------------------------------------------------------------- */

const GRAPH_SOURCE = readFileSync(
  join(import.meta.dirname, '..', 'src', 'components', 'TopologyGraph.tsx'), 'utf8')

test('边必须画出方向，而不是只写在悬浮提示里', () => {
  // 对一个 NetworkPolicy 工具，方向是最要紧的那个事实：「A 调 B」与
  // 「B 调 A」产出完全不同的策略（A 的 egress 对 B 的 ingress）。而悬浮层
  // 在触屏上摸不到、在截图与打印里不存在 —— 这张图会被贴进工单。
  assert.match(GRAPH_SOURCE, /markerEnd=/,
    '边上没有箭头，方向只能从悬浮提示里读')
  assert.match(GRAPH_SOURCE, /<marker\b/, '没有定义箭头 marker')
})

test('每个判定一个箭头 marker，不共用一个', () => {
  // SVG 的 marker 不继承 line 的 stroke：共用一个会让全部箭头变成同一个
  // 颜色，于是箭头与线身各说各的判定。
  assert.match(GRAPH_SOURCE, /arrow-\$\{v\}|arrow-\$\{e\.verdict\}/,
    '箭头 marker 的 id 不随判定变化，说明三种判定共用了一个箭头颜色')
})

test('边是弧线，五六条汇到一个节点时才分得开', () => {
  // 直线在多条边汇聚时会叠成一束分不开的线，读者数不出有几条、也看不出
  // 各自从哪来。轻微的弧度让它们各走各的路径。
  assert.match(GRAPH_SOURCE, /arcPath\(/, '边退回直线了')
  assert.doesNotMatch(GRAPH_SOURCE, /<line\b/, '还留着直线画法')
})

test('节点标签描背景色，否则会被连线切断', () => {
  // 力导图里连线必然从标签底下穿过。不描边的字会被线切断，读者要凑近才
  // 认得出是哪个 namespace —— 而认不出节点名，这张图什么都答不了。
  // 数出来，不只看"有没有出现过"：图上有两处文字（节点名与「无策略」），
  // 只描其中一处时另一处照样会被线切断，而一条只看存在性的断言分辨不出来。
  const labels = GRAPH_SOURCE.match(/<text\b/g) ?? []
  const haloed = GRAPH_SOURCE.match(/paintOrder:\s*'stroke'/g) ?? []
  assert.equal(haloed.length, labels.length,
    `${labels.length} 处文字里只有 ${haloed.length} 处描了边；没描的那处会被连线切断`)
})
