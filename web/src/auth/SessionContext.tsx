import {
  createContext, useCallback, useContext, useEffect, useMemo, useState,
  type ReactNode,
} from 'react'
import { api, onUnauthorized } from '../api/client'
import type { CurrentSession } from '../api/types'

interface SessionValue {
  /**
   * 当前身份与它此刻生效的角色，**全部来自服务端的当前会话端点**。
   *
   * 角色不来自登录响应（那里没有角色）、不来自 Cookie（HttpOnly 读不到）、
   * 也不来自任何本地存储 —— 界面因此没有任何一条能自称管理员的路径
   * （design doc 2026-08-14 §8，规范 §34）。
   */
  identity: CurrentSession | null
  /** 首次探测会话期间为 true —— 此时不能判定用户未登录并跳转。 */
  loading: boolean
  login: (username: string, password: string) => Promise<void>
  logout: () => Promise<void>
}

const SessionContext = createContext<SessionValue | null>(null)

export function SessionProvider({ children }: { children: ReactNode }) {
  const [identity, setIdentity] = useState<CurrentSession | null>(null)
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

  // 登录成功后再问一次当前会话，而不是把登录响应直接存下来：登录响应里
  // 只有用户名，**没有角色**（handleCreateSession 刻意只回身份）。要角色
  // 就只能来自当前会话端点，那里回的是服务端本次授权判定用过的那一个。
  // 少了这一次请求，刚登录的人会一直是「角色未知」，直到他刷新页面。
  const login = useCallback(async (username: string, password: string) => {
    await api.login(username, password)
    setIdentity(await api.me())
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
