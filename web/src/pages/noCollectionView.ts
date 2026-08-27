/*
 * 「这个集群还没有可用的采集数据」这条状态的取数层。
 *
 * 后端在这条路径上给的是 HTTP 200 加业务码 20005，文案是
 * 「该集群还没有可用的采集数据，请先跑一次采集与流量摄入」。各页原样把它
 * 渲染成一行红字，于是它读起来像一次读取故障；而更要命的是**那句话不告诉
 * 你去哪儿跑**——界面上没有链接、没有按钮，一个刚注册完集群的人就断在这里。
 *
 * 与 flowIngestView 分成两个文件：那一条说的是「流量这条链路从来没跑过」，
 * 这一条说的是「资产或这段窗口的数据不可用」，处置不同 —— 前者去部署采集器
 * 或开流量日志，后者要先看这个集群到底有没有采集过。
 */

/**
 * 「还没有可用的采集数据」对应的后端业务码（response.CodeNoUsableCollection）。
 *
 * **按码判，不按文案判**，与 FLOW_NEVER_INGESTED_CODE 同一条纪律：文案会改，
 * 码不会。拿字符串匹配的那一天，后端改一个字，这一屏就退回去显示红字报错，
 * 而且没有任何东西会变红。
 */
export const NO_USABLE_COLLECTION_CODE = 20005

/** isNoUsableCollectionError 判断一次失败是不是「这个集群还没有可用的采集数据」。 */
export function isNoUsableCollectionError(e: unknown): boolean {
  return typeof e === 'object' && e !== null && 'code' in e
    && (e as { code: unknown }).code === NO_USABLE_COLLECTION_CODE
}

export interface NoCollectionView {
  /** 一句话说清现在是什么状态。 */
  readonly headline: string
  /** 照着这句话能做点什么。**必须说出去哪儿**，不能只说"去跑一次"。 */
  readonly action: string
  /** 下一步该去的那一屏。 */
  readonly href: string
  /** 那一屏的名字，用作链接文字。 */
  readonly hrefLabel: string
}

/**
 * 把这条状态翻成屏幕上那两句话加一个去处。
 *
 * href 是这个模块存在的全部理由。原先每一屏说的都是「请先跑一次采集与流量
 * 摄入」—— 一句正确、但走不通的话：正确的入口在「资产采集」那一屏，以及
 * 集群管理里那一行的 agent 按钮，而这两处都在两跳之外，界面上没有任何东西
 * 指过去。
 */
export function noCollectionView(): NoCollectionView {
  return {
    headline: '这个集群还没有可用的采集数据',
    action: '候选策略、拓扑与判定都要先有一次成功的资产采集才答得出来。'
      + '去「资产采集」看这个集群跑过几次、结果是什么；一次都没跑过的话，'
      + '在「集群管理」里那一行点「agent」签一把 token，装进被管集群的 DaemonSet。',
    href: '/collection',
    hrefLabel: '资产采集',
  }
}
