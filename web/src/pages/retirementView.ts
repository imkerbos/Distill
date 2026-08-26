import type { RetirementCandidate, RetirementReport } from '../api/types.ts'

/**
 * 接管模式的展示口径（design doc 2026-08-25-existing-policies §6）。
 *
 * **平台只报告，不删。** 它对被管集群没有策略写权限，退休一条策略要么由人做、
 * 要么走 GitOps。这一屏给的是判断依据，界面上因此不该出现任何"删除"按钮。
 */

/** 一条退休判断在界面上要显示的东西。 */
export interface RetirementRow {
  /** 形如 `argocd/argocd-redis-network-policy`。 */
  label: string
  /** 结论文案。 */
  verdict: string
  /** true 表示这一条还撑着流量，界面据此上语义色。 */
  holding: boolean
  /** 支撑这个结论的数字。 */
  detail: string
}

/** 整份报告的展示视图。 */
export interface RetirementView {
  /** 能不能给出建议。 */
  available: boolean
  /** 给不出时的原因；能给出时是空串。 */
  unavailableReason: string
  rows: RetirementRow[]
  /** 清单被截断时的说明；未截断时是空串。 */
  truncationNote: string
  /** 一条策略都没有时的说明；有内容时是空串。 */
  emptyNote: string
}

/**
 * retirementView 把报告渲染成一张表。
 *
 * **还撑着流量的排在最前。** 那些才是操作者要看的：它们告诉他候选集还差什么。
 * 一份按名字排序的表格会把唯一有问题的那条排到第 40 行，而"可以退休"的那些
 * 读一眼数量就够了。
 */
export function retirementView(r: RetirementReport | null | undefined): RetirementView {
  if (r == null) {
    return { available: false, unavailableReason: '', rows: [], truncationNote: '', emptyNote: '' }
  }
  if (!r.eligible) {
    return {
      available: false,
      unavailableReason: r.ineligibleReason,
      rows: [], truncationNote: '', emptyNote: '',
    }
  }
  const rows = [...r.candidates]
    .sort((a, b) => Number(b.wouldBreak > 0) - Number(a.wouldBreak > 0))
    .map(toRow)
  return {
    available: true,
    unavailableReason: '',
    rows,
    truncationNote: r.truncated
      ? '集群里的策略数超过了平台一次能逐条评估的上限，这份清单「不完整」。'
        + '没有列出来的那些不是"平台认为它们可以留着"——它们根本没被算过。'
      : '',
    emptyNote: rows.length === 0
      ? '这个集群里没有平台之外的 NetworkPolicy —— 没有需要接管的旧策略。'
      : '',
  }
}

function toRow(c: RetirementCandidate): RetirementRow {
  if (c.wouldBreak > 0) {
    return {
      label: `${c.namespace}/${c.name}`,
      verdict: '还在撑着流量',
      holding: true,
      detail: `删掉它，这段观测里有 ${c.wouldBreak} 条连接会从通变成不通。`
        + '候选集还没有接手它管的那部分放行。',
    }
  }
  return {
    label: `${c.namespace}/${c.name}`,
    verdict: '候选集已接手',
    holding: false,
    detail: c.coveredBy > 0
      ? `删掉它，这段观测里没有连接会断。该命名空间有 ${c.coveredBy} 个主体在候选集里。`
      : '删掉它，这段观测里没有连接会断 —— 但该命名空间没有任何主体进了候选集，'
        + '这个"没影响"很可能只是因为那些主体在这段窗口里没有流量。',
  }
}

/**
 * 这一屏最要紧的一句话。
 *
 * 必须同时说清三件事，少一件都会让人做错事：
 * 平台不删、逐条结论不能叠加、以及 dry-run 看不见窗口外的流量。
 */
export const RETIREMENT_HELP =
  '平台不会删除集群里的任何策略 —— 它对被管集群没有写权限，退休要么由人做、'
  + '要么走 GitOps。下面每一条的结论只描述「单独退休它」：两条策略可能互相兜底，'
  + '各自单独删都没影响，一起删就断了。'
  + '而"没有连接会断"说的是「这段观测里」：一条只在月结那天走的放行，'
  + '在这个窗口里看不见，删掉它下个月才会表现出来。'
