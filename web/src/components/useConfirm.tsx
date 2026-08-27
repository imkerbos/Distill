import { useCallback, useRef, useState, type ReactNode } from 'react'

import { ConfirmDialog } from './radix'

/** 一次二次确认要问的东西。 */
export interface ConfirmRequest {
  /** 问句，比如「确认下线集群 prod-asia-1？」。 */
  title: string
  /**
   * 后果。**不是可选的。**
   *
   * 这个平台的每一处二次确认都在拦一个不可逆动作，而「你确定吗」问的是
   * 决心，操作者需要知道的是"确定之后会怎样"。类型上必填，是为了让
   * 「加一个确认框却忘了写后果」在编译期就过不去 —— 那正是下线集群
   * 那一处原先的样子。
   */
  detail: ReactNode
  /**
   * 确认按钮上的字。写动作名，别写「确定」。
   *
   * 一段说明后果的文字底下摆一个「确定」，读的人会条件反射地点它，
   * 而按钮上的字是他关掉这个框之前看到的最后一样东西。
   */
  confirmLabel: string
}

/**
 * 把二次确认变成一次 await。
 *
 * 返回 `[confirm, dialog]`：调用点写 `if (!await confirm({...})) return`，
 * 与它替换掉的 `if (!window.confirm(...)) return` 只差一个 await —— 这几处
 * 都在 async 函数里，控制流一行都不用改。把浮层的开关状态摊到每个调用点上
 * 是另一种写法，代价是四处各维护一份 open/pending 状态，而其中一处忘了在
 * 取消时清空，就会让下一次点击直接执行。
 *
 * `dialog` 要渲染在组件树里，位置随意 —— 它走 Portal。
 */
export function useConfirm(): [(req: ConfirmRequest) => Promise<boolean>, ReactNode] {
  const [request, setRequest] = useState<ConfirmRequest | null>(null)
  // resolve 放 ref 不放 state：它每次都要被换掉，而换它不该触发一次重渲染。
  const resolveRef = useRef<((ok: boolean) => void) | null>(null)

  const settle = useCallback((ok: boolean) => {
    // 先取出再清空：Promise 只能定一次，重复 resolve 是静默无效的，
    // 而"静默无效"意味着一次点击什么都没发生，读起来像界面卡住。
    const resolve = resolveRef.current
    resolveRef.current = null
    setRequest(null)
    resolve?.(ok)
  }, [])

  const confirm = useCallback((req: ConfirmRequest) => new Promise<boolean>((resolve) => {
    // 上一次还没答完就又来一次：把旧的那次当作取消，而不是丢掉它的
    // Promise —— 丢掉的话，那个 await 永远不会返回，调用它的函数
    // 就此挂住，连 finally 里的 setBusy(false) 都不会跑。
    resolveRef.current?.(false)
    resolveRef.current = resolve
    setRequest(req)
  }), [])

  const dialog = (
    <ConfirmDialog
      open={request !== null}
      title={request?.title ?? ''}
      detail={request?.detail ?? null}
      confirmLabel={request?.confirmLabel ?? ''}
      onConfirm={() => settle(true)}
      onCancel={() => settle(false)}
    />
  )

  return [confirm, dialog]
}
