import { readdirSync } from 'node:fs'
import { join } from 'node:path'
import test from 'node:test'
import assert from 'node:assert/strict'

import {
  NO_USABLE_COLLECTION_CODE, isNoUsableCollectionError, noCollectionView,
} from '../src/pages/noCollectionView.ts'
import { FLOW_NEVER_INGESTED_CODE } from '../src/pages/flowIngestView.ts'
import { readStripped } from './sourceScan.ts'

/** 按码判，不按文案判：文案会改，码不会。 */
test('按业务码识别，不匹配字符串', () => {
  assert.equal(isNoUsableCollectionError({ code: NO_USABLE_COLLECTION_CODE }), true)
  assert.equal(isNoUsableCollectionError({ msg: '该集群还没有可用的采集数据' }), false)
  assert.equal(isNoUsableCollectionError(null), false)
  assert.equal(isNoUsableCollectionError('该集群还没有可用的采集数据'), false)
})

/**
 * 两个码必须分得开。
 *
 * 20005 说的是「资产或这段窗口的数据不可用」，20009 说的是「流量这条链路
 * 从来没跑过」—— 处置一个是去看采集跑没跑过，一个是去部署采集器或开流量
 * 日志。混成一句，操作者会按错的那一半行动。
 */
test('与「从未摄入过流量」是两个码', () => {
  assert.notEqual(NO_USABLE_COLLECTION_CODE, FLOW_NEVER_INGESTED_CODE)
  assert.equal(isNoUsableCollectionError({ code: FLOW_NEVER_INGESTED_CODE }), false)
})

/**
 * 这条状态必须给出一个**去处**。
 *
 * 原先各页说的是「请先跑一次采集与流量摄入」—— 一句正确、但走不通的话：
 * 正确的入口在两跳之外，而界面上没有任何东西指过去。一个刚注册完集群的
 * 人就断在这里。
 */
test('必须说出去哪儿，不能只说去跑一次', () => {
  const v = noCollectionView()
  assert.ok(v.href.startsWith('/'), `href = ${v.href}，要的是一个站内去处`)
  assert.ok(v.hrefLabel.length > 0, '链接没有文字，读屏器只会念一个地址')
  assert.match(v.action, /资产采集|集群管理/,
    '处置没点名任何一屏 —— 那正是这条状态原先的毛病')
})

/**
 * 五屏共用一个组件，不各抄一份 JSX。
 *
 * 抄五份的那一天，同一个状态会有五句略有出入的话，而人会按先看到的那一屏
 * 行动 —— 这正是它要修的那个缺陷本身的形状。
 */
test('没有哪一屏自己拼这句话', () => {
  const dir = new URL('../src/pages', import.meta.url).pathname
  const offenders: string[] = []
  for (const name of readdirSync(dir)) {
    if (!name.endsWith('Page.tsx')) continue
    // 去掉注释再扫：一条解释这个缺陷为什么存在的注释本身会被判成缺陷。
    const src = readStripped(join(dir, name))
    if (src.includes('请先跑一次采集') || src.includes('还没有可用的采集数据')) {
      offenders.push(name)
    }
  }
  assert.deepEqual(offenders, [],
    '这些页面自己写了这句文案，而它属于 noCollectionView：' + offenders.join('、'))
})
