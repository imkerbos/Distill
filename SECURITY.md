# 安全策略

## 报告漏洞

**不要用公开 issue 报告安全问题。**

请用 GitHub 的 [Private vulnerability reporting](https://docs.github.com/code-security/security-advisories/guidance-on-reporting-and-writing/privately-reporting-a-security-vulnerability)
（仓库 → Security → Report a vulnerability）提交。报告里请包含：

- 受影响的组件（`distill-api` / `distill-collector` / `distill-agent` / 前端）与版本或 commit
- 复现步骤，以及攻击者需要处于什么位置（未认证 / 已登录 viewer / 持有 agent token / 集群内）
- 你判断的影响面

**请不要在报告里附真实集群标识、kubeconfig、agent token 或任何生产数据。**
用占位符描述即可，我们不需要它们来复现。

我们会在 5 个工作日内确认收到。

## 支持范围

项目尚未发布版本，只有 `main` 分支接受安全修复。

## 本项目的信任边界

报告前值得知道哪些行为是**设计如此**，哪些才是漏洞。

| 组件 | 应当持有的权限 | 越界即漏洞 |
|---|---|---|
| `distill-api`（平台主服务） | 平台自身数据库、Git 只读校验、写回时向策略仓库推分支 | 持有任何集群的 NetworkPolicy 写权限；能把 kubeconfig 引用解析成可用凭据 |
| `distill-collector` | 目标集群的**只读** kubeconfig | 任何集群写操作 |
| `distill-agent`（跑在被管集群里） | 本集群 in-cluster 只读 SA；一把只能向平台写一个集群数据的 token | 二进制里存在通往平台数据库的代码路径（由 `scripts/check-push-purity.sh` 机械拦截） |

其他已知的、刻意的性质：

- 平台**从不接收**被管集群的 in-cluster 凭据。agent 用的是集群自己的 SA。
- agent token 一次性显示，只绑定一个集群，可吊销。它不是平台的读凭据。
- 会话 cookie 是 `HttpOnly` + `SameSite`；登录端点限流，其余端点全部默认拒绝 ——
  没有显式声明权限的路由会被拒绝而不是放行。
- 所有写集群 / 写 Git 的路径 dry-run 是默认值。

细节见 [docs/security-model.md](docs/security-model.md)。
