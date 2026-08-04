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
    try {
      await api.logout()
    } catch {
      // 服务端登出失败（网络、5xx）不阻止本地登出：用户点了登出就必须登出。
      // 吞掉异常是刻意的 —— 让它逃逸只会变成一条没人处理的 rejection，
      // 而本地身份无论如何都要清空，下面的 finally 负责这件事。
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
