# 本地起环境

## 前置

- Docker Desktop（后端跑在 compose 里）
- Node.js 20+（前端不进 Docker）
- Go 1.25（只在容器外跑 `go test` / `golangci-lint` 时需要）

## 起

```bash
# 后端 :10100 —— 容器内含完整 Go 工具链 + air 热更新，源码 bind-mount 进去
docker compose up -d

# 前端 :4000
cd web && npm install && npm run dev
```

打开 <http://localhost:4000>。

| | |
|---|---|
| 用户名 | `admin` |
| 密码 | `admin123` |

这组凭据定义在 [../configs/demo.yaml](../configs/demo.yaml)（配置里只存 bcrypt hash）。
**仅供本地 demo** —— 它配的是 fixture 数据，不应出现在任何真实环境的配置里。

首次启动会自动跑 `migrations/` 下的迁移，并种下两个 fixture 集群
（`data_source = FIXTURE`，见 [data-model.md](data-model.md)）。

## 端口

本机多项目并存，端口是固定分配的，`vite.config.ts` 里 `strictPort: true` ——
端口被占时直接失败，不静默换端口。

| 用途 | 端口 |
|---|---|
| `distill-api`（compose） | 10100 |
| 前端 vite dev server | 4000 |

MySQL **刻意不 publish 端口**。要连进去调试：

```bash
docker compose exec mysql mysql -uroot -pdistill-local distill
```

## 四个容易踩的点

1. **前端必须走 vite proxy 访问 `/api`，不要直连 `:10100`。**
   会话 cookie 是 `HttpOnly` + `SameSite=Lax`：走代理时浏览器视作同源，cookie 自然携带；
   跨端口直连则需要 CORS + `SameSite=None; Secure`，而 `Secure` 在 `http://localhost`
   上不生效，登录会直接失效。

2. **macOS 上改了后端代码没反应**，多半不是 air 坏了。Docker Desktop 的 bind mount
   经由虚拟机，fsnotify 事件会丢失或延迟数秒，所以 [`build/.air.toml`](../build/.air.toml)
   设了 `poll = true`。

3. **`npx tsc --noEmit` 在这个项目里不检查任何东西** —— 根 tsconfig 是 solution-style
   （`files: []`），类型错误也会静默退出 0。类型检查一律用 `npx tsc -b`。

4. **集成测试跑在 `distill_test` 库上，不是 `distill`。** 它会 truncate 业务表；
   打到 dev 库会清掉种子集群与全部覆盖决定。用 `make test-integration`，库名写死在
   Makefile 里，就不必每次手敲 DSN。

## 常用命令

```bash
make dev              # docker compose up --build
make dev-down         # docker compose down
make check            # lint + test + purity，合并前的门禁
make test-integration # 需要 compose 里的 mysql
```

前端：

```bash
cd web
npm run dev
npm run check && npm run build
```

## 然后呢

界面怎么用、一条策略怎么从观测走到 Git，见 [user-guide.md](user-guide.md)。
部署到真实 Kubernetes 见 [deployment.md](deployment.md)。
