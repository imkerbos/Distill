import assert from 'node:assert/strict'
import test from 'node:test'

import { UNKNOWN_REASON_LABEL } from '../src/api/types.ts'

// 新增的 unknown_reason 必须有文案。
//
// 封闭枚举加了取值而前端没跟上，界面上会显示原始枚举名或落进兜底文案 ——
// 而这条取值恰恰是要告诉操作者「不用去查采集」的那一条。
test('LB_INGRESS_ADDRESS 有中文文案', () => {
  const label = UNKNOWN_REASON_LABEL['LB_INGRESS_ADDRESS']
  assert.ok(label, 'LB_INGRESS_ADDRESS 没有文案')
  assert.notEqual(label, 'LB_INGRESS_ADDRESS')
})

// 这条与 SNAPSHOT_MISSING 处置方向相反：后者送人去查采集，前者是判完了
// 的结论。两句话不能撞车，否则操作者分不出该不该去查。
test('LB_INGRESS_ADDRESS 与 SNAPSHOT_MISSING 的措辞分得开', () => {
  const lb = UNKNOWN_REASON_LABEL['LB_INGRESS_ADDRESS']
  const snapshot = UNKNOWN_REASON_LABEL['SNAPSHOT_MISSING']
  assert.notEqual(lb, snapshot)
  // 这一条不该带"缺"字——它不是数据缺了一块，是这里本来就没有 Pod 主体。
  assert.doesNotMatch(lb, /缺/)
})
