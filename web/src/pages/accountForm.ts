import type { Account, Role } from '../api/types'
import { formatUtcTime } from './verifyView.ts'

/**
 * 账号页的纯逻辑层：谁看得到管理员入口、一次操作会造成什么、提交体长什么样。
 *
 * 单独成文件而不是留在组件里，理由与 verifyView 那条相同：组件是 .tsx，
 * `node --test` 的类型擦除读不了 JSX，留在组件里的判断等于永远没有测试。
 * 这里不 import 任何 React。
 */

/* ---------------------------------------------------------------------- */
/* 1. 角色只决定渲染什么，不决定允许什么                                       */
/* ---------------------------------------------------------------------- */

const ROLE_LABEL: Record<Role, string> = { ADMIN: '管理员', VIEWER: '只读' }

/**
 * 角色的中文名。
 *
 * 界面不认识的取值不按原样透出、也不留空，而是明说它未登记：角色是封闭
 * 枚举，出现界面不认识的值只可能是库里被手工改过或文案没跟上，此时把它
 * 显示成一个像模像样的角色名，读的人会以为平台承认这个角色。
 */
export function roleLabel(role: Role): string {
  return ROLE_LABEL[role] ?? `未登记的角色「${String(role)}」`
}

/**
 * showsAccountAdminEntry 报告这个角色是否该看到账号管理入口。
 *
 * **这只是体验，不是安全。** 服务端已经对 /api/v1/accounts 下的每一个端点
 * 声明了 accessAdmin，并在每次请求现读账号的角色（internal/httpapi/router.go、
 * internal/auth Verifier.RoleOf）—— 一个只读账号把地址直接敲进浏览器，
 * 拿到的仍然是 403，一行账号数据都读不到。这里隐藏入口省下的只是「点了
 * 才发现被拒」那一次无用点击（规范 §34、design doc 2026-08-14 §8）。
 *
 * 谁要是把这个函数当成那些端点的保护，删掉它时会以为自己只是改了导航。
 */
export function showsAccountAdminEntry(role: Role): boolean {
  return role === 'ADMIN'
}

/* ---------------------------------------------------------------------- */
/* 2. 引导账号：必须在失效之前把话说清楚                                       */
/* ---------------------------------------------------------------------- */

/**
 * 引导账号即将失效这件事，必须在它发生**之前**说清楚。
 *
 * 首次启动时库里一个账号都没有，唯一的入口是配置文件里的引导账号，而它
 * 只在「库里没有任何启用中的管理员」时可登录（design doc 2026-08-14 §2）。
 * 于是第一个启用中的管理员出现的那一刻，操作者手里的凭据当场作废 ——
 * 如果他不知道那个新管理员的密码，下一次登录就被关在自己的平台外面。
 *
 * 事后解释这件事没有意义：那时已经进不来了，只能改数据库救。
 */
export const BOOTSTRAP_LOCKOUT_WARNING =
  '你现在用的是配置文件里的引导账号。它只在库里一个启用中的管理员都没有时才能登录：'
  + '库里出现第一个启用中的管理员的那一刻，这个引导凭据立即失效，下一次登录就用不了了。'
  + '所以在把某个账号变成管理员之前，先确认你知道那个账号的密码 ——'
  + '否则下一次登录时，你会被关在自己的平台外面，只能改数据库才能进来。'

/**
 * 服务端拒绝「动最后一个启用中的管理员」时会说的那件事。
 *
 * 界面提前说一遍，是为了不让操作者点完才发现（design doc §5）。真正的
 * 判定在服务端同一个事务里做出，界面这份计数只是刚拉到的那一份列表的
 * 快照 —— 两个管理员同时互相降级时，只有事务里那次判定拦得住。
 */
export const LAST_ADMIN_REFUSAL =
  '服务端会拒绝这次操作：这是最后一个启用中的管理员，做完就没有人能管理平台了。'
  + '先把另一个账号提升为管理员，再回来动这一个。'

/**
 * signedInViaBootstrap 报告当前会话用的是不是配置文件里的引导账号。
 *
 * 判定方式与服务端 auditBootstrapLogin 同源：**在账号表里查不到自己**。
 * 列表返回全部未软删除的行，库内账号必然在其中，因此「已经登录、却不在
 * 账号表里」只剩引导账号这一种可能。
 *
 * 比对刻意用严格相等而不是忽略大小写，方向是故意的：账号表的排序规则是
 * utf8mb4_0900_ai_ci，登录名与库里那行可能只差大小写。判错成「是引导账号」
 * 的代价是多显示一条警告，判错成「不是」的代价是那条警告在最该出现的时候
 * 消失 —— 两个方向的代价不对称，就往多说一句的那边错。
 *
 * 列表还没到手时返回 false：还不知道，就先不吓人。
 */
export function signedInViaBootstrap(username: string, accounts: Account[] | null): boolean {
  if (!accounts) return false
  return !accounts.some((a) => a.username === username)
}

/** 库里此刻启用中的管理员。它为空，就意味着引导口是开着的。 */
export function enabledAdmins(accounts: Account[]): Account[] {
  return accounts.filter((a) => a.role === 'ADMIN' && !a.disabledAt)
}

/* ---------------------------------------------------------------------- */
/* 3. 一次操作会造成什么，在它发生之前写出来                                    */
/* ---------------------------------------------------------------------- */

/** 账号页上会改变某个账号的六件事，封闭枚举。 */
export type AccountActionKind =
  'PROMOTE' | 'DEMOTE' | 'DISABLE' | 'ENABLE' | 'DELETE' | 'RESET_PASSWORD'

export interface ActionPreview {
  /** 按钮上的动词。 */
  verb: string
  /** 这一次会把哪个账号变成什么 —— 点名账号，并说出变化本身。 */
  change: string
  /** 已知服务端会拒绝时的理由；空串表示没有已知的拒绝。 */
  refusal: string
  /** 这一次会不会让引导账号失效；空串表示不改变引导口的状态。 */
  bootstrap: string
}

/**
 * 动最后一个启用中的管理员，服务端一律拒绝（design doc §5）。
 *
 * 三种动法都算：降级、停用、软删除 —— 做完都是「没有人能管理平台」。
 */
function isLastEnabledAdmin(target: Account, accounts: Account[]): boolean {
  if (target.role !== 'ADMIN' || target.disabledAt) return false
  return enabledAdmins(accounts).length === 1
}

/**
 * 这一次操作会不会关上引导口 —— 也就是会不会让库里出现第一个启用中的管理员。
 *
 * 只有提升角色与启用一个已停用的管理员会造成这件事。新建账号不会：服务端
 * 固定把新账号建成只读。
 */
function closesBootstrapGate(
  kind: AccountActionKind, target: Account, accounts: Account[],
): boolean {
  if (enabledAdmins(accounts).length > 0) return false
  if (kind === 'PROMOTE') return true
  return kind === 'ENABLE' && target.role === 'ADMIN'
}

/**
 * 把一次操作折算成「点下去之前该读到的那段话」。
 *
 * 每一条都点名账号、说出变化本身，危险的那几条还要说出后果：这一页上的
 * 六个动作从按钮上看长得一样，而其中三个做完就再也没有人能管理平台。
 */
export function previewAccountAction(
  kind: AccountActionKind, target: Account, accounts: Account[],
): ActionPreview {
  const u = target.username
  const refusal = isLastEnabledAdmin(target, accounts)
    && (kind === 'DEMOTE' || kind === 'DISABLE' || kind === 'DELETE')
    ? LAST_ADMIN_REFUSAL
    : ''
  const bootstrap = closesBootstrapGate(kind, target, accounts)
    ? `${BOOTSTRAP_LOCKOUT_WARNING}这一步就是那一刻：${u} 会成为库里第一个启用中的管理员。`
    : ''

  switch (kind) {
    case 'PROMOTE':
      return {
        verb: '提升为管理员',
        change: `把 ${u} 的角色从只读改成管理员：它将能新建、停用、删除账号，`
          + '能改任何账号的角色，也能重置任何人的密码。下一次请求就生效。',
        refusal,
        bootstrap,
      }
    case 'DEMOTE':
      return {
        verb: '降为只读',
        change: `把 ${u} 的角色从管理员改成只读：它立刻失去账号管理与全部写操作，`
          + '下一次请求就生效，不必等它手里那张会话过期。',
        refusal,
        bootstrap,
      }
    case 'DISABLE':
      return {
        verb: '停用',
        change: `停用 ${u}：它现有的会话在下一次请求就失效，也不能再登录。`
          + '停用是可逆的暂停，随时可以再启用。',
        refusal,
        bootstrap,
      }
    case 'ENABLE':
      return {
        verb: '启用',
        change: `启用 ${u}：它可以重新登录，并按当前角色（${roleLabel(target.role)}）判定权限。`,
        refusal,
        bootstrap,
      }
    case 'DELETE':
      return {
        verb: '删除',
        change: `删除 ${u}：这是软删除 —— 它做过的事仍然留在审计里，`
          + '而这个用户名不会被回收，以后不能再建一个同名账号。',
        refusal,
        bootstrap,
      }
    case 'RESET_PASSWORD':
      return {
        verb: '重置密码',
        change: `给 ${u} 设一个新密码：它原来的密码立即失效。你不需要知道原密码，`
          + `这次重置会以你的名义写进审计。新密码不会回显，平台也不会替你记住它 ——`
          + `只有你把它交给 ${u} 的那个方式能保住它。`,
        refusal,
        bootstrap,
      }
  }
}

/** 账号在列表里的状态说明。启用中也要有字，一格空白会被读成「加载中」。 */
export function accountStatusLabel(a: Account): string {
  if (!a.disabledAt) return '启用中'
  return `已停用（${formatUtcTime(a.disabledAt)}）`
}

/* ---------------------------------------------------------------------- */
/* 4. 三份提交体                                                            */
/* ---------------------------------------------------------------------- */

/**
 * 三个表单都刻意**不**在这里复述服务端的密码长度规则。
 *
 * 那条规则的准确措辞只有服务端的 registry.ValidatePassword 有：下限按字符
 * 数（12 个）、上限按字节数（72，bcrypt 的输入上限），两个方向用的不是同一
 * 个单位，中文字符在 UTF-8 下占 2~4 字节。在这里抄一份必然与它漂开，而漂开
 * 的表现是一个后端明明收得下、界面却不让提交的死角，排查的人会去查后端。
 *
 * 这里只挡住两件服务端看不见或不值得为之发一次请求的事：字段空着，以及
 * 两次输入不一致 —— 后者服务端根本不知道有「两次输入」这回事。其余一律
 * 交给服务端，并原样展示它的拒绝理由（同 gitRepoForm 对 SSH 形态的处置，
 * 规范 §34）。
 */
const PASSWORD_MISMATCH = '两次输入的新密码不一致。密码不回显，也无从事后核对，因此必须当场对上。'

export interface NewAccountValues {
  username: string
  password: string
  confirm: string
}

export type NewAccountResolution =
  | { ok: true; body: { username: string; password: string }; summary: string }
  | { ok: false; error: string }

export const blankNewAccount = (): NewAccountValues => ({ username: '', password: '', confirm: '' })

/**
 * 把新建表单折算成提交体。
 *
 * 提交体里**没有 role**：服务端的 createAccountRequest 也没有这个字段，
 * 新账号一律建成只读。带一个不会被采纳的角色上去，操作者会以为自己刚
 * 建出了一个管理员，而那正是「提权必须是一次单独的、有自己审计行的操作」
 * 要防的形状（design doc §6）。
 */
export function resolveNewAccount(values: NewAccountValues): NewAccountResolution {
  const username = values.username.trim()
  if (username === '') return { ok: false, error: '请填写用户名。' }
  if (values.password === '') return { ok: false, error: '请填写密码。' }
  if (values.password !== values.confirm) return { ok: false, error: PASSWORD_MISMATCH }
  return {
    ok: true,
    body: { username, password: values.password },
    summary: `提交后：新建账号 ${username}，角色是只读。服务端固定把新账号建成只读 ——`
      + '要让它成为管理员得再单独改一次角色，那一步才是引导账号失效的时刻。',
  }
}

export interface OwnPasswordValues {
  currentPassword: string
  newPassword: string
  confirm: string
}

export type OwnPasswordResolution =
  | { ok: true; body: { currentPassword: string; newPassword: string } }
  | { ok: false; error: string }

export const blankOwnPassword = (): OwnPasswordValues =>
  ({ currentPassword: '', newPassword: '', confirm: '' })

/**
 * 把「改自己的密码」折算成提交体。
 *
 * **当前密码必填，且必须真的发出去**（规范 §28，design doc §6）：会话可能
 * 是一台没锁屏的机器留下的，而改密码会把账号的控制权永久转移给改的人。
 * 服务端的 handleChangeOwnPassword 会用它走一次 Verify，不对就回
 * CodeInvalidCredentials —— 这里空着就拦，是为了不让操作者把一次必然失败
 * 的请求读成「服务端说我密码错了」。
 */
export function resolveOwnPassword(values: OwnPasswordValues): OwnPasswordResolution {
  if (values.currentPassword === '') {
    return {
      ok: false,
      error: '请填写当前密码：改密码会把账号的控制权永久转移，服务端必须先确认'
        + '坐在这台机器前面的确实是你本人，而不是一张被捡到的会话。',
    }
  }
  if (values.newPassword === '') return { ok: false, error: '请填写新密码。' }
  if (values.newPassword !== values.confirm) return { ok: false, error: PASSWORD_MISMATCH }
  return {
    ok: true,
    body: { currentPassword: values.currentPassword, newPassword: values.newPassword },
  }
}

export interface ResetPasswordValues {
  password: string
  confirm: string
}

export type ResetPasswordResolution =
  | { ok: true; body: { password: string } }
  | { ok: false; error: string }

export const blankResetPassword = (): ResetPasswordValues => ({ password: '', confirm: '' })

/**
 * 把管理员重置他人密码折算成提交体。
 *
 * 提交体里没有当前密码，这是对的：管理员并不知道对方的密码。这条路径由
 * 审计（RESET_PASSWORD）负责事后可查，不由一次凭据校验负责。
 */
export function resolveResetPassword(values: ResetPasswordValues): ResetPasswordResolution {
  if (values.password === '') return { ok: false, error: '请填写新密码。' }
  if (values.password !== values.confirm) return { ok: false, error: PASSWORD_MISMATCH }
  return { ok: true, body: { password: values.password } }
}
