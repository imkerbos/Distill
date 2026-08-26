# Distill 前端

TypeScript + React + Vite。**不进 Docker** —— 本机 `npm run dev` 跑 vite，
后端在 compose 里。

```bash
npm install
npm run dev        # :4000
```

## 命令

| 命令 | 内容 |
|---|---|
| `npm run dev` | vite dev server，:4000，`strictPort` |
| `npm run check` | `tsc -b` + `oxlint` + `node --test`，合并前必须全绿 |
| `npm run build` | `tsc -b && vite build` |
| `npm test` | `node --test tests/*.test.ts` |

**`npx tsc --noEmit` 在这个项目里不检查任何东西** —— 根 tsconfig 是 solution-style
（`files: []`），类型错误也会静默退出 0。类型检查一律用 `tsc -b`（即 `npm run typecheck`）。

## 结构

```
src/
  pages/      每个页面一个 *Page.tsx，加一个同名的纯函数视图模块 *View.ts
  components/ 通用组件
  api/        HTTP 客户端
  auth/       会话状态
  tokens.css  设计 token      theme.css  主题
tests/        node:test 单测，只测 *View.ts / *Form.ts 那些纯函数
```

**页面组件与视图逻辑分开**是刻意的：判定与格式化落在 `*View.ts` 里，
它们是纯函数，可以在没有 DOM 的 `node --test` 下直接测；组件只负责渲染。
新增页面时跟随这个形状。

## 访问后端

**必须走 vite proxy 访问 `/api`，不要直连 `:10100`。**

会话 cookie 是 `HttpOnly` + `SameSite`：走代理时浏览器视作同源，cookie 自然携带；
跨端口直连则需要 CORS + `SameSite=None; Secure`，而 `Secure` 在 `http://localhost`
上不生效，登录会直接失效。

代理配置在 `vite.config.ts`。

## 展示约定

- **`verdict` 与 `confidence` 分两栏展示，不合并成一个"综合分"。**
  "允许但证据不足"和"允许且证据充分"是两件事。
- `UNKNOWN` 与 `DEGRADED` 要显式显示，不得静默按"通过"渲染。
- 涉及写集群 / 写 Git 的操作，界面上默认落在 dry-run，且要明确显示当前是哪一种。
