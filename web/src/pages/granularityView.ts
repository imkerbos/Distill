import type { Granularity, Widening } from '../api/types'

/*
 * 策略粒度在界面上的取数层（design doc 2026-08-19）。
 *
 * 判定集中在这一个纯函数模块里，页面只渲染它的返回值 —— 散在 .tsx 里的
 * `granularity === 'namespace'` 会各自漂，而漂的方向恰恰是"缺字段时当成
 * 粗粒度"，那正是这里要挡住的东西。这里也是 tests/ 唯一能钉住它的地方。
 */

export interface GranularityView {
  readonly code: Granularity
  /** 切换按钮上的短标签。 */
  readonly label: string
  /** 这个粒度的主体是什么 —— 决定 podSelector 选中谁。 */
  readonly subject: string
  /** 这个粒度意味着什么代价。 */
  readonly detail: string
}

/**
 * 把后端回显的粒度翻成屏幕上那几句话。
 *
 * **缺席、空串、以及任何未登记的取值一律落到 WORKLOAD**，不落到 NAMESPACE：
 * 老响应、字段改了名，在前端看起来是同一件事 —— 我们不知道屏幕上这份策略是
 * 哪个粒度。显示成 NAMESPACE 就是把一份本该只选中一个 workload 的策略说成
 * 选中整个命名空间，而那是更宽的那一侧（安全规范 §49）。
 */
export function granularityView(raw: Granularity | null | undefined): GranularityView {
  if (raw === 'NAMESPACE') {
    return {
      code: 'NAMESPACE',
      label: 'namespace',
      subject: '整个命名空间（podSelector 为空，选中其中全部 Pod）',
      detail: '一个命名空间一份策略，条数少得多、读得完。放行的对端仍然精确到'
        + ' workload 与端口 —— 粗化的只有「谁发起」这一半。',
    }
  }
  return {
    code: 'WORKLOAD',
    label: 'workload',
    subject: '单个 workload（podSelector 用它实际命中的标签键）',
    detail: '最精确的一层，也是人工确认挂靠的那一层。代价是份数多 ——'
      + ' 一个几百个工作负载的集群会给出几百份策略。',
  }
}

/** 两个粒度的登记取值，供切换控件枚举。 */
export const ALL_GRANULARITIES: readonly Granularity[] = ['NAMESPACE', 'WORKLOAD']

/**
 * 折叠的放宽报告那一句话。
 *
 * 三种情形都要说出话，一句都不能省：
 *
 * - **全 0**：这不是"没什么可说的"，而是一句很强的话 —— 这次折叠没有多放行
 *   任何东西，粗粒度与细粒度的允许面完全相同。省掉它，操作者会以为放宽了
 *   而不敢用。
 * - **有放宽**：点名是哪几个 namespace、多出多少份授权。只报总数会让人无从
 *   下手；把无损的也列出来会让人去看不需要看的地方。
 * - **null**：服务端没回答。契约要求这个字段恒为非 nil，因此真拿到 null 只
 *   说明这不是一份守约的响应 —— 读成"折叠无损"就是把一个我们不知道的事情
 *   说成最让人放心的那个方向（与 dataSourceView / notAssessedNote 同一条纪律）。
 */
export function wideningNote(list: readonly Widening[] | null | undefined): string {
  if (list == null) {
    return '服务端没有说明这次折叠多放宽了多少（widening 缺席，按契约它恒为非 nil，'
      + '因此这是一份老响应或字段改了名）。**不得把它读作「折叠无损」** ——'
      + '粗化只会放宽，而这一屏此刻答不出放宽了多少。先弄清服务端为什么没回答。'
  }
  if (list.length === 0) return ''

  const widened = list.filter((w) => w.extraGrants > 0)
  if (widened.length === 0) {
    return `这次折叠**无损**：${list.length} 个命名空间里，每条规则原本就是该命名空间内`
      + '所有工作负载共有的，因此粗粒度与细粒度的允许面完全相同 —— 份数少了，'
      + '放行的东西一个没多。'
  }
  const total = widened.reduce((sum, w) => sum + w.extraGrants, 0)
  const detail = widened
    .slice()
    .sort((a, b) => b.extraGrants - a.extraGrants)
    .map((w) => `${w.namespace}（多 ${w.extraGrants} 份，${w.workloads} 个工作负载）`)
    .join('；')
  return `这次折叠**放宽了**：合计多出 ${total} 份授权，分布在 ${widened.length} 个命名空间 —— ${detail}。`
    + '多出来的意思是：这些命名空间里原本只有部分工作负载能走的放行，折叠之后'
    + '其中每个 Pod 都能走。要精确控制就切回 workload 粒度看这几个命名空间。'
}
