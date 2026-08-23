import type { IngestSummary } from '../api/types'

/*
 * 流量摄入的取数层（design doc 2026-08-19-flow-ingest-visibility）。
 *
 * 存在的理由是用户问的那句「哪里去采集流量」—— 界面上根本没有这个位置。
 * 摄入落了库，但没有任何一屏读它，于是每一屏都空着而**说不出为什么**。
 */

/**
 * 「这个集群从来没有过任何一次流量摄入」对应的后端业务码（response.CodeNoIngestRun）。
 *
 * **按码判，不按文案判。** 之前这里是一个 `/从来没有过|从未|20009/` 的正则去
 * 匹配错误字符串，而后端那句话是「这个集群**还没有过**任何一次流量摄入」——
 * 三个关键词一个都不中。它一直是绿的，只因为落回的那条分支恰好也是对的；
 * 哪天不再恰好，屏幕上就会出现一句没有人写过的话。文案会改，码不会。
 *
 * 与 20005 分开是必需的：那一条说的是「采过资产，但这次问到的窗口没有可用
 * 数据」，处置是去补那次采集；这一条说的是「流量这条链路从来没跑过」，处置是
 * 去部署采集器或开流量日志。
 */
export const FLOW_NEVER_INGESTED_CODE = 20009

/** isNeverIngestedError 判断一次失败是不是「这个集群从未摄入过流量」。 */
export function isNeverIngestedError(e: unknown): boolean {
  return typeof e === 'object' && e !== null && 'code' in e
    && (e as { code: unknown }).code === FLOW_NEVER_INGESTED_CODE
}

export type FlowIngestCode = 'NEVER' | 'FAILED' | 'EMPTY' | 'OK' | 'NOT_WIRED'

export interface FlowIngestView {
  readonly code: FlowIngestCode
  /** 一句话说清现在是什么状态。 */
  readonly headline: string
  /** 照着这句话能做点什么。 */
  readonly action: string
}

/** 失败原因到人话与处置的映射，封闭枚举。 */
const FAILURE: Record<string, { what: string; action: string }> = {
  UNREACHABLE: {
    what: '连不上流量来源',
    action: '来源的地址或网络不通。检查采集器与来源之间的连通性，以及来源本身在不在跑。',
  },
  UNAUTHORIZED: {
    what: '流量来源拒绝了这次访问',
    action: '凭据无效或权限不足。这不是网络问题，重试不会有帮助。',
  },
  QUOTA_EXHAUSTED: {
    what: '来源侧的配额用尽',
    action: '来源限流或计费配额到顶。等待配额恢复，或调低摄入频率。',
  },
  TIMEOUT: {
    what: '这次摄入超时',
    action: '来源响应太慢或数据量太大。缩短观测窗口后重试。',
  },
  OTHER: {
    what: '这次摄入失败',
    action: '采集器判定不出更具体的原因。看采集器自己的日志。',
  },
  UNRECOGNIZED: {
    what: '这次摄入失败，而失败原因平台不认识',
    action: '库里那个取值不在封闭枚举内 —— 这是平台侧的问题，不是集群的。真实取值在服务端日志里。',
  },
}

/**
 * 把最近一次摄入翻成屏幕上那两句话。
 *
 * `null` 表示这个集群**从来没有过任何一次摄入**。
 *
 * **它与「摄入过、这段窗口没有连接」是两句不同的话。** 两者在界面上长得
 * 一模一样（都是"没有流量"），而处置完全相反：前者要去部署采集器或开流量
 * 日志，后者什么都不用做 —— 那是一句关于集群的话。塌成一句，操作者会照着
 * 错的那一半行动（design doc §3）。
 */
export function flowIngestView(latest: IngestSummary | null | undefined): FlowIngestView {
  if (latest == null) {
    return {
      code: 'NEVER',
      headline: '这个集群还没有过任何一次流量摄入',
      action: '候选策略里的基础设施规则来自资产快照，不依赖流量；但「谁在访问谁」'
        + '与「加了会拦断什么」都要等有流量才答得出。'
        + '去部署集群内的采集器（DaemonSet），或开启流量日志。',
    }
  }
  if (latest.status === 'FAILED') {
    const f = FAILURE[latest.errorReason] ?? FAILURE.OTHER
    return {
      code: 'FAILED',
      headline: `最近一次摄入失败：${f.what}`,
      action: f.action,
    }
  }
  if (latest.connections === 0) {
    return {
      code: 'EMPTY',
      headline: '最近一次摄入成功，但这段窗口里一条连接都没有',
      action: '这是一句关于集群的话：来源看过了，那段时间确实没有它能看见的连接。'
        + '若与预期不符，先确认来源覆盖的范围 —— 有些数据面（如 eBPF）不经过 netfilter。',
    }
  }
  return {
    code: 'OK',
    headline: `最近一次摄入看到 ${latest.connections} 条连接`,
    action: '',
  }
}

/**
 * 完整度到不了 COMPLETE 时，缺的是哪几项证据。
 *
 * **只报一个 UNKNOWN 不够**：操作者会以为那是平台的毛病，而它其实是来源的
 * 性质 —— Hubble 报不出采样率与丢弃数，conntrack 是轮询快照、说不出自己
 * 覆盖了多久。两者都永远到不了 COMPLETE，各有各的原因（design doc §4）。
 */
export function missingEvidence(latest: IngestSummary): readonly string[] {
  const out: string[] = []
  if (!latest.coveredKnown) {
    out.push('覆盖窗口：来源说不出自己实际覆盖了这段时间的多少')
  }
  if (!latest.sampleRateKnown) {
    out.push('采样率：来源不报采样，因此不知道看到的是全部还是一部分')
  }
  if (!latest.droppedReported) {
    out.push('丢弃数：来源不报丢弃，因此不知道有没有连接没被记下来')
  }
  return out
}
