import test from 'node:test'
import assert from 'node:assert/strict'
import { readdirSync, readFileSync } from 'node:fs'
import { join } from 'node:path'

const PAGES_DIR = join(import.meta.dirname, '..', 'src', 'pages')
const pages = readdirSync(PAGES_DIR).filter((f) => f.endsWith('.tsx'))
const read = (f: string) => readFileSync(join(PAGES_DIR, f), 'utf8')

test('页面里不得再各自定义按钮样式', () => {
  // 在这之前四个页面各抄了一份同样的 buttonStyle。四份会各自漂，而漂的
  // 症状是同一个动作在不同页面上长得不一样，读者会以为它们的分量不同
  // （同 ui.tsx 抬头那条：三种表格会让人以为在讲三件事）。
  const offenders = pages.filter((f) => /const (secondary)?[Bb]uttonStyle\b/.test(read(f)))
  assert.deepEqual(offenders, [],
    `这些页面又自己定义了按钮样式：${offenders.join(', ')}；用 components/ui 的 Button`)
})

test('语义色只经由组件层使用，页面不直接拿判定色描边或填充', () => {
  // ALLOW / DENY / UNKNOWN 各自唯一，全站不得挪作他用 —— 一旦被拿去表示
  // 「跨 namespace」之类，用户就再也无法从颜色读出判定（tokens.css 抬头）。
  // 页面里直接写 background: var(--verdict-*) 是这条最容易破的地方。
  const offenders = pages.filter((f) => /background:\s*'var\(--verdict-(allow|unknown)/.test(read(f)))
  assert.deepEqual(offenders, [],
    `这些页面直接用判定色做背景：${offenders.join(', ')}`)
})

test('组件层自己不写死颜色', () => {
  const ui = readFileSync(join(import.meta.dirname, '..', 'src', 'components', 'ui.tsx'), 'utf8')
  const literals = ui.match(/#[0-9a-fA-F]{3,8}\b/g)
  assert.equal(literals, null,
    `components/ui.tsx 里有写死的颜色：${literals?.join(', ')}；色板只能来自 tokens.css`)
})

test('逐行重复的长说明要抽出来，不印在每一行上', () => {
  const page = readFileSync(join(PAGES_DIR, 'PolicyPage.tsx'), 'utf8')
  // 处置只取决于缺口种类，与 namespace 无关。UAT 上 42 个 namespace 全缺
  // NODE_AGENT，逐行渲染就是同一句话印 42 遍 —— 而一面读不完的墙与没有
  // 说明是同一个效果。
  assert.doesNotMatch(page, /\{gaps\.map\(\(g\) => \([\s\S]{0,120}g\.remedy/,
    '「处置」又回到了逐行渲染')
  assert.match(page, /RemedyLegend/,
    '整表一次的处置说明不见了 —— 说明被删掉与被折起来是两回事')
})
