import type { EvidenceClass } from '../api/types.ts'

/**
 * 证据类别的中文标签。
 *
 * 裸枚举名要读的人自己去猜，而猜错的方向是把「证据可能不全」读成一句
 * 技术噪声。
 */
export const EVIDENCE_LABEL: Record<EvidenceClass, string> = {
  TRUSTED_ALLOW: '可信放行',
  TRUSTED_DENY: '当前被拒',
  INTERNET_EGRESS: '出公网',
  CROSS_CLUSTER: '跨集群',
  INCOMPLETE_WINDOW: '证据可能不全',
}

/**
 * INCOMPLETE_WINDOW 的说明。
 *
 * **只有这一类有说明**，这是刻意的：其余四类在这一轮之前就存在、语义没变，
 * 顺手给它们编一段解释会让「这一句是新的、要读」这件事被稀释掉。
 *
 * 这一类是唯一一个「规则本身没错、但可能不够」的：漏看的连接不会进候选集，
 * 覆盖它的规则于是缺席，而缺席的规则会被判「无流量、可收紧」
 * （design doc 2026-08-18-learn-from-incomplete-evidence §3）。
 */
const INCOMPLETE_WINDOW_NOTE =
  '这条规则学自一段证明不了完整的观测：平台可能没看全，因此候选集可能缺规则。'
  + '它默认不启用，要你确认之后才会进生效集与 dry-run。'

/** evidenceNote 返回该类别需要多说的一句话；不需要时为空串。 */
export function evidenceNote(evidence: EvidenceClass | undefined): string {
  return evidence === 'INCOMPLETE_WINDOW' ? INCOMPLETE_WINDOW_NOTE : ''
}
