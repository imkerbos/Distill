# Distill

GKE NetworkPolicy 意图分析与安全发布平台。

当前仓库处于 demo 竖切阶段：数据来自内置 fixture，不连真实集群，不下发任何策略。

## 本地起 demo

后端跑在 Docker Compose 里（含 Go 工具链 + air 热更新），前端不进 Docker。

```bash
# 后端 :10100
docker compose up -d

# 前端 :4000
cd web && npm install && npm run dev
```

打开 <http://localhost:4000>。

| | |
|---|---|
| 用户名 | `admin` |
| 密码 | `admin123` |

这组凭据定义在 [configs/demo.yaml](configs/demo.yaml)（配置里只存 bcrypt hash）。
**仅供本地 demo** —— 它配的是 fixture 数据，不应出现在任何真实环境的配置中。

## 几个容易踩的点

- **前端必须走 vite proxy 访问 `/api`，不要直连 `:10100`。**
  会话 cookie 是 `HttpOnly` + `SameSite=Lax`：走代理时浏览器视作同源，cookie 自然携带；
  跨端口直连则需要 `SameSite=None; Secure`，而 `Secure` 在 `http://localhost` 上不生效，
  登录会直接失效。
- **macOS 上改了后端代码没反应**，多半不是 air 坏了。Docker Desktop 的 bind mount 经由虚拟机，
  fsnotify 事件会丢失，所以 `.air.toml` 设了 `poll = true`。
- 端口 `10100` / `4000` 是固定分配，`vite.config.ts` 里 `strictPort: true` ——
  端口被占时直接失败，不静默换端口。

## 开发

```bash
go build ./... && go test -race -count=1 ./...
golangci-lint run

cd web && npx tsc -b && npm run build && npm run lint
```

`npx tsc --noEmit` 在这个项目里**不检查任何东西** —— 根 tsconfig 是 solution-style（`files: []`），
类型错误也会静默退出 0。类型检查一律用 `npx tsc -b`。

## 文档

设计决策在 [docs/superpowers/specs/](docs/superpowers/specs/)。实现与 spec 冲突时先改 spec。
