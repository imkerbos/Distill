# 配置

配置分两处，边界很明确：

| 放哪 | 放什么 | 改了之后 |
|---|---|---|
| 配置文件 / 环境变量 | **启动必需项** —— 没有它就连不上数据库或起不来进程 | 需要重启 |
| `platform_setting` 表 | 运行期配置 —— 会话 TTL、HTTP 超时、凭据后端、Git 校验超时与 SSH host keys | 设置页改，立即生效 |

数据库 DSN 留在文件里而设置在库里，不是矛盾：读设置本身就要先连上库，把 DSN 也挪进去就成环了。

## 配置文件

`configs/demo.yaml` 是本地 demo 的那份。字段：

```yaml
server:
  addr: ":10100"

auth:
  # 引导账号：**仅**为让首次登录成为可能而存在。
  # 账号本该落库，但那样会成环 —— 首次登录需要先有账号，建账号需要先登录。
  bootstrap_user:
    username: admin
    password_hash: "<bcrypt hash>"   # 明文密码永远不进配置

database:
  dsn: "root:...@tcp(mysql:3306)/distill?parseTime=true&loc=UTC"
  max_open_conns: 0
  max_idle_conns: 0
  conn_max_lifetime: 0s

log:
  level: INFO   # DEBUG / INFO / WARN / ERROR

evidence:
  # 证据记账周期。省略时为 5m；显式写 0s 关掉。
  interval: 5m
```

指定文件：`distill-api -config configs/demo.yaml`。

### `evidence.interval`

平台每隔这么久给每个 `COLLECTED` 集群做一轮派生记账，两件事：

- **证据**：把当前候选集算出来，给每条规则的 `windows` 加一、`observations` 累加。
  界面上"这条规则观察了多久"就是这个数。
- **对账**：算一次平台判定与执行面的一致率，落进对账历史，喂一致率趋势页。

两件事各带各的"记到哪个窗口了"，一件失败不影响另一件。

**它在文件里而不是设置页**，与其余运行期配置的归属不同：它描述的是这套部署
的形态。拉取式采集器每跑完一轮自己就记账；推送式接入没有那一轮循环可挂，
只能由平台按周期发起。哪一种成立是部署时决定的。

- 省略 → 补 5m。不补的话，升级上来的推送式部署会静默地停止记账，而症状是
  每条规则永远显示"刚观察到"。
- `0s` → 关掉。部署里已经有拉取式采集器在记账时用。
- 负数 → 启动失败。它是写错了，不是"关掉"。

同一个窗口不会被记两次：记账前先问库里"证据记到哪个窗口末端了"，没往前走就
跳过。**这一道不是优化**——`windows` 是 `windows + 1`，agent 停掉之后窗口不再
前进，少了它，一条规则会在无人观测的情况下看起来越来越可信。

一次记账要把整个集群的候选集算出来（600 Pod 的集群实测约二十秒），对账要再
过一遍同一个窗口的连接，周期必须远大于两者之和。

量级（600 Pod / 98 namespace / 约 9700 条连接/窗口，5 分钟周期）：

- `rule_evidence`：随候选规则数饱和，实测稳定在约 1900 行，不随时间线性增长。
- `reconciliation_run`：每轮一行，288 行/天。
- `reconciliation_subject`：**来源不报判定时不写**。conntrack 接入下每个主体都是
  `SOURCE_SILENT`，几百行长得一模一样、零信息量，而这一轮每五分钟就跑一次。
  Hubble 这类会报判定的来源照常逐主体落库。

## 环境变量覆盖

前缀 `DISTILL_`，嵌套用双下划线：

```
DISTILL_SERVER__ADDR=:10100
DISTILL_DATABASE__DSN='user:pass@tcp(host:3306)/distill?parseTime=true&loc=UTC'
DISTILL_LOG__LEVEL=DEBUG
```

真实部署用环境变量注入 DSN，**凭据不写进仓库**。

## 已迁走的键：出现即拒

下列键已经搬进 `platform_setting` 表。它们**留在配置文件里会让启动失败**，
并在报错里指出去向：

| 旧键 | 现在在哪 |
|---|---|
| `server.read_timeout` | `httpReadTimeout` |
| `server.write_timeout` | `httpWriteTimeout` |
| `server.shutdown_timeout` | `httpShutdownTimeout` |
| `auth.session_ttl` | `sessionTtl` |
| `auth.users` | `auth.bootstrap_user`（现在只有一个账号，且只为首次登录存在） |
| `secrets.project` | `secretsProject` |
| `secrets.prefix` | `secretsPrefix` |

**刻意不做兼容读取，也不做"读文件的值写进库"那种迁移。** 静默忽略是最坏的落法：
操作者改一个超时、重启、观察到毫无变化，而平台没有发出任何信号 —— 把这些键挪进
数据库正是为了终结"改了要重启"，一个被忽略的键会让它变成"改了永远不生效"。
而文件覆盖库，等于每次重启都用文件盖掉操作者在页面上的修改。

## 配置错误在启动时暴露

koanf 解析进强类型 struct，校验失败直接拒绝启动。不接受"跑起来再说，错的那部分到时候再看"。
