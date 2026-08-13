import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { join } from 'node:path'

import {
  HOST_KEYS_AFTER, NUMERIC_FIELDS, RESTART_NOTICE, buildSettingsWrite,
  restartRequiredLabels, settingsDiff, settingsFormValuesOf,
} from '../src/pages/settingsForm.ts'
import type { SettingsFormValues } from '../src/pages/settingsForm.ts'
import type { PlatformSettingView } from '../src/api/types.ts'

/** 一段看起来像真 known_hosts 的原文。断言"它没出现在任何地方"时要用它。 */
const HOST_KEYS =
  'gitlab.example.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIKyjWioKIYrbPTzY9F8JKIElwSThZ4xuqtqQPGo9tDIg'

/** 与线上 GET /api/v1/settings 实测形状一致的一份设置。 */
function view(over: Partial<PlatformSettingView> = {}): PlatformSettingView {
  return {
    sessionTtlSeconds: 28800,
    httpReadTimeoutMs: 10000,
    httpWriteTimeoutMs: 20000,
    httpShutdownTimeoutMs: 15000,
    secretsBackend: 'NONE',
    secretsProject: '',
    secretsPrefix: '',
    secretsDir: '',
    gitVerifyTimeoutMs: 10000,
    gitVerifyHostKeysFingerprint: '',
    ...over,
  }
}

function values(v: PlatformSettingView, over: Partial<SettingsFormValues> = {}): SettingsFormValues {
  return { ...settingsFormValuesOf(v), ...over }
}

/* ---------------------------------------------------------------------- */
/* 1. host key 只入不出                                                     */
/* ---------------------------------------------------------------------- */

test('播种表单时 host key 输入框恒为空，指纹也不会被填进去', () => {
  const current = view({ gitVerifyHostKeysFingerprint: 'SHA256:abcdEFGH1234' })
  const seeded = settingsFormValuesOf(current)

  assert.equal(seeded.hostKeysInput, '',
    '输入框被预填了内容——预填的任何东西都会在下一次保存时被当成新的 host key 原样写进库')

  // 指纹哪怕被填进任何一个字段，保存时都会被原样提交成某一项设置。
  // 落到 hostKeysInput 上尤其致命：信任锚会被换成一串摘要文本，
  // 此后每一次 Git 校验都连不上，而界面上一切正常。
  for (const [k, val] of Object.entries(seeded)) {
    assert.notEqual(val, current.gitVerifyHostKeysFingerprint,
      `字段 ${k} 装着当前指纹——指纹是用来核对的，不是用来回填的`)
  }
})

test('设置的读取形状里根本没有 host key 原文的位置', () => {
  // 这一条钉的是类型形状而不是某次调用：只要 PlatformSettingView 上多出
  // 一个承载原文的字段，页面就多了一条能把 host key 显示出来的路径，
  // 而那条路径不会有任何编译错误提醒它的存在（规范 §19、§20）。
  const seeded: Record<string, unknown> = { ...view() }
  assert.equal(Object.hasOwn(seeded, 'gitVerifyHostKeys'), false,
    '读取形状带上了 gitVerifyHostKeys —— 服务端不回它，前端也不该有它的位置')
})

test('host key 输入框留空时不提交清空：库里已装着信任锚就拦下这次保存', () => {
  const current = view({ gitVerifyHostKeysFingerprint: 'SHA256:abcdEFGH1234' })
  // 操作者只想改 Git 校验超时，压根没打算碰信任锚。
  const built = buildSettingsWrite(values(current, { gitVerifyTimeoutMs: '20000' }), current)

  assert.equal(built.ok, false,
    '留空的 host key 被提交了出去——服务端的保存是整行替换，这一次提交会把 SSH 信任锚清空，'
    + '此后所有 Git 校验都无法进行，而操作者以为自己只改了一个超时')
  if (built.ok) return
  assert.match(built.error, /信任锚|host key/,
    '拦下了却没说清拦的是什么，操作者只会以为页面坏了')
})

test('从未配过 host key 时留空照常提交空串：那不是一次清空', () => {
  const current = view({ gitVerifyHostKeysFingerprint: '' })
  const built = buildSettingsWrite(values(current, { gitVerifyTimeoutMs: '20000' }), current)

  assert.equal(built.ok, true,
    '库里本来就没有 host key，写空串不改变任何事实；拦下它只会让平台连超时都改不了')
  if (!built.ok) return
  assert.equal(built.body.gitVerifyHostKeys, '')
})

test('输入框填了原文就照原文提交', () => {
  const current = view({ gitVerifyHostKeysFingerprint: 'SHA256:old' })
  const built = buildSettingsWrite(values(current, { hostKeysInput: `  ${HOST_KEYS}\n` }), current)

  assert.equal(built.ok, true)
  if (!built.ok) return
  assert.equal(built.body.gitVerifyHostKeys, HOST_KEYS)
})

/* ---------------------------------------------------------------------- */
/* 2. 后端与字段互相印证（镜像 registry.validateSecretsBackendFields）        */
/* ---------------------------------------------------------------------- */

test('凭据后端与其字段必须互相印证，且这道检查在提交路径上真的被走到', () => {
  const current = view()
  const cases: [string, Partial<SettingsFormValues>, boolean][] = [
    ['NONE 三项全空', { secretsBackend: 'NONE' }, true],
    ['NONE 却留着 project', { secretsBackend: 'NONE', secretsProject: 'p' }, false],
    ['NONE 却留着 dir', { secretsBackend: 'NONE', secretsDir: '/var/secrets' }, false],
    ['DIR 填了 dir', { secretsBackend: 'DIR', secretsDir: '/var/secrets' }, true],
    ['DIR 却没填 dir', { secretsBackend: 'DIR' }, false],
    // 这一格是 brief 第一条风险的原型：进程正常起来、校验正常出结论，
    // 只是身份来源读成了本地目录，没有任何症状会暴露它。
    ['DIR 却同时填了 project', { secretsBackend: 'DIR', secretsDir: '/var/secrets', secretsProject: 'p' }, false],
    ['SECRET_MANAGER 项目与前缀都填', { secretsBackend: 'SECRET_MANAGER', secretsProject: 'p', secretsPrefix: 'distill-' }, true],
    ['SECRET_MANAGER 少了前缀', { secretsBackend: 'SECRET_MANAGER', secretsProject: 'p' }, false],
    ['SECRET_MANAGER 却填了 dir', { secretsBackend: 'SECRET_MANAGER', secretsProject: 'p', secretsPrefix: 'd-', secretsDir: '/var/secrets' }, false],
  ]

  for (const [name, over, want] of cases) {
    // 走的是 buildSettingsWrite 而不是那个私有的检查函数：这里同时问两件
    // 事——检查本身对不对，以及提交路径是不是还在调用它。只测检查函数，
    // 把它从 buildSettingsWrite 里删掉也照样全绿。
    const built = buildSettingsWrite(values(current, over), current)
    assert.equal(built.ok, want, `${name}：期望 ok=${want}`)
  }
})

test('超时与有效期必须为正整数', () => {
  const current = view()
  for (const bad of ['0', '-1', '', '  ', '1.5', '10s', 'abc']) {
    const built = buildSettingsWrite(values(current, { httpReadTimeoutMs: bad }), current)
    assert.equal(built.ok, false, `httpReadTimeoutMs=${JSON.stringify(bad)} 被放过了`)
  }
  // 五项都要被读到：漏掉一项就等于那个输入框存在却从不被提交。
  for (const f of NUMERIC_FIELDS) {
    const built = buildSettingsWrite(values(current, { [f.key]: '0' }), current)
    assert.equal(built.ok, false, `${f.key} 填 0 被放过了——0 不是"不限制"`)
  }
})

/* ---------------------------------------------------------------------- */
/* 3. 保存前的差异                                                          */
/* ---------------------------------------------------------------------- */

test('差异只列改动项，并给出前后值', () => {
  const current = view()
  const built = buildSettingsWrite(values(current, { gitVerifyTimeoutMs: '20000' }), current)
  assert.equal(built.ok, true)
  if (!built.ok) return

  const rows = settingsDiff(current, built.body)
  assert.deepEqual(rows.map((r) => r.label), ['Git 校验超时（毫秒）'],
    '没改的项也被列进差异，或改了的项没被列出来')
  assert.equal(rows[0].before, '10000')
  assert.equal(rows[0].after, '20000')
})

test('表单与库里一致时差异为空', () => {
  const current = view()
  const built = buildSettingsWrite(values(current), current)
  assert.equal(built.ok, true)
  if (!built.ok) return
  assert.deepEqual(settingsDiff(current, built.body), [])
})

test('差异表永远不渲染 host key 原文，只说明它会被换掉', () => {
  const current = view({ gitVerifyHostKeysFingerprint: 'SHA256:old' })
  const built = buildSettingsWrite(values(current, { hostKeysInput: HOST_KEYS }), current)
  assert.equal(built.ok, true)
  if (!built.ok) return

  const rows = settingsDiff(current, built.body)
  const row = rows.find((r) => r.label.includes('host key'))
  assert.ok(row, 'host key 被换掉了却没出现在差异里——这是本页最需要被看见的一次改动')

  // 差异区是会被截图、被贴进工单的一块界面。原文进了这里，服务端拒绝
  // 回显 host key 的那份克制就在最后一步被前端抵消掉了（规范 §19、§20）。
  assert.equal(JSON.stringify(rows).includes(HOST_KEYS), false,
    '差异行里出现了 host key 原文')
  assert.equal(row.before, 'SHA256:old')
  assert.equal(row.after, HOST_KEYS_AFTER)
})

test('没提交 host key 时差异里没有 host key 那一行', () => {
  const current = view()
  const built = buildSettingsWrite(values(current, { gitVerifyTimeoutMs: '20000' }), current)
  assert.equal(built.ok, true)
  if (!built.ok) return
  assert.equal(settingsDiff(current, built.body).some((r) => r.label.includes('host key')), false,
    '凭空多出一条"信任锚要变了"，操作者会以为自己碰了不该碰的东西')
})

/* ---------------------------------------------------------------------- */
/* 4. 启动时读一次的那四项                                                   */
/* ---------------------------------------------------------------------- */

test('四项启动时读取的设置标为重启后生效，Git 校验超时不在其列', () => {
  const effects = new Map(NUMERIC_FIELDS.map((f) => [f.key, f.effect]))
  for (const key of [
    'sessionTtlSeconds', 'httpReadTimeoutMs', 'httpWriteTimeoutMs', 'httpShutdownTimeoutMs',
  ] as const) {
    assert.equal(effects.get(key), 'AFTER_RESTART',
      `${key} 被标成立刻生效——它被 http.Server / 会话存储在构造时收进内部（design doc §1.1），`
      + '说它立刻生效是一句会被相信的假话')
  }
  assert.equal(effects.get('gitVerifyTimeoutMs'), 'IMMEDIATE',
    'Git 校验在使用处现读设置，把它也说成要重启会让人白重启一次')
})

test('改了要重启的项，差异里逐项标出来并能列出名字', () => {
  const current = view()
  const built = buildSettingsWrite(
    values(current, { sessionTtlSeconds: '3600', gitVerifyTimeoutMs: '20000' }), current)
  assert.equal(built.ok, true)
  if (!built.ok) return

  const rows = settingsDiff(current, built.body)
  assert.deepEqual(restartRequiredLabels(rows), ['会话有效期（秒）'],
    '要重启的项没被挑出来，或把立刻生效的项也算了进去')

  const ttl = rows.find((r) => r.label.startsWith('会话有效期'))
  assert.equal(ttl?.effect, 'AFTER_RESTART')
  const gitTimeout = rows.find((r) => r.label.startsWith('Git 校验超时'))
  assert.equal(gitTimeout?.effect, 'IMMEDIATE')
})

test('只改了立刻生效的项时不列重启项', () => {
  const current = view()
  const built = buildSettingsWrite(values(current, { gitVerifyTimeoutMs: '20000' }), current)
  assert.equal(built.ok, true)
  if (!built.ok) return
  assert.deepEqual(restartRequiredLabels(settingsDiff(current, built.body)), [])
})

test('重启提示说清了"要等重启"，不是含糊其辞', () => {
  assert.match(RESTART_NOTICE, /重启/)
  assert.equal(/可能|也许|通常|一般/.test(RESTART_NOTICE), false,
    '含糊的措辞会让操作者以为刷新一下页面就好')
})

/* ---------------------------------------------------------------------- */
/* 5. 调用点还在调用                                                        */
/* ---------------------------------------------------------------------- */

/*
 * 上面每一条测的都是纯逻辑层。它们全绿并不能证明 SettingsPage.tsx 还在用
 * 这一层——本项目上一次前端事故正是这个形状：tile 读一份 report、下面的
 * 表读另一份，tsc / oxlint / vite build 三关全过，因为没有任何一个编译期
 * 检查能绑定"某个组件读的是哪个字段"。
 *
 * 这个仓库的前端测试是 `node --test` 直接跑 TS 模块，没有 DOM、没有 React
 * 测试渲染器，且本任务不得新增任何依赖 —— 因此能用来绑定调用点的手段只
 * 剩下读源码文本。它挡不住"调用了但渲染错了地方"，只挡得住"整段被删掉或
 * 改成另写一份"。这条局限写在这里，不要把它当成"页面已被测试覆盖"。
 */
const PAGE_SOURCE = readFileSync(
  join(import.meta.dirname, '..', 'src', 'pages', 'SettingsPage.tsx'), 'utf8')

test('设置页仍然从纯逻辑层拿提交体、差异与重启提示', () => {
  for (const symbol of [
    'buildSettingsWrite',   // 提交体与那两道校验
    'settingsDiff',         // 保存前的差异
    'restartRequiredLabels', // 哪几项要重启
    'RESTART_NOTICE',       // 重启这件事说给操作者听
    'settingsFormValuesOf', // 播种（host key 输入框恒空由它保证）
    'NUMERIC_FIELDS',       // 输入框与差异共用同一张字段表
  ]) {
    assert.ok(PAGE_SOURCE.includes(symbol),
      `SettingsPage.tsx 不再引用 ${symbol}——纯逻辑层测得再全，页面已经不走它了`)
  }
})

test('设置页提交的是 buildSettingsWrite 的产物，不是自己拼的请求体', () => {
  assert.match(PAGE_SOURCE, /api\.updateSettings\(built\.body\)/,
    '保存路径提交的不是 buildSettingsWrite 算出来的那份 body——'
    + '页面自己拼一份，就等于绕开了 host key 留空与凭据后端两道检查')
})

test('设置页把 host key 标成信任锚，且没有任何地方回显原文', () => {
  assert.ok(PAGE_SOURCE.includes('信任锚'),
    'host key 区域没有把它是什么说出来——一个只写着 host key 的输入框会被当成连接参数，'
    + '而改它的人是在决定平台愿意和哪台 SSH 服务器说话（design doc §1.3）')
  assert.equal(/gitVerifyHostKeys\b(?!Fingerprint)/.test(PAGE_SOURCE), false,
    '页面直接碰了 host key 原文字段——它只应该出现在 buildSettingsWrite 算出的 body 里')
})
