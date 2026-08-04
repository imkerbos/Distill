import { useEffect, useRef, useState } from 'react'

export interface ResourceState<T> {
  data: T | null
  error: string
  loading: boolean
}

/**
 * useResource 统一处理"请求还没落地时 key 已经变了"这一类竞态。
 *
 * 四个页面（拓扑、流量、质量、判定抽屉）原先各自手写同一个 useEffect
 * 形状：换 key（切集群、改筛选、点新的一行）时旧请求并不会消失，如果
 * 它比新请求晚落地，`.then(setState)` 就会用旧集群/旧流量的数据覆盖
 * 已经写好的新状态——界面上标签是新的，数据却是旧的，而且没有任何
 * 迹象提示这一点。这里用闭包里的 `cancelled` 标记替代四份手写判断：
 * key 变化或组件卸载都会先跑上一次的 cleanup 把它标记为已取消，
 * 之后落地的 .then/.catch 一律不再碰 state。
 *
 * fetcher 不放进依赖数组——它几乎总是每次渲染都重新创建的闭包，放进
 * 依赖数组会让 effect 在 key 不变时也重跑。用 ref 拿最新闭包即可，
 * 真正决定"要不要发起新请求"的只有 key 本身。
 */
export function useResource<T>(key: string, fetcher: () => Promise<T>): ResourceState<T> {
  const [state, setState] = useState<ResourceState<T>>({ data: null, error: '', loading: false })
  const fetcherRef = useRef(fetcher)
  fetcherRef.current = fetcher

  useEffect(() => {
    let cancelled = false

    if (!key) {
      setState({ data: null, error: '', loading: false })
      return
    }

    setState({ data: null, error: '', loading: true })

    fetcherRef.current()
      .then((data) => {
        if (cancelled) return
        setState({ data, error: '', loading: false })
      })
      .catch((e) => {
        if (cancelled) return
        setState({ data: null, error: String(e.message ?? e), loading: false })
      })

    return () => {
      cancelled = true
    }
  }, [key])

  return state
}
