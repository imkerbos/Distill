/*
 * 「命中的是哪一条策略」的展示形态。
 *
 * 判定里那一栏（Reason.matchedPolicy）同时装着三种东西：
 *
 *   payment/deny-all   一条 NetworkPolicy，namespace/name
 *   anp/tenant-iso     一条 AdminNetworkPolicy，集群级、没有 namespace
 *   banp/default       那条 BaselineAdminNetworkPolicy
 *
 * 不区分的话，一条 ANP 的命中在界面上读起来像一条 anp 命名空间里的
 * NetworkPolicy。而这三者的分量完全不同：ANP 排在标准 NetworkPolicy
 * 之前且压得过它，BANP 在它之后兜底 —— 一个操作者看到 DENY 却以为是自己
 * 命名空间里的策略拦的，会去改一份根本不起作用的 YAML。
 */

/** 判定命中的策略属于哪个平面。 */
export type PolicyPlane = 'NETWORK_POLICY' | 'ADMIN_NETWORK_POLICY' | 'BASELINE_ADMIN_NETWORK_POLICY'

export interface PolicyRefView {
  readonly plane: PolicyPlane
  /** 平面的名字，用于标签。 */
  readonly planeLabel: string
  /** 去掉前缀之后的策略名。 */
  readonly name: string
  /** 这个平面与标准 NetworkPolicy 的关系，一句话。 */
  readonly precedence: string
}

/**
 * 把一条命中引用折算成可渲染的形态。
 *
 * 前缀与后端 replay.adminPolicyRef / baselinePolicyRef 对齐。**认不出前缀时
 * 按 NetworkPolicy 处理**：那是这一栏历史上唯一的形态，而把一条 NetworkPolicy
 * 误标成管理面策略，会让人以为它压过了别的策略。
 */
export function policyRefView(ref: string): PolicyRefView {
  if (ref.startsWith('anp/')) {
    return {
      plane: 'ADMIN_NETWORK_POLICY',
      planeLabel: 'AdminNetworkPolicy',
      name: ref.slice('anp/'.length),
      precedence: '集群级策略，在命名空间自己的 NetworkPolicy 之前求值，'
        + '且结论压过它 —— 改命名空间里的策略动不了这条判定。',
    }
  }
  if (ref.startsWith('banp/')) {
    return {
      plane: 'BASELINE_ADMIN_NETWORK_POLICY',
      planeLabel: 'BaselineAdminNetworkPolicy',
      name: ref.slice('banp/'.length),
      precedence: '集群级兜底策略，只在这个主体没有被任何 NetworkPolicy 选中时才生效。'
        + '给它加一条 NetworkPolicy，这条兜底就不再参与。',
    }
  }
  return {
    plane: 'NETWORK_POLICY',
    planeLabel: 'NetworkPolicy',
    name: ref,
    precedence: '命名空间级策略。',
  }
}

/** isAdminPlane 判断这次命中是不是来自管理面策略。 */
export function isAdminPlane(ref: string): boolean {
  return policyRefView(ref).plane !== 'NETWORK_POLICY'
}
