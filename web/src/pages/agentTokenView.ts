import type { ClusterAgent } from '../api/types'
import { formatUtcTime } from './verifyView.ts'

/*
 * 集群 agent（推送式采集的机器身份）可见面的取数层。
 *
 * 判定集中在这一个纯函数模块里，页面只渲染返回值 —— 「这把能不能吊销」
 * 「这个集群还有几把活的」这类判断散进 .tsx 会各自漂，而漂的方向是把一把
 * 已吊销的 token 再画一个「吊销」按钮，或把「从未连接」显示成一个真实时刻。
 */

/**
 * 签发响应里那把明文 token 的一次性告示。
 *
 * 明文只在签发那一次出现，此后平台只剩哈希（规范 §19、§33）。这句话必须在
 * token 一露面时就在旁边，而不是等人问 —— 关掉面板它就再也拿不回来了。
 */
export const AGENT_TOKEN_ONCE_WARNING =
  '这把 token 只显示这一次。现在就把它交给要装 agent 的人或存进密钥管理器；'
  + '关掉后无法再查看，丢了只能重签一把、吊销这把。'

/** agent 状态到人话。封闭枚举，与后端 registry.AgentState 逐值对齐。 */
export function agentStateLabel(state: ClusterAgent['state']): string {
  switch (state) {
    case 'ACTIVE': return '可用'
    case 'REVOKED': return '已吊销'
    default: return `未登记的状态「${String(state)}」`
  }
}

/**
 * 只有 ACTIVE 的才可吊销。
 *
 * 给一把已吊销的 token 再画一个「吊销」按钮，点下去要么报错、要么是空操作，
 * 而两者都会让操作者以为自己刚刚改变了什么。
 */
export function isRevocable(a: ClusterAgent): boolean {
  return a.state === 'ACTIVE'
}

/**
 * 这个集群还有几把活的 token。
 *
 * 单独算出来摆在标题上：一把忘了吊销的 token 只有在「活的有几把」这个数字
 * 与预期对不上时才会被发现，而列表里混着历史吊销记录，光扫一眼看不出来。
 */
export function activeAgentCount(agents: readonly ClusterAgent[]): number {
  return agents.filter((a) => a.state === 'ACTIVE').length
}

/**
 * 「上次连接」这一格。
 *
 * lastSeenAt 为空表示这把 token **从未被用过** —— 与「很久以前用过」是两件事：
 * 前者是「签了但 agent 还没部署」，后者是「agent 曾经在跑」。空值渲染成
 * 「从未连接」，不渲染成任何一个时刻。
 */
export function lastSeenLabel(a: ClusterAgent): string {
  if (!a.lastSeenAt) return '从未连接'
  return formatUtcTime(a.lastSeenAt)
}
