/**
 * 两层校验结论的共用展示词汇。
 *
 * 仓库级与路径级是两个各自封闭的枚举（design doc 2026-08-13 §3.3），各自的
 * 文案表分别在 gitRepoForm.ts 与 clusterForm.ts —— 那两张表刻意不合并，
 * 合并回去就得再约定「哪些取值在这一层合法」，而那种约定只活在注释里。
 *
 * 这里只放两层必须逐字一致的部分：语气分档、校验时刻的措辞，以及「界面
 * 不认识的取值一律收窄成未校验」这条收窄纪律。分成两份拷贝一定会漂，
 * 而漂的落点是其中一层的 verifiedAt 又变回一个孤零零的时间戳，读起来像
 * 是「现在是通过的」（design doc §3.4）。
 *
 * 单独成文件而不是让一层 import 另一层：绑定指向仓库，仓库不知道集群，
 * 让 gitRepoForm 去 import clusterForm 会把这条依赖倒过来。这里不 import
 * 任何 React，测试可以直接跑它。
 */

/**
 * 结论的语气分档。只有三档，且刻意不做成"分数"：
 * 「没查过」与「查了、没通过」不是程度差别，是不同性质的事实。
 */
export type VerifyTone = 'ok' | 'bad' | 'unverified'

export interface VerifyStatusView {
  /** 结论本身，一律非空。 */
  label: string
  /** 一句话说明这个结论意味着什么、该找谁。 */
  detail: string
  tone: VerifyTone
  /** 校验时刻的说明，已明示是历史事实而非当前状态。 */
  checkedAt: string
}

export interface VerifyOutcomeView {
  /** 这次请求是否真的发生了一次校验。 */
  happened: boolean
  /** 给操作者看的一句话回执，一律非空。 */
  message: string
  tone: VerifyTone
}

/**
 * 一层校验结论的全部文案。
 *
 * 三张表的键类型都是那一层的封闭枚举而不是 string：后端新增一个取值却忘了
 * 在这里补文案，`tsc` 会直接报错，而不是让界面渲染出一个空白的结论。空白的
 * 校验结论会被读成「这个东西没问题」。
 */
export interface VerdictCopy<E extends string> {
  label: Record<E, string>
  detail: Record<E, string>
  tone: Record<E, VerifyTone>
}

/** 一次校验请求的响应形状，两层同构（只有枚举不同）。 */
export interface VerifyStatusResponse<E extends string> {
  verifyResult: E
  verifiedAt?: string | null
}

/**
 * 把一个 RFC3339 时刻格式化成 UTC 展示串。
 *
 * 解析不出来时原样返回而不是抛错：一个格式意外的时间戳该让那一格显示
 * 得难看，不该让整张表白屏——同一张表里还有别的行要看。
 */
export function formatUtcTime(iso: string): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return d.toISOString().replace('T', ' ').replace(/\.\d+Z$/, ' UTC')
}

/**
 * 把校验时刻写成一句明确的历史陈述。
 *
 * 不能只甩一个时间戳：一个孤零零的时间挨着「只读校验通过」，读起来就是
 * 「现在是通过的」。轮 4 写回前必须重新校验，拿几天前的结论当此刻的状态
 * 正是 design doc §3.4 禁止的那件事，界面上的措辞是第一道拦截。
 */
export function describeCheckedAt(at: string | null | undefined): string {
  if (!at) return '从未校验过'
  return `上次校验于 ${formatUtcTime(at)} —— 这是当时的结论，不代表此刻的状态`
}

/**
 * 把一个结论折算成可直接渲染的形态。
 *
 * 单独成函数而不是在组件里就地查表，是为了让这段判断能被测试直接跑：
 * 组件是 .tsx，`node --test` 的类型擦除读不了 JSX，留在组件里就等于
 * 这套文案永远没有测试。
 *
 * 未登记的取值不按原样透出，也不留空，而是收窄成「未校验」的语气：
 * 结论字段是封闭枚举，一个界面还不认识的值只能说明文案没跟上，此时
 * 失败方向朝「未确认」关，不朝「可信」开（与后端 Valid() 的收窄同一条纪律）。
 */
export function describeVerdict<E extends string>(
  copy: VerdictCopy<E>, result: E, at: string | null | undefined,
): VerifyStatusView {
  if (!Object.hasOwn(copy.label, result)) {
    return {
      label: '结论无法识别',
      detail: `服务端给出了本界面还不认识的结论「${String(result)}」。`
        + '在文案补齐之前一律按未校验处置：不认识的结论不是通过了的结论。',
      tone: 'unverified',
      checkedAt: describeCheckedAt(at),
    }
  }
  return {
    label: copy.label[result],
    detail: copy.detail[result],
    tone: copy.tone[result],
    checkedAt: describeCheckedAt(at),
  }
}

/**
 * 把一次保存/重校验请求的响应折算成一句回执。
 *
 * 要处理的是一个会让人读错的形状：未配置校验器时，服务端返回
 * `NOT_VERIFIED` 且 `verifiedAt` 缺席，并且**刻意什么都不落库** —— 写一个
 * 时间戳就等于宣称某时某刻校验过一次，而那件事没有发生
 * （internal/httpapi/gitverify_handler.go、repo_handler.go 两处同一处置）。
 *
 * 于是响应会与重新加载后看到的东西不一致：响应说"从未校验"，库里那行
 * 却还留着更早的结论。两者都没错，它们回答的是不同的问题 —— 响应说的是
 * "这一次发生了什么"，列表说的是"库里现在记着什么"。
 *
 * 处置：`verifiedAt` 缺席一律读作"这次没有发生校验"，回执明说这一点，
 * 且**不把 NOT_VERIFIED 当成一个新结论贴到界面上**。把它渲染成一个崭新的
 * 「未校验」，操作者会以为自己刚刚把结论刷掉了，而下一次刷新页面它又变
 * 回旧结论 —— 一个自己会变回去的界面，比一个说"什么都没发生"的界面更
 * 难被信任。列表那一格照旧由服务端的读模型驱动，不受这条回执影响。
 */
export function describeOutcome<E extends string>(
  copy: VerdictCopy<E>, status: VerifyStatusResponse<E>,
): VerifyOutcomeView {
  if (!status.verifiedAt) {
    return {
      happened: false,
      // 措辞要同时对两种情形成立：库里已有一个更早的结论（重校验），以及
      // 库里本来就没有结论（刚建好/刚绑上）。说成「仍是上一次校验的结论」
      // 在后者是假的——那里从来没有过一次校验。
      message: '这次没有发生校验：平台未配置校验器（或校验器给出了未登记的结论），'
        + '服务端因此没有记下任何结论——列表里那一格是库里原有的结论，不是刚刚得出的。',
      tone: 'unverified',
    }
  }
  const known = Object.hasOwn(copy.label, status.verifyResult)
  return {
    happened: true,
    message: `校验于 ${formatUtcTime(status.verifiedAt)} 完成：`
      + `${known ? copy.label[status.verifyResult] : '结论无法识别'}。`,
    tone: known ? copy.tone[status.verifyResult] : 'unverified',
  }
}
