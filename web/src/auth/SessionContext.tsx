import {
  createContext, useCallback, useContext, useEffect, useMemo, useState,
  type ReactNode,
} from 'react'
import { api, onUnauthorized } from '../api/client'
import type { Identity } from '../api/types'

interface SessionValue {
  identity: Identity | null
  /** 首次探测会话期间为 true —— 此时不能判定用户未登录并跳转。 */
  loading: boolean
  login: (username: string, password: string) => Promise<void>
  logout: () => Promise<void>
}

const SessionContext = createContext<SessionValue | null>(null)

export function SessionProvider({ children }: { children: ReactNode }) {
  const [identity, setIdentity] = useState<Identity | null>(null)
  const [loading, setLoading] = useState(true)

  // 刷新页面后 Cookie 还在，但内存里的身份没了 —— 启动时探测一次，
  // 否则每次刷新都会把已登录用户踢回登录页。
  useEffect(() => {
    api.me()
      .then(setIdentity)
      .catch(() => setIdentity(null))
      .finally(() => setLoading(false))
  }, [])

  // 会话在任意请求上过期时，集中清空身份，路由守卫随即跳转。
  useEffect(() => {
    onUnauthorized(() => setIdentity(null))
  }, [])

  const login = useCallback(async (username: string, password: string) => {
    setIdentity(await api.login(username, password))
  }, [])

  const logout = useCallback(async () => {
    // 服务端调用失败（网络、5xx）也要在本地清空身份：用户点了登出，
    // 就不能让他继续以为自己还挂在一个可能已经失效的会话上。
    // 401 会经 onUnauthorized 自愈，这里兜底非 401 的失败路径。
    try {
      await api.logout()
    } finally {
      setIdentity(null)
    }
  }, [])

  const value = useMemo(
    () => ({ identity, loading, login, logout }),
    [identity, loading, login, logout],
  )

  return <SessionContext.Provider value={value}>{children}</SessionContext.Provider>
}

export function useSession(): SessionValue {
  const v = useContext(SessionContext)
  if (!v) throw new Error('useSession must be used inside SessionProvider')
  return v
}
