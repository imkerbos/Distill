# 架构

## 三个二进制

拆成三个可执行文件不是为了部署方便，是为了让权限边界成为**部署事实**而不是约定。

| 二进制 | 跑在哪 | 持有什么 | 职责 |
|---|---|---|---|
| `distill-api` | 平台侧 | 平台数据库、Git 只读校验凭据、写回用的策略仓库推送凭据 | HTTP API、求值回放、策略生成、写回编排 |
| `distill-collector` | 平台侧，手动触发跑一次 | 目标集群的**只读** kubeconfig | 拉取式采集：采一个集群的资产快照并落库 |
| `distill-agent` | **被管集群内**（DaemonSet） | 本集群 in-cluster 只读 SA、一把只绑定一个集群的 token | 推送式采集：采本集群资产与 conntrack 流量，推回平台 |

三条边界，每条都有机械检查而不是靠 review：

- **`distill-agent` 不得链接平台状态库。** 它会被装进别人的集群，带着状态库的访问路径
  等于把平台数据库的连接能力一起发出去。`scripts/check-push-purity.sh` 查的是整个传递
  依赖图（`.Deps`）而非直接 import —— 关心的是编译产物。这条门禁进 `make check`。
- **`distill-api` 没有把 kubeconfig 引用解析成凭据的路径。** 主服务存引用、显示引用，
  但 `internal/httpapi` 零处 import `internal/secrets`；解析只发生在 collector 进程里。
- **平台永不持有集群 NetworkPolicy 写权限。** 见 [security-model.md](security-model.md)。

## 数据流

```
                   ┌──────────────── 被管集群 ────────────────┐
                   │  distill-agent (DaemonSet)               │
                   │    in-cluster SA 只读 → 资产             │
                   │    /proc/net/nf_conntrack → 流量         │
                   └───────────────┬──────────────────────────┘
                                   │ HTTPS + agent token
                                   ▼
  只读 kubeconfig                POST /api/v1/agent/{collection-runs,flow-ingests}
  ┌────────────────┐                │
  │distill-collector│──────┐        │
  └────────────────┘      ▼         ▼
                       ┌──────────────────────┐
                       │  MySQL（平台状态库） │  资产 / Pod 身份区间 / 观测连接
                       └──────────┬───────────┘  导入策略 / 覆盖决定 / 审计
                                  │
                                  ▼
                       ┌──────────────────────┐
                       │  distill-api         │
                       │   replay 求值回放    │  verdict + confidence（两个字段）
                       │   policygen / baseline│ 候选策略，依据落库
                       │   predict dry-run    │  改成那样会发生什么
                       └──────────┬───────────┘
                                  │ 人工逐条确认
                                  ▼
                        推分支到策略仓库（Git）
                                  │
                                  ▼
                        人审 PR → 合并 → Argo/Flux apply
```

平台在最后一步**停下**。它推分支，不 apply。

## 策略平面

Kubernetes 的 NetworkPolicy 不是集群里唯一能决定连通性的东西。同一个集群上还可能
跑着 AdminNetworkPolicy 一族、Cilium 的 CNP/CCNP、Calico 的私有策略 —— 它们**都带
deny**，而标准 NetworkPolicy 没有。只按 NetworkPolicy 求值，会把一条其实被拦住的
连接判成放行，而那正是会被写进一条放行建议、下发之后才断的方向。

平台对此分三层处理：

| 层 | 回答的问题 | 答不出时 |
|---|---|---|
| 探测 | 这个集群里**有没有**别的平面的对象 | 没查成 = 未知，判定整片降级 |
| 声明 | 这个集群的 CNI **真的执行**哪些平面 | 默认一个都不声明 = 一个都不解释 |
| 求值 | 那些对象**放行或拒绝了什么** | 只有 AdminNetworkPolicy 一族做到了这一层 |

探测与声明必须分开，因为**装了 CRD 不等于执行**：实测原生 Calico v3.30.4 执行
AdminNetworkPolicy，而 Cilium 1.19 完全不实现它 —— 两种集群上都可能躺着同样的对象。
声明错的方向是危险的：按一个并不生效的平面求值，平台会以为某条连接被拦着、于是不
生成放行规则，那条连接会在下发之后才真的断。所以声明默认为空，且要求写明理由。

### AdminNetworkPolicy 的求值次序

这一族与标准 NetworkPolicy 是**有序短路**，不是叠加：

```
ANP（按 priority 升序，同一策略内按规则顺序）
  ├ Allow → 终局放行，压过 NetworkPolicy
  ├ Deny  → 终局阻断
  └ Pass  → 跳过剩余 ANP 规则，交给下一段
NetworkPolicy（照常求值）
BANP —— 只在主体没被任何 NetworkPolicy 选中时才轮到，只有 Allow / Deny
```

`Pass` 跳过的是**剩余的 ANP 规则**，不是 BANP：主体若没被任何 NetworkPolicy 选中，
执行仍然会走到 BANP。

平台还解释不了的部分一律判 `UNKNOWN`，不当作"不命中"：出向的 `nodes` peer 要节点
标签（没采），两条同 priority 的 ANP 同时选中一个主体时集群行为按 API 定义就是未
定义的。当作不命中会让一条 Deny 静默消失，而 ANP 是短路的 —— 跳过它，后面任何一条
Allow 都会变成终局结论。

## 包结构

```
internal/
  replay/          求值引擎 —— 全平台正确性的根。纯函数，零 I/O
  cluster/         网段分类与检测。纯
  snapshot/        快照数据结构与纯逻辑。纯
  fixture/         内置合成数据集（FIXTURE 集群读它）

  collect/         采集编排        collectrun/  一次采集运行的语义
  kubeclient/      client-go 封装  conntrack/   conntrack 表解析
  hubble/          Hubble 流量源   flow/        流量模型
  identity/        身份模型        identityderive/ Pod 身份区间推导

  policygen/       候选策略生成    baseline/    Baseline 策略与推导依据
  predict/         dry-run 影响预测 risk/       风险判定
  registry/        集群 / 账号 / 设置的纯类型与校验
  store/           读接口          snapshotstore/ collectstore/ mysqlregistry/  持久化实现

  httpapi/         HTTP 路由、认证、授权、限流   response/  统一响应
  auth/            会话与口令      agentauth/   agent token
  gitverify/ gitssh/ gitwrite/     Git 绑定校验与写回
  secrets/         凭据后端        settings/    运行期设置（platform_setting 表）
  config/ log/ buildinfo/          边缘层
```

### 分层铁律

**框架只能待在边缘层。** `chi` / `koanf` / `slog` 只出现在 `cmd/`、`internal/httpapi`、
`internal/config`、`internal/log`。

`internal/replay`、`internal/cluster`、`internal/snapshot` 必须保持纯净：零 I/O、
零框架依赖，只依赖标准库与纯类型包。判定方式见 [../CONTRIBUTING.md](../CONTRIBUTING.md#分层铁律)
—— 查**直接** import，不用 `go list -deps`。

## 两种接入形态

| | 拉取式（collector） | 推送式（agent） |
|---|---|---|
| 凭据方向 | 平台持有集群的只读 kubeconfig | 集群持有一把只能写自己那份数据的 token |
| 平台看得见集群凭据吗 | 是（存在凭据后端里） | **否** |
| Pod 身份区间在哪推导 | collector 自己做（它有库） | 平台侧做（agent 读不到整张区间表） |
| 适用 | 平台与集群同属一方 | 集群属于客户 / 平台不该持有其凭据 |

两条链路落的是同一批表，下游求值与生成完全共用。

### 流量来源必须按数据面选

| 数据面 | 可用来源 | 说明 |
|---|---|---|
| Cilium（eBPF） | **Hubble** | agent 的 `NODE_CONNTRACK` 在这里是瞎的，见下 |
| legacy（iptables kube-proxy） | `NODE_CONNTRACK` / VPC flow logs | conntrack 能看到 Pod 间流量 |

**`NODE_CONNTRACK` 在 eBPF 数据面上看不见 Pod 间流量。** 2026-08-24 在 kind + Cilium 1.19.5 上实测：
Pod 内 `netstat` 确有连接、Cilium 自己的 BPF CT 表里也有这几条，而节点
`/proc/net/nf_conntrack` 里一条都没有 —— eBPF 数据面绕过 netfilter。后果不是报错，是平台把
这个集群的流量读成「只有宿主网络命名空间那点」，判定全部退化成 `UNKNOWN`。

给 Cilium 集群接 agent 的 conntrack 来源，得到的是一份看起来在工作、实际什么都没覆盖的接入。

## 数据来源标记

每个集群有 `data_source` 列，取 `FIXTURE` 或 `COLLECTED`，**不由"有没有采到数据"推断**。

推断会让一次采集故障把真集群悄悄退回演示集群：操作者拿到一份写着真集群名字、
数字合理、流程走得通的完整报告，据此批准一次下发 —— 而那些连接属于一个不存在的集群。
默认值是 `COLLECTED`：标错方向的两种代价不对称，必须朝"页面上显得难看"那一边掉。
