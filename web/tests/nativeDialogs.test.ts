import test from 'node:test'
import assert from 'node:assert/strict'

import { readStripped, sourceFiles } from './sourceScan.ts'

const SRC = new URL('../src', import.meta.url).pathname

/**
 * 界面里不许再出现浏览器原生对话框。
 *
 * window.confirm 与 window.alert 有三条毛病，按分量排：
 *
 * 一、**只能显示一行纯文本**。这个平台的不可逆动作要说清后果 —— 下线一个集群
 * 会让它的 agent 凭据立刻失效、且没有恢复入口 —— 而那不是一句话能讲完的事。
 * 塞进原生框的结果是一大段挤在一起的文字，读的人只会按回车。
 *
 * 二、**长得不属于这个产品**。外观由操作系统决定，与卡片、表格、按钮的质感
 * 完全对不上（spec §17.1：组件系统规整）。
 *
 * 三、**阻塞整个渲染进程**。不是理论问题：本仓库的浏览器自动化被它挂死过 ——
 * 一个没人应答的原生框会让页面此后再也不响应任何脚本。
 *
 * 替代品：确认走 useConfirm，报错走页内的 ErrorNotice —— 后者还有一个额外
 * 好处，服务端写明的拒绝理由（「先去解除哪一处绑定」那一类）会留在屏幕上
 * 让人照着做，而不是点掉之后就再也找不回来。
 */
test('没有页面再用浏览器原生对话框', () => {
  const offenders: string[] = []
  for (const file of sourceFiles(SRC)) {
    const stripped = readStripped(file)
    stripped.split('\n').forEach((line, i) => {
      if (/\b(window\.)?(confirm|alert)\s*\(/.test(line) && !line.includes('await confirm(')) {
        offenders.push(`${file.slice(SRC.length + 1)}:${i + 1}  ${line.trim().slice(0, 70)}`)
      }
    })
  }
  assert.deepEqual(offenders, [],
    '这些地方还在用浏览器原生对话框：\n' + offenders.join('\n'))
})
