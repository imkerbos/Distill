# 安全模型

这个平台会影响生产集群的网络可达性。下面每一条约束都是为了让"最坏情况"停在可接受的位置。

## 平台不 apply 策略

**平台主服务不得持有日常 Kubernetes 策略写权限。** 出现这类代码路径视为设计错误，
不是权限配置问题。

确认后的策略走 GitOps：

```
平台生成候选 → dry-run 预览 → 人逐条确认 → 平台推分支 → 人审 PR → 合并 → Argo/Flux apply
```

平台在推分支之后停下。这样做的代价是慢一步，买到的是：策略变更有 PR 记录、有 review、
可 revert，而且**平台被攻破不等于集群被改**。

## 权限边界

| 主体 | 应当持有 | 不得持有 |
|---|---|---|
| `distill-api` | 平台数据库；Git 只读校验；写回时向策略仓库推分支的凭据 | 任何集群的 kubeconfig 解析能力；任何 NetworkPolicy 写权限 |
| `distill-collector` | 目标集群只读 kubeconfig（从凭据后端读） | 任何集群写操作 |
| `distill-agent` | 本集群 in-cluster 只读 SA；一把绑定单集群的 token | 通往平台数据库的任何代码路径 |
| viewer 角色 | 读集群、拓扑、质量、安全视图、流量与判定 | 改配置、改集群、导出、写回、账号管理 |
| admin 角色 | 上述全部 + 集群 / 账号 / 设置 / Git 绑定 / 写回 | —— |

### 机械守住的两条

- `scripts/check-push-purity.sh`（进 `make check`）：`cmd/distill-agent` 的**传递依赖图**
  里不得出现 `internal/mysqlregistry`、`internal/snapshotstore`、`github.com/go-sql-driver/mysql`
  或 `database/sql`。查传递图不查直接 import —— 直接 import 干净但中间包把它拖进来，
  编译产物里照样有。
- `internal/registry` 的 kubecred 调用点测试：主服务里没有把 kubeconfig 引用变成凭据的路径。
- 采集器启动时的只读自证（`collect.AssertReadOnly`）：用 SelfSubjectAccessReview 逐一
  问过**七类**能阻断生产流量的策略对象 —— 标准 NetworkPolicy、Cilium 的 CNP/CCNP、
  AdminNetworkPolicy 与 BaselineAdminNetworkPolicy、Calico 的 GlobalNetworkPolicy 与
  NetworkPolicy —— 任何一类拿到写动词就拒绝启动。只查标准 NetworkPolicy 会让一个持有
  CCNP 或 ANP 写权限的凭据顺利通过自检，而后两族尤其危险：它们带 Deny 动作，一条就能
  拦掉整个集群的流量。**问不出结果时同样拒绝启动** —— 连"我有没有写权限"都答不上来时，
  假定没有是这条守卫最没用的失败方向。

## 凭据处置

- **agent token**：256 bit `crypto/rand`，一次性显示，库里只存 SHA-256，可吊销。
  **集群下线时这些凭据立刻失效**，两道防线各自成立：认证层按集群是否已下线拒绝
  （对下线之前签发的那些同样生效），下线动作本身在同一个事务里把它们置为已吊销。
  少了任何一道，一个已经从每一屏消失的集群仍然在收数据，而那些凭据没有任何界面
  可以看见或吊销 —— 影响是凭据、账单与审计三条。
  不用 bcrypt —— 慢哈希买的是低熵口令的暴力破解成本，而 256 bit 随机串没有字典空间可省，
  代价却是每次摄入都加一次 bcrypt 的 CPU。
- **平台从不接收被管集群的 in-cluster 凭据。** agent 用集群自己的 SA 读，平台没见过它。
- **kubeconfig 只在 collector 进程里被解析**；主服务存引用、显示引用。
- **Git 写回凭据**是 deploy key，只对策略仓库有写权限，与集群凭据不相通。
- 真实集群标识、token、kubeconfig 一律不进仓库、不进 PR 描述、不发外部服务。

## 默认拒绝

- **路由默认拒绝**：每条受保护路由在注册的同一行声明所需权限，没有声明的路由被拒绝而不是放行。
- **登录限流**：10 次 / 分钟，键表封顶 4096；表满且回收不出空位时新键一律被拒 ——
  失效方向必须是关闭。登录是唯一无需会话的端点，也是唯一每次调用要算一次 bcrypt 的端点。
- **请求体上限按子树声明**：agent 子树（一次流量摄入 1–2 MB）与人的子树（一次登录几百字节）
  差三个数量级，装在根部只能取小的那个。
- **会话 cookie**：`HttpOnly` + `SameSite`。
- **agent 子树与人的子树并列、不嵌套**：人的会话不得成为一次摄入的身份，
  一把泄漏的 agent token 也不得成为一把能读全平台的钥匙。

## dry-run 是默认值

**任何写集群或写 Git 的代码路径必须先有 dry-run 实现，且 dry-run 是默认值。**
写回路径分两步：先记一次计划（plan），人确认后才推（push），两步都落审计。
`Writeback` 存储为 nil 时两个端点都拒绝 —— 一次留不下痕迹的写回，事后没有任何东西
能回答"谁把它推了出去"。

## 判定的严谨性

不得为提高覆盖率或让指标好看而放宽判定：

- 无法确定就返回 `UNKNOWN`；判定不可信就标 `DEGRADED`。
- `verdict` 与 `confidence` 永远是两个独立字段，不得合并 ——
  "允许但证据不足"和"允许且证据充分"是两件事，合并之后没人能把它们分开。
- `unknown_reason` 是封闭枚举，不得自由文本。新增取值要同步更新枚举与统计口径。
- Baseline 策略必须带推导依据落库，不得硬编码常量表。

## 报告漏洞

见 [../SECURITY.md](../SECURITY.md)。
