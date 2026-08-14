import { readFileSync } from 'node:fs'
import test from 'node:test'
import assert from 'node:assert/strict'

import {
  BOOTSTRAP_LOCKOUT_WARNING, LAST_ADMIN_REFUSAL, accountStatusLabel, blankNewAccount,
  blankOwnPassword, blankResetPassword, enabledAdmins, previewAccountAction, resolveNewAccount,
  resolveOwnPassword, resolveResetPassword, roleLabel, showsAccountAdminEntry,
  signedInViaBootstrap, type AccountActionKind,
} from '../src/pages/accountForm.ts'
import type { Account, Role } from '../src/api/types.ts'

/** 六个动作的完整清单。少一个，下面那几条遍历就漏测一个动作。 */
const ALL_ACTIONS: AccountActionKind[] =
  ['PROMOTE', 'DEMOTE', 'DISABLE', 'ENABLE', 'DELETE', 'RESET_PASSWORD']

function account(username: string, role: Role, disabledAt: string | null = null): Account {
  return {
    username, role, disabledAt,
    createdAt: '2026-08-14T01:02:03Z',
    updatedAt: '2026-08-14T04:05:06Z',
  }
}

const viewer = account('alice', 'VIEWER')
const admin = account('root', 'ADMIN')

/* ---------------------------------------------------------------------- */
/* 1. 角色可见性                                                            */
/* ---------------------------------------------------------------------- */

/**
 * 双向：管理员看得到入口，只读看不到。
 *
 * 只测一个方向时，一个恒真（或恒假）的实现同样是绿的 —— 恒真会把管理员
 * 入口渲染给只读账号，恒假会让管理员自己也进不去自己的页面。
 */
test('账号管理入口只对管理员渲染，只读账号看不到', () => {
  assert.equal(showsAccountAdminEntry('ADMIN'), true, '管理员看不到自己的账号管理入口')
  assert.equal(showsAccountAdminEntry('VIEWER'), false,
    '只读账号看到了管理员入口：那一点省不掉的是「点了才发现被拒」，但界面不该主动骗人')
  assert.equal(showsAccountAdminEntry('SUPERUSER' as Role), false,
    '界面不认识的角色被当成了管理员：失败方向必须朝关的那边')
})

/**
 * 角色文案不许留空，也不许把裸枚举甩在屏幕上。
 *
 * 一格空白会被读成「这个账号没有角色」，而未登记的取值显示成一个像模像样
 * 的角色名，读的人会以为平台承认这个角色。
 */
test('角色文案非空、互不相同，未登记的取值明说自己未登记', () => {
  assert.notEqual(roleLabel('ADMIN').trim(), '')
  assert.notEqual(roleLabel('VIEWER').trim(), '')
  assert.notEqual(roleLabel('ADMIN'), roleLabel('VIEWER'))
  assert.equal(roleLabel('ADMIN'), '管理员')

  const unknown = roleLabel('OWNER' as Role)
  assert.notEqual(unknown.trim(), '')
  assert.match(unknown, /未登记/)
})

/* ---------------------------------------------------------------------- */
/* 2. 引导账号：在它失效之前说清楚                                            */
/* ---------------------------------------------------------------------- */

/**
 * 警告文案必须把三件事都说到：现在用的是引导账号、它什么时候失效、
 * 以及操作者该先做什么。
 *
 * 少掉最后一句，这条警告就退化成一句"注意"，读完的人不知道该干什么。
 */
test('引导账号警告说清了"什么时候失效"和"先确认密码"', () => {
  assert.match(BOOTSTRAP_LOCKOUT_WARNING, /引导账号/)
  assert.match(BOOTSTRAP_LOCKOUT_WARNING, /失效/)
  assert.match(BOOTSTRAP_LOCKOUT_WARNING, /管理员/)
  assert.match(BOOTSTRAP_LOCKOUT_WARNING, /密码/,
    '没有告诉操作者"先确认你知道那个账号的密码"：那正是把他关在门外的那一步')
  assert.match(BOOTSTRAP_LOCKOUT_WARNING, /登录/)
})

/**
 * 「我是不是用引导账号登进来的」按账号表反推，与服务端 auditBootstrapLogin 同源。
 *
 * 双向：在表里 → 不是引导账号；不在表里 → 是。列表还没到手时不下结论。
 */
test('账号表里查不到自己，才判定为引导账号', () => {
  assert.equal(signedInViaBootstrap('admin', []), true,
    '账号表是空的却没判成引导账号：那正是首次启动时的样子，警告会在最该出现时消失')
  assert.equal(signedInViaBootstrap('admin', [account('admin', 'ADMIN')]), false,
    '自己就在账号表里，却被当成引导账号')
  assert.equal(signedInViaBootstrap('admin', [viewer]), true)
  assert.equal(signedInViaBootstrap('admin', null), false, '列表还没到手就下了结论')
})

/** 启用中的管理员计数：停用的与只读的都不算。它为空即引导口开着。 */
test('启用中的管理员计数排除停用的与只读的', () => {
  assert.equal(enabledAdmins([]).length, 0)
  assert.equal(enabledAdmins([viewer]).length, 0)
  assert.equal(enabledAdmins([account('root', 'ADMIN', '2026-08-14T06:00:00Z')]).length, 0,
    '已停用的管理员被算进了启用中的管理员：引导口其实是开着的')
  assert.equal(enabledAdmins([admin, viewer]).length, 1)
  assert.equal(enabledAdmins([admin, account('ops', 'ADMIN')]).length, 2)
})

/* ---------------------------------------------------------------------- */
/* 3. 危险操作在执行之前把后果写出来                                          */
/* ---------------------------------------------------------------------- */

/** 六个动作各有非空动词与非空说明，且说明点名账号 —— 按钮上看它们长得一样。 */
test('六个动作各有非空动词与说明，且说明点名了账号', () => {
  const verbs = new Set<string>()
  for (const kind of ALL_ACTIONS) {
    const p = previewAccountAction(kind, viewer, [viewer, admin])
    assert.notEqual(p.verb.trim(), '', `${kind} 没有动词`)
    assert.notEqual(p.change.trim(), '', `${kind} 没有说明会发生什么`)
    assert.match(p.change, /alice/, `${kind} 的说明没有点名是哪个账号`)
    verbs.add(p.verb)
  }
  assert.equal(verbs.size, ALL_ACTIONS.length, '有两个动作共用同一个动词，界面上分不开')
})

/**
 * 提升为管理员是引导账号失效的那一刻，警告必须挂在这个动作上。
 *
 * 这是本页最要紧的一条：操作者点下去之前必须读到它，事后解释没有意义 ——
 * 那时他已经进不来了（design doc 2026-08-14 §2）。
 */
test('库里没有启用中的管理员时，"提升为管理员"带引导账号失效警告', () => {
  const p = previewAccountAction('PROMOTE', viewer, [viewer])
  assert.notEqual(p.bootstrap.trim(), '',
    '提升为第一个管理员却没有任何警告：操作者会在下一次登录时被关在平台外面')
  assert.match(p.bootstrap, /引导账号/)
  assert.match(p.bootstrap, /失效/)
  assert.match(p.bootstrap, /密码/)
  assert.match(p.bootstrap, /alice/, '警告没说清是哪一次操作触发的')
})

/** 重新启用一个已停用的管理员同样会关上引导口 —— 少了它就是一条漏网的路径。 */
test('启用一个已停用的管理员同样带引导账号失效警告', () => {
  const disabledAdmin = account('root', 'ADMIN', '2026-08-14T06:00:00Z')
  const p = previewAccountAction('ENABLE', disabledAdmin, [disabledAdmin, viewer])
  assert.match(p.bootstrap, /引导账号/,
    '重新启用管理员这条路径没有警告：它与"提升为管理员"造成的是同一件事')

  // 启用一个只读账号不改变引导口的状态，不该乱警告：到处都是警告等于没有警告。
  const disabledViewer = account('bob', 'VIEWER', '2026-08-14T06:00:00Z')
  assert.equal(previewAccountAction('ENABLE', disabledViewer, [disabledViewer]).bootstrap, '')
})

/** 引导口已经关上时不再重复警告：那件事已经发生过了。 */
test('已经有启用中的管理员时不再报引导账号失效', () => {
  for (const kind of ALL_ACTIONS) {
    assert.equal(previewAccountAction(kind, viewer, [viewer, admin]).bootstrap, '',
      `${kind} 在引导口已关时仍在报失效警告：真正要紧的那次警告会被淹掉`)
  }
})

/**
 * 动最后一个启用中的管理员，服务端会拒绝，界面提前说出来。
 *
 * 三种动法都要说（降级、停用、删除）：只挡住一种，另外两条路径上操作者
 * 仍然要点完才发现（design doc §5）。
 */
test('动最后一个启用中的管理员时提前说明服务端会拒绝', () => {
  const only = [admin, viewer]
  for (const kind of ['DEMOTE', 'DISABLE', 'DELETE'] as AccountActionKind[]) {
    const p = previewAccountAction(kind, admin, only)
    assert.equal(p.refusal, LAST_ADMIN_REFUSAL, `${kind} 没有提前说明服务端会拒绝`)
    assert.match(p.refusal, /最后一个/)
    assert.match(p.refusal, /管理员/)
  }
  // 重置最后一个管理员的密码不会让平台失去管理员，不该假报一次拒绝：
  // 一句假的"服务端会拒绝"会让操作者放弃一个其实做得到的操作。
  assert.equal(previewAccountAction('RESET_PASSWORD', admin, only).refusal, '')
})

/** 还有第二个启用中的管理员时不报拒绝 —— 那一次服务端确实会放行。 */
test('还有别的启用中的管理员时不报拒绝', () => {
  const two = [admin, account('ops', 'ADMIN'), viewer]
  for (const kind of ALL_ACTIONS) {
    assert.equal(previewAccountAction(kind, admin, two).refusal, '',
      `${kind} 在还有第二个管理员时假报了一次拒绝`)
  }
  // 只读账号怎么动都不会触发这条保护。
  for (const kind of ALL_ACTIONS) {
    assert.equal(previewAccountAction(kind, viewer, [viewer, admin]).refusal, '')
  }
})

/** 删除要说清是软删除、用户名不回收：否则操作者会以为可以再建一个同名的。 */
test('删除说明写清了软删除与用户名不回收', () => {
  const p = previewAccountAction('DELETE', viewer, [viewer, admin])
  assert.match(p.change, /软删除/)
  assert.match(p.change, /审计/)
  assert.match(p.change, /用户名/)
})

/** 状态一律有字，启用中也不留空 —— 空单元格会被读成「加载中」。 */
test('账号状态非空，停用的带上停用时刻', () => {
  assert.equal(accountStatusLabel(viewer), '启用中')
  const stopped = accountStatusLabel(account('bob', 'VIEWER', '2026-08-14T06:07:08Z'))
  assert.match(stopped, /已停用/)
  assert.match(stopped, /2026-08-14 06:07:08 UTC/)
})

/* ---------------------------------------------------------------------- */
/* 4. 三份提交体：密码只往一个方向走                                          */
/* ---------------------------------------------------------------------- */

/**
 * 改自己的密码**必须把当前密码发出去**（规范 §28，design doc §6）。
 *
 * 两条断言缺一不可：键集合证明它在请求体里，值断言证明发的是操作者输入的
 * 那一个而不是一个空串。少掉后者，一个 `currentPassword: ''` 的实现照样绿。
 */
test('改自己的密码，提交体里带着当前密码', () => {
  const resolved = resolveOwnPassword({
    currentPassword: 'old-secret-12ch', newPassword: 'new-secret-12ch', confirm: 'new-secret-12ch',
  })
  assert.equal(resolved.ok, true)
  if (!resolved.ok) return
  assert.deepEqual(Object.keys(resolved.body).sort(), ['currentPassword', 'newPassword'],
    '提交体的字段变了：少了 currentPassword，一张被捡到的会话就能改走账号')
  assert.equal(resolved.body.currentPassword, 'old-secret-12ch',
    '当前密码没有原样发出去')
  assert.equal(resolved.body.newPassword, 'new-secret-12ch')
})

/** 当前密码空着就拦住：让它发出去只会换回一句"密码不正确"，读者会以为自己记错了。 */
test('当前密码留空被拦住，且理由说清为什么非要它不可', () => {
  const resolved = resolveOwnPassword({
    currentPassword: '', newPassword: 'new-secret-12ch', confirm: 'new-secret-12ch',
  })
  assert.equal(resolved.ok, false, '当前密码空着也放行：这条要求是 §28 明写的')
  if (resolved.ok) return
  assert.match(resolved.error, /当前密码/)
  assert.match(resolved.error, /会话/)
})

/** 两次输入不一致必须当场拦住：密码不回显，事后无从核对。 */
test('新密码两次输入不一致时三个表单都拦住', () => {
  const own = resolveOwnPassword({
    currentPassword: 'old-secret-12ch', newPassword: 'aaaaaaaaaaaa', confirm: 'bbbbbbbbbbbb',
  })
  assert.equal(own.ok, false)

  const created = resolveNewAccount({
    username: 'alice', password: 'aaaaaaaaaaaa', confirm: 'bbbbbbbbbbbb',
  })
  assert.equal(created.ok, false)

  const reset = resolveResetPassword({ password: 'aaaaaaaaaaaa', confirm: 'bbbbbbbbbbbb' })
  assert.equal(reset.ok, false)
  if (reset.ok) return
  assert.match(reset.error, /不一致/)
})

/**
 * 新建账号的提交体里**没有 role**。
 *
 * 服务端固定把新账号建成只读，请求体里的角色不会被采纳；带一个上去，
 * 操作者会以为自己刚建出了一个管理员，而提权必须是一次单独的、有自己
 * 审计行的操作（design doc §6）。断言整个键集合，不是逐个 hasOwn ——
 * 后者只挡得住已经想到的那几个拼法。
 */
test('新建账号的提交体只有用户名与密码，没有 role', () => {
  const resolved = resolveNewAccount({
    username: '  alice  ', password: 'a-secret-1234', confirm: 'a-secret-1234',
  })
  assert.equal(resolved.ok, true)
  if (!resolved.ok) return
  assert.deepEqual(Object.keys(resolved.body).sort(), ['password', 'username'],
    '提交体多出了字段：一个不会被采纳的 role 会让人以为自己建出了管理员')
  assert.equal(resolved.body.username, 'alice', '用户名两端的空白没有去掉')
  assert.match(resolved.summary, /只读/)
  assert.match(resolved.summary, /引导账号/,
    '建号回执没提"提权那一步才是引导账号失效的时刻"')
})

/** 管理员重置他人密码不带当前密码：管理员并不知道对方的密码。 */
test('重置他人密码的提交体里只有新密码', () => {
  const resolved = resolveResetPassword({ password: 'a-secret-1234', confirm: 'a-secret-1234' })
  assert.equal(resolved.ok, true)
  if (!resolved.ok) return
  assert.deepEqual(Object.keys(resolved.body), ['password'])
  assert.equal(resolved.body.password, 'a-secret-1234')
})

/**
 * 前端不复述服务端的密码长度规则。
 *
 * 下限按字符（12）、上限按字节（72）是两个不同的单位，抄一份必然漂开，
 * 而漂开的表现是一个后端收得下、界面却不让提交的死角（规范 §34，同
 * gitRepoForm 对 SSH 形态那条的处置）。三个表单都要放行，交给服务端点名。
 */
test('长度规则交给服务端判定，前端不自己拦', () => {
  const short = 'abc'
  assert.equal(resolveNewAccount({ username: 'a', password: short, confirm: short }).ok, true,
    '前端自己复述了密码长度规则：那条规则的准确措辞只有服务端有')
  assert.equal(resolveResetPassword({ password: short, confirm: short }).ok, true)
  assert.equal(resolveOwnPassword(
    { currentPassword: 'x', newPassword: short, confirm: short },
  ).ok, true)
})

/** 空表单一律是全空串：一个带着上次输入的初值就是一次回显。 */
test('三个空表单的初值全是空串', () => {
  assert.deepEqual(blankNewAccount(), { username: '', password: '', confirm: '' })
  assert.deepEqual(blankOwnPassword(), { currentPassword: '', newPassword: '', confirm: '' })
  assert.deepEqual(blankResetPassword(), { password: '', confirm: '' })
})

/* ---------------------------------------------------------------------- */
/* 5. 界面确实按这些规则接线                                                  */
/* ---------------------------------------------------------------------- */

const PAGE_SRC = readFileSync(new URL('../src/pages/AccountsPage.tsx', import.meta.url), 'utf8')
const SHELL_SRC = readFileSync(new URL('../src/components/AppShell.tsx', import.meta.url), 'utf8')
const CLIENT_SRC = readFileSync(new URL('../src/api/client.ts', import.meta.url), 'utf8')
const FORM_SRC = readFileSync(new URL('../src/pages/accountForm.ts', import.meta.url), 'utf8')
const SESSION_SRC = readFileSync(new URL('../src/auth/SessionContext.tsx', import.meta.url), 'utf8')

/*
 * 从这里往下的断言读的是**源码文本**，不是渲染结果。
 *
 * `node --test` 的类型擦除读不了 JSX，这个仓库也没有 DOM 测试设施（且本轮
 * 不新增依赖），组件因此挂不起来。于是「界面真的按上面那些规则接线」只能
 * 靠文本绑定：它抓得住「这一页根本没调那个函数」，**抓不住**「调了但把结果
 * 渲染到看不见的地方」，也抓不住「把调用挪进一个本页自己写的同名 helper」。
 *
 * 这个局限是真的。tsc / lint / build 三道门禁一道都覆盖不到它，本文件也
 * 只覆盖到文本这一层 —— 不该假装它们能。上一轮这个仓库的前端缺陷正是这个
 * 形状：一个组件渲染的报告和它的标题说的不是同一件事，四道门禁全绿。
 */

/** 上面那一整组纯函数断言，只有在页面真的调用它们时才管得到界面。 */
test('账号页调用了折算层，六个动作也都接上了端点', () => {
  for (const call of ['previewAccountAction(', 'resolveNewAccount(', 'resolveResetPassword(',
    'showsAccountAdminEntry(', 'signedInViaBootstrap(', 'accountStatusLabel(', 'roleLabel(']) {
    assert.equal(PAGE_SRC.includes(call), true,
      `账号页没有调用 ${call}，上面那组断言就管不到界面`)
  }
  for (const call of ['api.accounts(', 'api.createAccount(', 'api.updateAccountRole(',
    'api.disableAccount(', 'api.enableAccount(', 'api.deleteAccount(',
    'api.resetAccountPassword(']) {
    assert.equal(PAGE_SRC.includes(call), true, `账号页没有接上 ${call}`)
  }
})

/**
 * 引导账号的警告必须真的出现在页面上，而不是只躺在文案表里。
 *
 * 两处都要：进页面就看得见的那一条（操作发生之前），以及确认面板里
 * 针对那一次操作的那一条（preview.bootstrap）。
 */
test('账号页渲染了引导账号警告，进页面就看得见', () => {
  assert.equal(PAGE_SRC.includes('BOOTSTRAP_LOCKOUT_WARNING'), true,
    '账号页没有渲染引导账号警告：那句话必须在操作发生之前就在屏幕上')
  assert.match(PAGE_SRC, /viaBootstrap && <Notice>\{BOOTSTRAP_LOCKOUT_WARNING\}<\/Notice>/,
    '常量被引用了，但没有渲染成一条进页面就看得见的提示')
  assert.equal(PAGE_SRC.includes('preview.bootstrap'), true,
    '确认面板没有展示这一次操作会不会让引导账号失效')
  assert.equal(PAGE_SRC.includes('preview.refusal'), true,
    '确认面板没有提前展示服务端会拒绝的理由')
  assert.equal(PAGE_SRC.includes('preview.change'), true,
    '确认面板没有说清这一次会把哪个账号变成什么')
})

/**
 * 管理员专属内容要挂在角色判断后面，且那段注释必须说清它不是安全边界。
 *
 * 注释里的这几个字是这条纪律唯一能被检查到的形状：删掉守卫的人会顺手
 * 删掉注释，而写下一个类似守卫的人会照着抄这段话（规范 §34）。
 */
test('管理员入口挂在角色判断后面，且注释写明它不是安全边界', () => {
  assert.equal(SHELL_SRC.includes('showsAccountAdminEntry(identity.role)'), true,
    '导航没有按角色过滤账号管理入口')
  assert.match(SHELL_SRC, /showsAccountAdminEntry\(identity\.role\)[\s\S]{0,120}?'\/accounts'/,
    '账号管理入口没有挂在角色判断后面：只读账号也会看到它')
  assert.equal((SHELL_SRC.match(/'\/accounts'/g) ?? []).length, 1,
    '账号管理入口在导航里出现了不止一次，其中一处可能没有被角色判断罩住')
  assert.match(SHELL_SRC, /'\/me\/password'/,
    '导航里没有改密入口：任何角色都要能改自己的密码，而只读账号没有别的入口')

  assert.match(PAGE_SRC, /showsAccountAdminEntry\(identity\.role\)[\s\S]{0,80}?AdminOnlyNotice/,
    '账号页没有在角色判断后面才渲染管理内容')

  for (const src of [SHELL_SRC, FORM_SRC]) {
    assert.match(src, /不是安全/,
      '按角色隐藏的注释没有写明它不是安全边界：下一个读到的人会以为隐藏就是保护')
    assert.match(src, /服务端/,
      '注释没有说明服务端已经拒绝了：那才是真正拦住请求的东西')
  }
})

/** 改密页对任何角色都可用，因此它不能被任何角色判断罩住。 */
test('改密页不在任何角色判断里', () => {
  assert.match(PAGE_SRC, /export function OwnPasswordPage/)
  assert.equal(PAGE_SRC.includes('api.changeOwnPassword('), true, '改密页没有接上端点')
  assert.equal(PAGE_SRC.includes('resolveOwnPassword('), true,
    '改密页没有走折算层：那条"当前密码必填"的断言就管不到界面')
  const own = PAGE_SRC.slice(PAGE_SRC.indexOf('export function OwnPasswordPage'))
  assert.equal(own.includes('showsAccountAdminEntry'), false,
    '改密页被角色判断罩住了：只读账号会因此改不了自己的密码')
})

/**
 * 明文密码只往一个方向走：输入框 → 请求体。
 *
 * 三条出口逐一钉死 —— 屏幕（回显）、本地存储、URL。前两条在页面上查，
 * 第三条在客户端查：只有 client.ts 拼得出 URL。
 */
test('密码不回显、不进本地存储、不进 URL', () => {
  // 全部密码输入框走同一个组件，且它把 type="password" 写死。
  assert.match(PAGE_SRC, /type="password"/, '密码输入框没有设成密码类型，值会明文显示在屏幕上')
  assert.equal(PAGE_SRC.includes('type={'), false,
    '有输入框的类型是算出来的：一个可切换成明文的密码框迟早会被切开')
  // 逐个数 <input> 标签，而不是数整份源码里 type="password" 出现了几次：
  // 后者连注释里那一处也会数进去，于是多出一个明文输入框反而看不出来。
  const inputs = PAGE_SRC.match(/<input[\s\S]*?\/>/g) ?? []
  const plain = inputs.filter((tag) => !tag.includes('type="password"'))
  assert.equal(plain.length, 1,
    '除用户名之外还有别的裸输入框：密码一律要走 PasswordField')
  assert.match(plain[0], /autoComplete="off"/, '那个裸输入框不是用户名那一格')

  // 匹配的是**使用**（后面跟着 . 或 [ 或 =），不是注释里提到这几个名字：
  // 上面那些注释正是在写"不许往这里放"，把它们一起判红，这条断言就只能
  // 靠删注释来通过。
  for (const src of [PAGE_SRC, FORM_SRC, CLIENT_SRC, SESSION_SRC]) {
    assert.equal(/\b(?:local|session)Storage\s*[.[]/.test(src), false,
      '密码路径上用到了本地存储：明文一旦落进去，登出与会话过期都清不掉它')
    assert.equal(/\bdocument\.cookie\s*=/.test(src), false,
      '密码路径上写了 document.cookie：会话 Cookie 是 HttpOnly，这里不该有第二份身份')
  }

  // URL 里不许出现密码：地址会进浏览器历史、Referer 与服务端访问日志。
  assert.equal(/`\/api\/[^`]*\$\{[^}]*[Pp]assword[^}]*\}/.test(CLIENT_SRC), false,
    '有密码被插进了请求路径')
  assert.equal(/URLSearchParams\([^)]*[Pp]assword/.test(CLIENT_SRC), false,
    '有密码被拼进了查询串')
  assert.equal(/\.set\(\s*'[^']*[Pp]assword/.test(CLIENT_SRC), false,
    '有密码被 set 进了查询串')

  // 密码只在请求体里。三个端点各查一次：漏一个就是一条明文走别处的路径。
  for (const re of [
    /createAccount:[\s\S]{0,400}?JSON\.stringify\(\{ username, password \}\)/,
    /resetAccountPassword:[\s\S]{0,400}?JSON\.stringify\(\{ password \}\)/,
    /changeOwnPassword:[\s\S]{0,400}?JSON\.stringify\(\{ currentPassword, newPassword \}\)/,
  ]) {
    assert.match(CLIENT_SRC, re, '有一个端点的密码没有走请求体')
  }
})

/**
 * 改自己的密码这条路径上，当前密码必须一路发到服务端。
 *
 * 这一条与上面那条纯函数断言配对：那条证明守卫本身对，这条证明调用方
 * 还在经过它，并且把折算出来的当前密码真的交给了客户端。本仓库已经出过
 * 十三次「守卫被测到、却没有东西证明调用方还在调用它」的测试。
 */
test('改密页把折算出来的当前密码交给了客户端，客户端把它发了出去', () => {
  assert.match(PAGE_SRC,
    /api\.changeOwnPassword\(resolved\.body\.currentPassword, resolved\.body\.newPassword\)/,
    '改密页没有把当前密码传给客户端：服务端会拒绝，而操作者只看到一句"密码不正确"')
  assert.match(CLIENT_SRC, /changeOwnPassword: \(currentPassword: string, newPassword: string\)/,
    '客户端的改密签名里没有当前密码')
})

/**
 * 角色只来自服务端的当前会话端点。
 *
 * 界面上任何一处「我是管理员」都必须能追到那一次请求 —— 从登录响应、
 * Cookie 或本地存储里推出来的角色，都是客户端自称（规范 §34）。
 */
test('角色来自当前会话端点，不从登录响应或本地推断', () => {
  assert.match(CLIENT_SRC, /me: \(\) => request<CurrentSession>\('\/api\/v1\/sessions\/current'\)/,
    '当前会话端点不再返回角色类型：界面的角色就没有来源了')
  assert.match(SESSION_SRC, /await api\.login\([\s\S]{0,120}?await api\.me\(\)/,
    '登录之后没有再问一次当前会话：登录响应里没有角色，界面会一直不知道自己是谁')
  assert.equal(SESSION_SRC.includes('setIdentity(await api.login('), false,
    '把登录响应直接当成了身份：那里面没有角色')
})

/** 服务端的拒绝理由一律原样展示，不收窄成一句通用失败。 */
test('服务端的拒绝理由原样展示，不收窄成通用失败', () => {
  const catches = PAGE_SRC.match(/catch \(err\)/g) ?? []
  const surfaced = PAGE_SRC.match(/err instanceof ApiError \? err\.msg/g) ?? []
  assert.equal(catches.length > 0, true, '账号页没有任何错误处理')
  assert.equal(surfaced.length, catches.length,
    '有 catch 分支吞掉了服务端的 msg：被吞掉的正是"先提一个管理员上来"那句话')
  assert.equal(PAGE_SRC.includes('window.alert'), false,
    '用 alert 展示拒绝理由：一弹就消失的提示，操作者照着做不了')
  assert.equal(PAGE_SRC.includes('window.confirm'), false,
    '用 confirm 做危险操作的确认：那里放不下"会把哪个账号变成什么"这三段话')
})
