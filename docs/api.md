# HTTP API

前缀 `/api/v1`。响应体统一由 `internal/response` 生成。

## 认证

两条**并列、不嵌套**的链：

| 链 | 身份 | 怎么带 |
|---|---|---|
| 人 | 会话 cookie（`HttpOnly` + `SameSite`） | `POST /api/v1/sessions` 换取 |
| agent | agent token | `Authorization` 头，只能访问 `/api/v1/agent/*` |

刻意不嵌套：人的会话不得成为一次摄入的身份，一把泄漏的 agent token 也不得
成为一把能读全平台的钥匙。

## 授权

角色两种：`viewer`、`admin`。每条受保护路由在注册的同一行声明所需权限，
**没有声明的路由被拒绝而不是放行**。角色由校验器在每次请求上现读。

## 端点

### 无需会话

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/sessions` | 登录。唯一挂限流的端点：10 次 / 分钟 |

### agent 子树（token 认证）

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/agent/config` | agent 拉取自己的运行参数 |
| POST | `/agent/collection-runs` | 推一次资产采集运行 |
| POST | `/agent/flow-ingests` | 推一次流量摄入 |

资产与流量是两条独立链路，各自会失败、各自重试，因此是两条路由、两个 sink。
本部署收不下推送时端点答"依赖不可用"，**不是静默成功** —— 静默成功会让 agent
把这一轮当成已交付，那批观测就此丢了。

### 会话与账号

| 方法 | 路径 | 权限 |
|---|---|---|
| GET / DELETE | `/sessions/current` | 任何已登录会话 |
| POST | `/me/password` | 任何已登录会话（目标取自会话，路径里没有用户名） |
| GET / POST | `/accounts` | admin |
| PUT | `/accounts/{username}/role` | admin |
| POST | `/accounts/{username}/disable` \| `/enable` \| `/password` | admin |
| DELETE | `/accounts/{username}` | admin |

### 平台设置与 Git 仓库

| 方法 | 路径 | 权限 |
|---|---|---|
| GET / PUT | `/settings` | admin |
| GET / POST | `/git-repos` | admin |
| PUT / DELETE | `/git-repos/{repoID}` | admin |
| POST | `/git-repos/{repoID}/verify` | admin |

### 集群

| 方法 | 路径 | 权限 |
|---|---|---|
| GET | `/clusters` | viewer |
| POST | `/clusters` | admin |
| PUT / DELETE | `/clusters/{clusterID}` | admin |
| GET / POST | `/clusters/{clusterID}/agents` | admin |
| DELETE | `/clusters/{clusterID}/agents/{agentID}` | admin |
| PUT / DELETE | `/clusters/{clusterID}/git-binding` | admin |
| POST | `/clusters/{clusterID}/git-binding/verify` | admin |
| GET | `/clusters/{clusterID}/git-binding/drift` | admin |
| GET / POST | `/clusters/{clusterID}/policy-imports` | admin |
| DELETE | `/clusters/{clusterID}/policy-imports/{importID}` | admin |
| GET | `/clusters/{clusterID}/collection` \| `/flow-ingest` | admin |
| GET | `/clusters/{clusterID}/topology` \| `/quality` \| `/security` \| `/policy-preview` | viewer |
| GET | `/clusters/{clusterID}/policy-export` | admin |
| POST | `/clusters/{clusterID}/policy-writeback/plan` | admin |
| POST | `/clusters/{clusterID}/policy-writeback/push` | admin |
| POST / DELETE | `/clusters/{clusterID}/rule-overrides` | admin |

### 流量与判定

| 方法 | 路径 | 权限 |
|---|---|---|
| GET | `/flows` | viewer |
| GET | `/flows/{flowID}/decision` | viewer |

`/flows` 支持 `cluster`、`namespace`、`workload`、`verdict`、`confidence`、
`granularity`、`fingerprint`、`limit` 等查询参数。

## 约定

- **`verdict` 与 `confidence` 永远是两个独立字段。**
- 判不出来时 `verdict` 为 `UNKNOWN`，并给出封闭枚举的 `unknown_reason`；
  判定不可信时标 `DEGRADED`。
- 每个响应带 `X-Request-Id`，日志里同名字段可用于报障定位。
- 请求体上限按子树声明：人的子树与 agent 子树差三个数量级。
- 未命中路由答 404，方法不匹配答 405 —— 两者都不经过 handler，但同样带安全头。
