import { readFileSync, readdirSync, statSync } from 'node:fs'
import { join } from 'node:path'
import test from 'node:test'
import assert from 'node:assert/strict'

/**
 * 界面文案里不许出现 markdown 的强调记号。
 *
 * 这个 UI 不渲染 markdown —— `**这样写**` 会把星号原样显示给操作者。
 * 一次真实的浏览器巡检在四个位置抓到了它，其中两处藏在折叠区里，不展开
 * `<details>` 根本扫不到。写的人看见的是自己编辑器里的加粗，读的人看见的
 * 是一串星号，而这类瑕疵会顺着"这份界面不太讲究"的印象扩散到它旁边那些
 * 真正要被当真的判定文案上。
 *
 * 只扫注释之外的部分：注释里用 `**` 强调是这个仓库的既有习惯，它不会被渲染。
 */
const SRC = new URL('../src', import.meta.url).pathname

function sourceFiles(dir: string): string[] {
  const out: string[] = []
  for (const name of readdirSync(dir)) {
    const p = join(dir, name)
    if (statSync(p).isDirectory()) { out.push(...sourceFiles(p)); continue }
    if (/\.tsx?$/.test(name)) out.push(p)
  }
  return out
}

/**
 * 去掉注释。
 *
 * 块注释先去，`{/* ... *​/}` 这种 JSX 注释也在其中；行注释后去，且跳过
 * `://`，否则 URL 里的双斜杠会把半行代码当成注释吃掉。
 */
function stripComments(src: string): string {
  const noBlocks = src.replace(/\/\*[\s\S]*?\*\//g, '')
  return noBlocks
    .split('\n')
    .map((line) => {
      const i = line.search(/(^|[^:])\/\//)
      return i < 0 ? line : line.slice(0, i)
    })
    .join('\n')
}

test('界面文案里没有渲染不出来的 markdown 记号', () => {
  const offenders: string[] = []
  for (const file of sourceFiles(SRC)) {
    const stripped = stripComments(readFileSync(file, 'utf8'))
    stripped.split('\n').forEach((line, i) => {
      if (line.includes('**')) {
        offenders.push(`${file.slice(SRC.length + 1)}:${i + 1}  ${line.trim().slice(0, 80)}`)
      }
    })
  }
  assert.deepEqual(offenders, [],
    '这些文案带着 markdown 强调记号，而界面不渲染 markdown —— 操作者会看到一串星号：\n'
    + offenders.join('\n'))
})
