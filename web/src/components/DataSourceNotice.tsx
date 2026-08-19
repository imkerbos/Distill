import { createContext, useContext } from 'react'
import type { DataSource } from '../api/types'
import { dataSourceView } from '../pages/preconditionsView'
import { Chip, Notice } from './ui'

/**
 * 当前选中集群的数据来源。
 *
 * 值来自 AppShell 挂载时那一次集群列表请求——本轮不新增任何请求
 * （design doc 2026-08-17 §9）。列表还没落地、或选中的 id 不在列表里时是
 * `undefined`，而 dataSourceView 把 undefined 渲染成「来源未知」：这两种
 * 情况下我们确实不知道来源，显示"未知"说的是实情。
 */
const ClusterDataSourceContext = createContext<DataSource | undefined>(undefined)

export const ClusterDataSourceProvider = ClusterDataSourceContext.Provider

/**
 * 数据来源标识，放在每一屏的**内容区**。
 *
 * 不放在侧边栏的集群选择器上：那里改一处就够，但截图、导出、单屏转发的
 * 时候来源不跟着走——而"有人把一份演示报告当成生产报告"正是这条要挡的事，
 * 它发生的场合恰恰是内容离开了浏览器之后（design doc §2）。
 *
 * 因此它也渲染在取数失败与加载中的分支上：一屏"读不到数据"同样要说清
 * 它说的是哪一种集群。
 */
export default function DataSourceNotice() {
  const view = dataSourceView(useContext(ClusterDataSourceContext))

  // 演示数据与来源未知必须显著：读不出来的标注等于没有标注。真实采集
  // 则不抢镜——它是常态，用与其它元信息一致的低调形态陈述即可。
  if (view.emphasized) {
    return (
      <Notice>
        <strong>{view.label}</strong>
        <span className="ml-2">{view.detail}</span>
      </Notice>
    )
  }

  return (
    <div style={{
      display: 'flex', alignItems: 'baseline', gap: 'var(--space-2)',
      flexWrap: 'wrap', marginBottom: 'var(--space-4)',
      fontSize: 'var(--text-xs)', color: 'var(--text-muted)',
    }}>
      <Chip>{view.label}</Chip>
      <span>{view.detail}</span>
    </div>
  )
}
