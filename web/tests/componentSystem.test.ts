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

test('品牌色不用于任何数据', () => {
  // 主色（靛蓝）只用于导航选中态、焦点环、链接。它出现在数据上，界面就有了
  // 第二套颜色语言 —— 而读者分不清哪一套在讲判定。数据只由语义色与中性色
  // 表达（tokens.css 里 --brand 的说明）。
  const offenders = pages.filter((f) => /var\(--brand/.test(read(f)))
  assert.deepEqual(offenders, [],
    `这些页面用了品牌色：${offenders.join(', ')}；数据只用语义色与中性色`)
})

test('暗色模式必须重映射判定色，不能只换中性色', () => {
  const tokens = readFileSync(
    join(import.meta.dirname, '..', 'src', 'tokens.css'), 'utf8')
  const dark = tokens.slice(tokens.indexOf('prefers-color-scheme: dark'))
  assert.notEqual(dark, '', 'tokens.css 里没有暗色模式')
  for (const name of ['--verdict-allow', '--verdict-deny', '--verdict-unknown']) {
    assert.ok(dark.includes(`${name}:`),
      `暗色下没有重新定义 ${name} —— 浅色那三个在深底上会掉到读不出的对比度，`
      + '而它们是判定结论的唯一载体，读不出等于结论丢了')
  }
})

test('加载态不是一行字', () => {
  // 一行「加载中…」与一个失败的空屏在视觉上没有区别，而"这一屏是空的"
  // 与"这一屏还没答上来"是两件必须分得开的事（同 EmptyState 的纪律）。
  const offenders = pages.filter((f) => /<p[^>]*>加载中…<\/p>/.test(read(f)))
  assert.deepEqual(offenders, [],
    `这些页面又用纯文字当加载态：${offenders.join(', ')}；用 Skeleton`)
})

test('表单控件走统一类，不各写一份内联样式', () => {
  // 此前 ClustersPage 与 SettingsPage 各抄了一份 textareaStyle，与
  // buttonStyle 是同一个病：几份会各自漂，而漂的症状是同一种控件在不同
  // 页面上高矮不一。
  const offenders = pages.filter((f) => /const (input|textarea)Style\b/i.test(read(f)))
  assert.deepEqual(offenders, [],
    `这些页面又自己定义了输入控件样式：${offenders.join(', ')}；用 .ctl`)
})

test('失败提示只有一处定义', () => {
  // 此前四个页面各抄了一份逐字节相同的 FormError。这是 buttonStyle 与
  // textareaStyle 之后的第三次同一个病 —— 而失败提示是**唯一允许借用判定色
  // 的非判定场景**，借得越随意，这条例外越站不住。收在一处，例外就只有
  // 一个位置。
  const offenders = pages.filter((f) => /function FormError\b/.test(read(f)))
  assert.deepEqual(offenders, [],
    `这些页面又自己定义了失败提示：${offenders.join(', ')}；用 ErrorNotice`)
})

test('表格只有一套样式', () => {
  // 三个页面三种表格，读者会以为它们在讲不同性质的事（ui.tsx 抬头）。
  // 表格是这个产品的主要信息载体，样式集中在 .dt 与 TableCard，不由各页
  // 各写一份。
  const offenders = pages.filter((f) => /<table(?![^>]*className="dt")/.test(read(f)))
  assert.deepEqual(offenders, [],
    `这些页面自己拼了表格：${offenders.join(', ')}；用 TableCard 或 className="dt"`)
})

/* ---------------------------------------------------------------------- */
/* 交互控件走无头组件，不手写                                                */
/* ---------------------------------------------------------------------- */

const COMPONENTS_DIR = join(import.meta.dirname, '..', 'src', 'components')
const readComp = (f: string) => readFileSync(join(COMPONENTS_DIR, f), 'utf8')

test('抽屉/弹层由 Radix Dialog 承担，不是自己摆一个 fixed 的盒子', () => {
  // 手写的浮层跑不掉这四样：焦点陷阱、Escape 关闭、role="dialog"、
  // aria-modal。写漏的症状不会出现在截图里 —— 键盘用户打开它之后焦点
  // 还留在背景里，读屏器根本不知道弹出了东西。
  const drawer = readComp('DecisionDrawer.tsx')
  assert.match(drawer, /<Drawer\b/, 'DecisionDrawer 没走共用的 Drawer')
  assert.doesNotMatch(drawer, /position:\s*'fixed'/,
    '还留着手写的 fixed 定位浮层')
  // Drawer 本体必须真的建在 Dialog 上，否则上一条只是换了个名字。
  assert.match(readComp('radix.tsx'), /@radix-ui\/react-dialog/,
    'Drawer 不是建在 Radix Dialog 上')
})

test('下拉与勾选走无头组件', () => {
  // 只看真的渲染出来的标签，不看注释里提到的那个词。
  const ui = readComp('ui.tsx').replace(/\/\*[\s\S]*?\*\//g, '')
  assert.doesNotMatch(ui, /<select\b/,
    'Select 还在包原生 <select>：原生下拉在 macOS 与 Windows 上外观差异很大，'
    + '与卡片、表格的质感对不上，界面会显得是几段拼起来的')
  const offenders = pages.filter((f) => /<input[^>]*type="checkbox"/.test(read(f)))
  assert.deepEqual(offenders, [],
    `这些页面还在用原生 checkbox：${offenders.join(', ')}`)
})

test('组件层没有被压扁成一个装满类名的外壳', () => {
  // 一次按行号合并 className 的机械改动把 PageHeader / Section / AppShell 的
  // 内层元素（h1 / h2 / span / p）的类全部抽到了外层 div 上，内层变成裸标签。
  // 编译过、测试过、构建过 —— 只有肉眼能看出那三屏塌成了漂浮的文字。
  //
  // 判据：一个 className 长到超过 90 个字符，几乎一定是被合并进来的一堆
  // 互不相干的类（真实的组合到不了那个长度）。
  for (const f of ['ui.tsx', 'AppShell.tsx', 'radix.tsx']) {
    const long = (readComp(f).match(/className="[^"]{90,}"/g) ?? [])
      .filter((c) => !c.includes('\n'))
    assert.deepEqual(long, [],
      `${f} 里有超长 className，多半是内层元素的类被合并到了外层：${long.join(' | ')}`)
  }
})

test('标题元素自己带样式，不靠父节点', () => {
  const ui = readComp('ui.tsx')
  // 上面那次破坏的直接症状就是 <h1> 与 <h2> 变成了裸标签。
  assert.doesNotMatch(ui, /<h1\s*\n\s*>/, 'PageHeader 的 h1 是裸标签')
  assert.doesNotMatch(ui, /<h2\s*\n\s*>/, 'Section 的 h2 是裸标签')
  assert.match(ui, /<h1\s+className=/, 'PageHeader 的 h1 没有自己的类')
  assert.match(ui, /<h2\s+className=/, 'Section 的 h2 没有自己的类')
})

test('窄格里的徽标不许被逐字折行', () => {
  // 「有 · 判定降级」在 CCNP 那一格里被折成了一列单字。这种坏法在截图里
  // 一眼可见、在代码里看不出来 —— 也不会有任何测试因此变红，除非专门盯着。
  // 盯住那个徽标自己那一段，不是它附近某处也出现过这个类 —— 第一版
  // 用了一个 1200 字符的切片，把邻近组件的 nowrap 一起算了进来，变异
  // 因此没红。
  // 剥掉注释再断言：这一轮已经第四次出现守卫被注释里的词骗过 ——
  // 这次是我解释"为什么要加 whitespace-nowrap"的那句注释自己命中了断言。
  const src = read('ClustersPage.tsx')
    .replace(/\/\*[\s\S]*?\*\//g, '')
    .replace(/^\s*\/\/.*$/gm, '')
  const at = src.indexOf('有 · 判定降级')
  assert.ok(at > 0, '找不到 CCNP 徽标')
  const badge = src.slice(Math.max(0, at - 420), at)
  assert.match(badge, /whitespace-nowrap/,
    'CCNP 徽标没有禁止折行；这一格很窄，不加会被逐字折成一列单字')
})

test('不可逆的动作不画成视觉上最重的那一个', () => {
  // 「下线」是集群管理里唯一不可逆的动作。拦住误点的是二次确认，不是颜色，
  // 但也不该反过来用颜色去吸引点击。
  const src = read('ClustersPage.tsx')
  const at = src.indexOf('offboard(c.id)')
  assert.ok(at > 0, '找不到下线按钮')
  const around = src.slice(at, at + 240)
  assert.doesNotMatch(around, /variant="primary"/,
    '下线按钮用了主操作样式')
})

test('表格单元格里不放整段散文', () => {
  // 一段三行的说明塞进单元格，会把那一行撑到相邻行的六倍高，整张表就读不
  // 成一张表了。说明必须在，但要折起来。
  const src = read('ClustersPage.tsx')
  const cell = src.slice(src.indexOf('function GitBindingCell'))
  assert.match(cell.slice(0, 2000), /<Disclosure/,
    'Git 绑定那一格仍然把整段说明摊在表格里')
})
