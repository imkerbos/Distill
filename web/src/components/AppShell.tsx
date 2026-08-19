import { useEffect, useRef, useState, type ReactNode } from 'react'
import { NavLink, useNavigate } from 'react-router-dom'
import { api } from '../api/client'
import { ClusterDataSourceProvider } from './DataSourceNotice'
import { Select } from './ui'
import type { RegisteredCluster } from '../api/types'
import { useSession } from '../auth/SessionContext'
import { showsAccountAdminEntry } from '../pages/accountForm'

interface Props {
  cluster: string
  onClusterChange: (id: string) => void
  children: ReactNode
}

export default function AppShell({ cluster, onClusterChange, children }: Props) {
  const { identity, logout } = useSession()
  const [clusters, setClusters] = useState<RegisteredCluster[]>([])
  const [clustersError, setClustersError] = useState(false)
  const navigate = useNavigate()

  // 只在挂载时取一次集群列表：切换集群不会改变"有哪些集群"这个事实，
  // 把 cluster 放进依赖数组会让每次手动选择都重新拉一遍列表，纯浪费。
  // 用 ref 读最新的 cluster，避免在只运行一次的 effect 里闭包住挂载时的旧值。
  const clusterRef = useRef(cluster)
  clusterRef.current = cluster

  useEffect(() => {
    api.clusters().then((cs) => {
      setClusters(cs)
      setClustersError(false)
      if (!clusterRef.current && cs.length > 0) onClusterChange(cs[0].id)
    }).catch(() => setClustersError(true))
  }, [onClusterChange])

  // 导航按**回答什么问题**分组，不按功能名平铺。
  //
  // 十项平铺时，"网络拓扑"与"平台设置"看起来是同一量级的两件事，而前者是
  // 每天看的、后者是配一次的。分组本身就是一次信息层次的表达。
  const groups: Array<{ label: string; items: Array<{ to: string; label: string }> }> = [
    {
      label: '现状',
      items: [
        { to: '/topology', label: '网络拓扑' },
        { to: '/flows', label: '流量与判定' },
        { to: '/security', label: '安全发现' },
        { to: '/policy', label: '候选策略' },
        { to: '/quality', label: '数据质量' },
      ],
    },
    {
      label: '接入',
      items: [
        { to: '/clusters', label: '集群管理' },
        // 资产采集讲的是"平台看见了这个集群的什么"，属于接入这一段，
        // 不属于上面那五屏的判定链路。入口对所有角色都渲染 —— 服务端
        // 声明 accessAdmin，隐藏它只会让只读账号以为这块界面不存在
        // （规范 §34）。
        { to: '/collection', label: '资产采集' },
        { to: '/git-repos', label: '策略仓库' },
      ],
    },
    {
      label: '平台',
      items: [
        { to: '/settings', label: '平台设置' },
        /*
          账号管理入口只对管理员渲染。**这只是体验，不是安全** ——
          服务端对 /api/v1/accounts 下的每一个端点都声明了 accessAdmin，
          并在每次请求现读角色，只读账号把地址敲进浏览器一样是 403
          （规范 §34、design doc 2026-08-14 §8）。隐藏这一项省下的只是
          「点了才发现被拒」那一次无用点击；谁把它当成那些端点的保护，
          删掉这一行时会以为自己只是改了导航。
        */
        ...(identity && showsAccountAdminEntry(identity.role)
          ? [{ to: '/accounts', label: '账号管理' }]
          : []),
        // 改自己的密码任何角色都能做，因此不在上面那道过滤里。
        { to: '/me/password', label: '修改密码' },
      ],
    },
  ]

  return (
    <div className="grid min-h-screen grid-cols-[236px_1fr] bg-bg flex flex-col gap-4 border-r border-line bg-surface px-3 py-4 text-lg tracking-[-0.01em] text-ink font-title text-xs text-ink-muted">
      <nav>
        <div>
          <div
          >
            Distill
          </div>
          <div>NetworkPolicy 可见性</div>
        </div>

        <label className="block">
          <span className="mb-1 block text-xs text-ink-muted">集群</span>
          <Select
            value={cluster}
            ariaLabel="集群"
            onChange={onClusterChange}
            options={clusters.map((c) => [c.id, c.id] as [string, string])}
            style={{ width: '100%' }}
          />
          {clustersError && (
            <span className="mt-1 block text-xs text-deny">集群列表加载失败</span>
          )}
        </label>

        <div className="flex flex-col gap-5">
          {groups.map((g) => (
            <div key={g.label}>
              <div className="mb-[6px] px-3 text-[11px] tracking-[0.06em] text-ink-muted uppercase">
                {g.label}
              </div>
              {g.items.map((item) => (
                <NavLink
                  key={item.to}
                  to={item.to}
                  className={({ isActive }) => [
                    'block rounded-chip px-3 py-[6px] text-sm no-underline transition-colors',
                    isActive
                      // 选中态用品牌色，不用中性底：只靠底色深浅在浅色系里
                      // 几乎读不出来，使用者会不确定自己正在看哪一屏。
                      ? 'bg-brand-weak text-brand-strong'
                      : 'text-ink-muted hover:bg-sunken hover:text-ink',
                  ].join(' ')}
                  style={({ isActive }) => (
                    isActive ? { fontWeight: 'var(--weight-section)' } : undefined
                  )}
                >
                  {item.label}
                </NavLink>
              ))}
            </div>
          ))}
        </div>

        <div className="mt-auto border-t border-line px-3 pt-4 text-xs text-ink-muted">
          <div className="mb-2 truncate text-ink-2">{identity?.username}</div>
          <button
            onClick={async () => { await logout(); navigate('/login') }}
            className="rounded-chip border border-line px-2 py-1 text-xs text-ink-muted
                       hover:border-line-strong hover:text-ink"
          >
            登出
          </button>
        </div>
      </nav>

      {/*
        限制正文宽度：宽屏下表格列会被拉到一两千像素，同一行的源与目的
        相隔太远，读者无法把它们连成一条记录。
      */}
      <div className="overflow-auto px-5 py-5">
        {/*
          数据来源沿这里下发给每一屏的内容区。取的是这一次已经发生的集群
          列表请求里的字段，不为标识本身再发一次请求（design doc
          2026-08-17 §2、§9）。选中的 id 不在列表里（还没落地、或列表加载
          失败）时是 undefined，各屏据此显示「来源未知」——那时我们确实不
          知道来源。
        */}
        <ClusterDataSourceProvider value={clusters.find((c) => c.id === cluster)?.dataSource}>
          <div style={{ maxWidth: 'var(--content-max)' }}>{children}</div>
        </ClusterDataSourceProvider>
      </div>
    </div>
  )
}
