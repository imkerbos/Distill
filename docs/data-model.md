# 数据模型与迁移

平台库存的是：集群注册、采集到的资产与身份、观测连接、导入策略、覆盖决定与审计。
**它不是策略的部署事实来源** —— 部署事实来源是 Git 仓库。

## 第一铁律：主键必须含 `cluster_id`

**所有身份类表主键必须含 `cluster_id`。** 新增表时这是 review 的第一检查项。

不同集群的 Pod CIDR 可能重叠。缺了这一列，A 集群的 Pod 会 join 到 B 集群的 Pod 上，
**且不报任何错** —— 症状要到求值阶段才冒出来，表现为一条毫无道理的判定。

同理，涉及历史查询的代码 join 键必须是 `(cluster_id, ..., timestamp)`：
**禁止用当前状态解释历史数据**。对账类 join 一律用 workload 而非 pod、用 identity 而非 IP。

## 迁移约定

`migrations/` 下按 `NNNNNN_name.{up,down}.sql` 成对存放，golang-migrate 执行。

- **每个版本必须 up + down 成对，且 down 真的能回滚。**
- **不改动任何已被应用过的迁移。** golang-migrate 只按版本号判断某个库跑过哪一版：
  改一个已被记录的版本，那个库永远不会重跑修正后的文件，于是它与从零建起来的测试库
  静默分叉 —— 而分叉本身不报错。要改就加新的一版。
- 封闭枚举**存字符串**（`VARCHAR`），由 Go 侧的 `Valid()` 把关，不用 MySQL `ENUM`：
  多一种表达方式只会让"枚举在哪里定义"有两个答案。
- 默认值要朝"错了立刻看得见"的方向掉。例：`cluster.data_source` 默认 `COLLECTED`
  而非 `FIXTURE` —— 前者错了页面显示"还没有可用的采集数据"，后者错了是一份看不出问题的假报告。

## 主要实体

| 表族 | 内容 |
|---|---|
| `cluster` | 集群注册、数据来源、kubeconfig 引用、CNI 事实 |
| `cluster_agent` | 集群 agent 的机器身份，token 只存 SHA-256 |
| `cluster_metrics_scraper` / `cluster_node_agent` | Baseline 推导所需的依据资产 |
| `observed_asset` / `observed_pod_*` | 采集到的资产快照 |
| `pod_identity_interval` | Pod 身份的时间区间 —— 六个读方法都从这张表出发 |
| `observed_connection` | 观测到的连接 |
| `observed_admin_policy` | 采集到的 ANP / BANP 原文，**只存不解释** |
| `collection_run` / `identity_derive_run` | 采集与推导的运行记录（状态、错误原因） |
| `policy_*` | 导入策略、覆盖决定、写回计划与推送记录 |
| `platform_account` / `platform_setting` | 账号与运行期设置 |

`policy_*` 系列表保留 `plane` 字段，当前取值仅 `networkpolicy`。

`observed_admin_policy` 与 `observed_network_policy` 刻意分表。ANP 一族是集群级的、带优先级、
且求值次序与标准 NetworkPolicy 完全不同 —— ANP 在前，BANP 在后。混进一张表要靠一个字段区分
两者，而那个字段一旦漏判，一条兜底规则就会被当成前置规则解释，方向恰好相反。

同理，`priority` 与 `priority_known` 是两列。0 是合法且**最高**的 ANP 优先级，拿 0 兼表
"没读到"，会把一条读不懂的策略排到所有策略之前。

## 时间

DSN 必须带 `parseTime=true` 与 `loc=UTC`：缺前者时 `DATETIME` 以 `[]byte` 读出，
缺后者时驱动按本地时区解释 —— 两者都让时间列静默错位，而错位的审计时间在复盘时毫无价值。

运行时连接池的 DSN **刻意不带 `multiStatements=true`**：那是迁移专用连接的能力
（见 `mysqlregistry.migrationDSN`），运行时开着它只会把任何一次注入的影响面
从单条语句放大成任意语句链。

## 默认时间窗

**没有全局默认时间窗，不要加回来** —— 包括加成一个"最近 N 小时"的配置项。
那样的取值要有人填对才安全，而填错了没有症状：填长了扫过量数据，填短了漏掉真实流量
并把它读成"这条规则没有流量、可以收紧"。默认窗口由 `store.Reader.DefaultWindow`
按集群现答，走与六个读方法相同的按来源分派。
