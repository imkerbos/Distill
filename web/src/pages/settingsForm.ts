import type { PlatformSettingView, PlatformSettingWrite, SecretsBackend } from '../api/types'

/**
 * 平台设置页的纯逻辑层：播种表单、折算写入体、算出"这次要改什么"。
 *
 * 单独成文件而不是留在 SettingsPage.tsx 里，理由同 clusterForm.ts：
 * 这里的每一条规则都要被测试直接跑到，而页面里跑不了。更要紧的是本页
 * 的两条规则一旦漂移，代价不是界面难看——一条决定平台愿意和哪台 SSH
 * 服务器说话（host key），另一条决定平台去哪里取身份（凭据后端）。
 *
 * 这里不 import 任何 React。
 */

/* ---------------------------------------------------------------------- */
/* 字段表：谁是启动时读一次的                                                */
/* ---------------------------------------------------------------------- */

/**
 * 一项设置改完之后什么时候生效。
 *
 * 绝大多数设置是"按需读取"的，改完立刻生效（design doc §1.1）。
 * `AFTER_RESTART` 是那一小撮例外：值被消费方在构造的那一刻收进内部，
 * 之后没有任何接口能改它们。
 */
export type SettingEffect = 'IMMEDIATE' | 'AFTER_RESTART'

/**
 * 凭据后端的界面文案。
 *
 * 括号里带上枚举原值：这一项决定平台去哪里取身份，而操作者读日志、读
 * 审计、读后端报错时看到的是 `DIR`、`SECRET_MANAGER` 这些原值——只给
 * 中文，两边就对不上号（同 clusterForm.ts 的 VERIFY_RESULT_LABEL）。
 */
export const SECRETS_BACKEND_LABEL: Record<SecretsBackend, string> = {
  NONE: '不解析凭据（NONE）',
  DIR: '本地目录（DIR）',
  SECRET_MANAGER: 'Secret Manager（SECRET_MANAGER）',
}

/** 下拉选项的顺序来源。与 SECRETS_BACKEND_LABEL 同一份，不另写一遍。 */
export const ALL_SECRETS_BACKENDS = Object.keys(SECRETS_BACKEND_LABEL) as SecretsBackend[]

/** 表单里以数字承载的那几项。 */
export type NumericSettingKey =
  | 'sessionTtlSeconds' | 'httpReadTimeoutMs' | 'httpWriteTimeoutMs'
  | 'httpShutdownTimeoutMs' | 'gitVerifyTimeoutMs'

export interface NumericFieldSpec {
  key: NumericSettingKey
  label: string
  effect: SettingEffect
  /** 一句说明这一项管什么，直接渲染在输入框旁边。 */
  hint: string
}

/**
 * 数字类设置的唯一一份清单：输入框由它渲染，差异也由它计算。
 *
 * 做成一张表而不是在页面里手写五个输入框、再在 settingsDiff 里手写五条
 * 比较：两处各写一份，`effect` 迟早只在一处被改对。落点是操作者在"立刻
 * 生效"的措辞下改了会话 TTL，以为新的有效期已经在管着现有会话，而实际上
 * 要等下一次重启——这是一句会被相信的假话，且不会有任何报错纠正它。
 *
 * 前四项标 AFTER_RESTART，依据是 design doc §1.1 记下的那三项例外加会话
 * TTL：`http.Server` 与 `auth.SessionStore` 在构造时把它们收进内部。
 * `gitVerifyTimeoutMs` 不在其列——Git 校验在使用处现读设置。
 */
export const NUMERIC_FIELDS: readonly NumericFieldSpec[] = [
  {
    key: 'sessionTtlSeconds',
    label: '会话有效期（秒）',
    effect: 'AFTER_RESTART',
    hint: '登录后会话多久过期。',
  },
  {
    key: 'httpReadTimeoutMs',
    label: 'HTTP 读超时（毫秒）',
    effect: 'AFTER_RESTART',
    hint: '读取一个请求的时长上限。',
  },
  {
    key: 'httpWriteTimeoutMs',
    label: 'HTTP 写超时（毫秒）',
    effect: 'AFTER_RESTART',
    hint: '写出一个响应的时长上限。',
  },
  {
    key: 'httpShutdownTimeoutMs',
    label: '优雅退出超时（毫秒）',
    effect: 'AFTER_RESTART',
    hint: '进程退出时等待在途请求收尾的上限。',
  },
  {
    key: 'gitVerifyTimeoutMs',
    label: 'Git 校验超时（毫秒）',
    effect: 'IMMEDIATE',
    hint: '一次只读校验的出站超时；校验在使用处现读设置，改完立刻生效。',
  },
]

/**
 * 需要重启才生效的那几项，写给操作者看的一句话。
 *
 * 措辞不含"可能""通常"：这件事是确定的，含糊的措辞会让人以为刷新一下
 * 页面就好。保存成功却不说这句，等于让操作者以为新值已经在管着现有
 * 会话与现有连接。
 */
export const RESTART_NOTICE =
  '这几项在进程启动时被 http.Server 与会话存储收进内部，保存后不会立刻生效，'
  + '要等 distill-api 重启（design doc §1.1）。在此之前生效的仍是旧值。'

/* ---------------------------------------------------------------------- */
/* 表单值                                                                   */
/* ---------------------------------------------------------------------- */

/**
 * 设置表单的全部可编辑值。
 *
 * 数字项以字符串承载，交给操作者在提交前自由编辑（同 ApiServerRow.port）：
 * 存成 number 就得在每次按键时决定空输入框是 0 还是 NaN，而 0 在这里的
 * 含义是"关掉超时保护"。
 *
 * host key 只有一个**输入**框，没有对应的当前值字段：服务端不回原文
 * （PlatformSettingView 里根本没有那个字段），因此这份表单也无从预填它。
 * 留空表示"不修改"，见 buildSettingsWrite。
 */
export interface SettingsFormValues {
  sessionTtlSeconds: string
  httpReadTimeoutMs: string
  httpWriteTimeoutMs: string
  httpShutdownTimeoutMs: string
  secretsBackend: SecretsBackend
  secretsProject: string
  secretsPrefix: string
  secretsDir: string
  gitVerifyTimeoutMs: string
  /** 新的 known_hosts 原文。留空 = 不修改信任锚。 */
  hostKeysInput: string
}

/**
 * 从服务端返回的设置播种表单。
 *
 * 每一项都预填，一项都不能省：PUT 是整行替换，表单里空着的项提交后就是
 * 库里被清成零值的项（同 EditClusterForm 的理由）。
 *
 * 唯一的例外是 hostKeysInput —— 它**恒为空串**。这不是"忘了预填"，是这
 * 一层唯一能做的事：入参 PlatformSettingView 里没有 host key 原文，指纹
 * 也绝不能填进输入框（填进去就会被当成新的 known_hosts 原样提交，把信任
 * 锚换成一串摘要文本，此后所有 Git 校验都连不上）。
 */
export function settingsFormValuesOf(v: PlatformSettingView): SettingsFormValues {
  return {
    sessionTtlSeconds: String(v.sessionTtlSeconds),
    httpReadTimeoutMs: String(v.httpReadTimeoutMs),
    httpWriteTimeoutMs: String(v.httpWriteTimeoutMs),
    httpShutdownTimeoutMs: String(v.httpShutdownTimeoutMs),
    secretsBackend: v.secretsBackend,
    secretsProject: v.secretsProject,
    secretsPrefix: v.secretsPrefix,
    secretsDir: v.secretsDir,
    gitVerifyTimeoutMs: String(v.gitVerifyTimeoutMs),
    hostKeysInput: '',
  }
}

/* ---------------------------------------------------------------------- */
/* 校验与折算                                                               */
/* ---------------------------------------------------------------------- */

export type SettingsBuildResult =
  | { ok: true; body: PlatformSettingWrite }
  | { ok: false; error: string }

/**
 * 凭据后端与其字段必须互相印证。
 *
 * 逐条镜像后端 registry.validateSecretsBackendFields。**镜像不是替代**
 * （规范 §34：前端不是安全边界）—— 服务端那份仍然是判定的那一份，这里
 * 只是不让操作者在点了保存之后才发现自己选的组合从一开始就不成立。
 *
 * 值得在提交前拦的理由是这个组合读起来像是成立的：选了 DIR 却把
 * project 留在框里，操作者看到的是"两套后端都配好了"，而实际生效的
 * 只有一个。服务端会拒掉它，但那时操作者已经在心里认定自己配好了。
 */
function secretsFieldsError(v: SettingsFormValues): string {
  const project = v.secretsProject.trim()
  const prefix = v.secretsPrefix.trim()
  const dir = v.secretsDir.trim()

  switch (v.secretsBackend) {
    case 'NONE':
      if (project !== '' || prefix !== '' || dir !== '') {
        return '凭据后端选 NONE（不解析凭据）时，项目 / 前缀 / 目录必须全部留空 ——'
          + '留着值等于你以为配置好的东西其实从未生效。'
      }
      return ''
    case 'DIR':
      if (dir === '') {
        return '凭据后端选 DIR 时必须填凭据目录。'
      }
      if (project !== '' || prefix !== '') {
        return '凭据后端选 DIR 时，Secret Manager 项目与前缀必须留空 ——'
          + '两边都填着，操作者会以为两套后端都在生效，而实际取身份的只有本地目录。'
      }
      return ''
    case 'SECRET_MANAGER':
      if (project === '' || prefix === '') {
        return '凭据后端选 SECRET_MANAGER 时，项目与前缀都必须填 ——'
          + '前缀为空仍能拼出合法路径，但围栏就只剩项目一层，任何短名都能指向项目里的任意 secret。'
      }
      if (dir !== '') {
        return '凭据后端选 SECRET_MANAGER 时，本地凭据目录必须留空。'
      }
      return ''
  }
}

/** 解析一个正整数毫秒/秒值。空、非整数、非正一律拒绝。 */
function parsePositiveInt(raw: string, label: string): number | string {
  const s = raw.trim()
  if (!/^\d+$/.test(s)) {
    return `${label} 必须是一个整数。`
  }
  const n = Number(s)
  // 与后端 ValidatePlatformSetting 同一条：为零不是"不限制"，而是会话
  // 立即过期、超时保护被关掉，且没有任何报错会提示这一点。
  if (n <= 0) {
    return `${label} 必须为正 —— 填 0 不是"不限制"，而是会话立即过期或超时保护被关闭。`
  }
  return n
}

/**
 * 把表单折算成一份完整的写入体。
 *
 * host key 的处理是本函数存在的主要理由：
 *
 * 输入框留空 = **不修改信任锚**。但服务端的 PUT 是整行替换，请求体里
 * 的 `gitVerifyHostKeys` 会被原样写进库（internal/mysqlregistry/setting.go
 * UpdateSetting）—— 也就是说，"留空"在协议上无法表达，发空串就是清空。
 * 而清空的后果不是"退化成不校验"：host key 为空时 gitverify.New 直接拒绝
 * 构造（design doc §1.3），此后每一次 Git 校验都出不了结论。
 *
 * 于是当库里已经装着 host key（指纹非空）而输入框留空时，这里**不提交**，
 * 而不是提交一个会清空它的请求体。安全的失败方向是一次被拒绝的保存，
 * 不是一次静默移除的信任锚（规范 §49）。
 *
 * 指纹为空时留空则照常提交空串：库里本来就没有 host key，写空串不改变
 * 任何事实，拦下它只会让一个从未配过 host key 的平台连会话 TTL 都改不了。
 */
export function buildSettingsWrite(
  values: SettingsFormValues, current: PlatformSettingView,
): SettingsBuildResult {
  // 五项一起走同一张表：漏掉一项就等于那一项的输入框存在、却从不被读取，
  // 保存后悄悄回到旧值。
  const numbers = {} as Record<NumericSettingKey, number>
  for (const f of NUMERIC_FIELDS) {
    const parsed = parsePositiveInt(values[f.key], f.label)
    if (typeof parsed === 'string') return { ok: false, error: parsed }
    numbers[f.key] = parsed
  }

  const fieldsError = secretsFieldsError(values)
  if (fieldsError !== '') return { ok: false, error: fieldsError }

  const hostKeys = values.hostKeysInput.trim()
  if (hostKeys === '' && current.gitVerifyHostKeysFingerprint !== '') {
    return {
      ok: false,
      error: '保存被拦下：host key 输入框留空表示"不修改"，但服务端的保存是整行替换，'
        + '一次留空的提交会把当前的 SSH 信任锚清空，此后所有 Git 校验都无法进行。'
        + `要保存其他改动，请把当前生效的 known_hosts 原文粘回输入框（当前指纹 ${current.gitVerifyHostKeysFingerprint}，`
        + '保存后可据此核对粘的是不是同一份）。',
    }
  }

  return {
    ok: true,
    body: {
      sessionTtlSeconds: numbers.sessionTtlSeconds,
      httpReadTimeoutMs: numbers.httpReadTimeoutMs,
      httpWriteTimeoutMs: numbers.httpWriteTimeoutMs,
      httpShutdownTimeoutMs: numbers.httpShutdownTimeoutMs,
      secretsBackend: values.secretsBackend,
      secretsProject: values.secretsProject.trim(),
      secretsPrefix: values.secretsPrefix.trim(),
      secretsDir: values.secretsDir.trim(),
      gitVerifyTimeoutMs: numbers.gitVerifyTimeoutMs,
      gitVerifyHostKeys: hostKeys,
    },
  }
}

/* ---------------------------------------------------------------------- */
/* 保存前的差异                                                             */
/* ---------------------------------------------------------------------- */

export interface SettingsDiffRow {
  label: string
  before: string
  after: string
  effect: SettingEffect
}

/** host key 那一行的 after 文案。常量而不是就地拼串，测试要能钉死它。 */
export const HOST_KEYS_AFTER = '换成本次提交的 known_hosts（保存后以新指纹核对）'

/**
 * 算出这次保存会改动哪些项，只列改动的。
 *
 * 存在的理由不是"友好"：这次写入会产生一条审计行，而其中一项决定平台
 * 愿意和哪台 SSH 服务器说话（design doc §1.3）。操作者应当在按下保存
 * 之前看见自己正在提交的那个差异，而不是事后去翻审计。
 *
 * **host key 那一行只写"换成本次提交的那一份"，不写原文。** 差异表是页
 * 面上一块会被渲染、会被截图、会被贴进工单的区域；把原文放进去，就等于
 * 服务端拒绝回显 host key 的那份克制在最后一步被前端抵消掉了
 * （规范 §19、§20）。这一行也不比较"是否真的变了"—— 前端算不出指纹
 * （比较需要 SHA-256，浏览器里只有异步的 crypto.subtle，而为一行差异
 * 文案引入异步或引入一个依赖都不值得），因此只要提交带了原文就列出来。
 */
export function settingsDiff(
  current: PlatformSettingView, next: PlatformSettingWrite,
): SettingsDiffRow[] {
  const rows: SettingsDiffRow[] = []

  for (const f of NUMERIC_FIELDS) {
    if (current[f.key] !== next[f.key]) {
      rows.push({
        label: f.label,
        before: String(current[f.key]),
        after: String(next[f.key]),
        effect: f.effect,
      })
    }
  }

  if (current.secretsBackend !== next.secretsBackend) {
    rows.push({
      label: '凭据后端',
      before: SECRETS_BACKEND_LABEL[current.secretsBackend],
      after: SECRETS_BACKEND_LABEL[next.secretsBackend],
      effect: 'IMMEDIATE',
    })
  }

  const texts: [string, string, string][] = [
    ['Secret Manager 项目', current.secretsProject, next.secretsProject],
    ['Secret Manager 前缀', current.secretsPrefix, next.secretsPrefix],
    ['本地凭据目录', current.secretsDir, next.secretsDir],
  ]
  for (const [label, before, after] of texts) {
    if (before !== after) {
      rows.push({ label, before: before || '（空）', after: after || '（空）', effect: 'IMMEDIATE' })
    }
  }

  if (next.gitVerifyHostKeys !== '') {
    rows.push({
      label: 'SSH 信任锚（host key）',
      before: current.gitVerifyHostKeysFingerprint || '（未配置）',
      after: HOST_KEYS_AFTER,
      effect: 'IMMEDIATE',
    })
  }

  return rows
}

/**
 * 这次改动里哪几项要等重启。
 *
 * 由差异行算出而不是由页面自己挑：页面挑就意味着"哪些项是启动时读的"
 * 这件事在界面上有第二份写法，而两份写法只会有一份被改对。
 */
export function restartRequiredLabels(rows: readonly SettingsDiffRow[]): string[] {
  return rows.filter((r) => r.effect === 'AFTER_RESTART').map((r) => r.label)
}
