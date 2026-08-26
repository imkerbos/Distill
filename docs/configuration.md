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
```

指定文件：`distill-api -config configs/demo.yaml`。

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
