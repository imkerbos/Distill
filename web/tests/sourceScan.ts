import { readFileSync, readdirSync, statSync } from 'node:fs'
import { join } from 'node:path'

/*
 * 扫源码的两条守卫共用的工具。
 *
 * 抽出来是因为「要不要跳过注释」这件事两处必须一致：不跳过的话，一条解释
 * 这个缺陷为什么存在的注释本身会被判成缺陷 —— 实测如此。
 */

/**
 * 去掉注释。
 *
 * 块注释先去，`{/* ... *​/}` 这种 JSX 注释也在其中；行注释后去，且跳过
 * `://`，否则 URL 里的双斜杠会把半行代码当成注释吃掉。
 */
export function stripComments(src: string): string {
  const noBlocks = src.replace(/\/\*[\s\S]*?\*\//g, '')
  return noBlocks
    .split('\n')
    .map((line) => {
      const i = line.search(/(^|[^:])\/\//)
      return i < 0 ? line : line.slice(0, i)
    })
    .join('\n')
}

/** sourceFiles 递归列出一棵目录下的 .ts / .tsx。 */
export function sourceFiles(dir: string): string[] {
  const out: string[] = []
  for (const name of readdirSync(dir)) {
    const p = join(dir, name)
    if (statSync(p).isDirectory()) { out.push(...sourceFiles(p)); continue }
    if (/\.tsx?$/.test(name)) out.push(p)
  }
  return out
}

/** readStripped 读一个文件并去掉注释。 */
export function readStripped(path: string): string {
  return stripComments(readFileSync(path, 'utf8'))
}
