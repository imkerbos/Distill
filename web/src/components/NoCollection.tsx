import { EmptyState } from './ui'
// 文案与去处都在 pages/noCollectionView 里，这里只负责画。同
// components/Verdict.tsx 从 pages/verifyView 取词汇表：说什么与怎么画
// 分开，前者可以直接被测到，不必先有一套 DOM 测试设施。
import { noCollectionView } from '../pages/noCollectionView'

/**
 * 「这个集群还没有可用的采集数据」的空态。
 *
 * 一个组件而不是各页抄四行 JSX：五屏会在同一个状态上给出五句略有出入的话，
 * 而人会按先看到的那一屏行动。这正是它要修的那个缺陷本身的形状 ——
 * 原先各页把后端的 msg 原样渲染成一行红字，读起来像"这一屏坏了"，
 * 而那句话还不告诉你该去哪儿。
 */
export default function NoCollectionState() {
  const v = noCollectionView()
  return (
    <EmptyState
      message={v.headline}
      detail={(
        <>
          {v.action}
          {' '}
          {/*
            链接是这个组件存在的理由。正确的入口在两跳之外，而原先界面上
            没有任何东西指过去 —— 一句「请先跑一次采集」对一个刚注册完
            集群的人是死路。
          */}
          <a href={v.href} className="underline">去「{v.hrefLabel}」</a>
        </>
      )}
    />
  )
}
