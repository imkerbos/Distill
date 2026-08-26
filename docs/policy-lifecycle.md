# 策略是怎么生效的

一条 NetworkPolicy 从"平台看到流量"到"集群里真的拦住东西"，中间有哪些步骤、
每一步是谁做的。**平台在任何路径上都不 apply** —— 它推分支，人合并，GitOps
controller 落地。

## 0. 三个主体

| 主体 | 能做什么 | 不能做什么 |
|---|---|---|
| **平台**（distill-api） | 读集群、生成候选、预测影响、推分支到策略仓库 | apply 任何对象；持有集群 NetworkPolicy 写权限 |
| **人** | 逐条确认、合并 PR、直接写仓库、直接改集群 | —— |
| **GitOps controller**（Argo/Flux） | 把仓库里的对象 apply 进集群、prune、selfHeal | —— |

## 1. 系统推荐规则：完整链路

```mermaid
flowchart TD
    A[采集：资产 + 身份 + 流量] --> B[求值：当前策略下每条连接的 verdict]
    B --> C[生成候选集<br/>按 workload，含 5 类 Baseline]
    C --> D[dry-run 预测<br/>四类计数 + 每条变化的连接]
    D --> E{人逐条确认}
    E -->|DISABLE 某条| D
    E -->|ENABLE 某条被默认禁用的| D
    E -->|确认完| F[生成写回计划<br/>文件 + 删除项 + 指纹]
    F --> G{门禁}
    G -->|Baseline 不齐备| X1[拒绝出计划]
    G -->|名字冲突| X2[拒绝出计划]
    G -->|一条规则都没有| X3[拒绝出计划]
    G -->|通过| H[推一条 distill/* 新分支]
    H --> I{人审 PR}
    I -->|不合| X4[到此为止，集群没有任何变化]
    I -->|合并| J[GitOps controller 检测到部署分支变了]
    J --> K[apply 进集群]
    K --> L[下一轮采集看到它们]
    L --> A
```

**平台在 H 就停下**。J 与 K 不是平台做的，平台甚至不知道它们发生了没有 ——
这正是 `clusterDriftResult` 存在的理由（见 §5）。

三道门禁（G）各自拦的东西：

| 门禁 | 拦什么 | 为什么 |
|---|---|---|
| Enforcing 门禁 | 必备 Baseline 未齐备 | 每条候选都是 default-deny，缺 Baseline 会切断 DNS / 健康检查 / metrics |
| 名字冲突 | 要写的对象名已被别人占用 | `candidate-` 是保留前缀；覆盖别人的对象在 Git 历史里看不出来 |
| 空集 | 当前时间窗下一条启用规则都没有 | 空的策略文件在不同集群上含义相反 |

## 2. 人工确认（override）能做什么、不能做什么

```mermaid
flowchart LR
    R[候选规则<br/>带指纹] --> D{人的决定}
    D -->|DISABLE| R1[这条不写进文件]
    D -->|ENABLE| R2[这条写进文件<br/>仅限默认禁用的风险规则]
    D -->|想加一条平台没生成的| X[做不到]
    R1 --> P[重算 dry-run]
    R2 --> P
```

- **只能开关平台已经生成的规则。** 写入前校验指纹命中当前候选集，页面过期就拒绝。
- **BASELINE 规则不允许禁用**（`ErrBaselineNotDisablable`）：禁掉 DNS 那一条的后果
  不是"少一条规则"，是那一片彻底不通。
- **没有"手写一条规则加进候选集"的入口。** 想加自己的规则，走 §3。

## 3. 用户自定义规则：三条路径

```mermaid
flowchart TD
    U[我想让一条自己写的策略生效] --> C1[路径 A<br/>直接写策略仓库]
    U --> C2[路径 B<br/>直接 kubectl apply]
    U --> C3[路径 C<br/>通过平台导入]

    C1 --> A1[放在 distill/ 子树**之外**]
    A1 --> A2[提 PR、合并]
    A2 --> A3[GitOps apply 进集群]
    A3 --> A4[平台采集看到它<br/>进「当前策略集」，影响所有 verdict]
    A4 --> A5[写回计划里列为「策略目录下的其它文件」<br/>平台不碰它，除非人确认删除]

    C2 --> B1[集群里立刻生效]
    B1 --> B2[平台采集看到它，影响 verdict]
    B2 --> B3{名字带 candidate- 前缀？}
    B3 -->|是| B4[clusterDriftResult = CLUSTER_AHEAD]
    B3 -->|否| B5[不参与漂移判定<br/>被当成别人的东西]
    B4 --> B6[下次 GitOps 同步可能把它抹掉]

    C3 --> D1[存进 policy_import 表]
    D1 --> D2[**到此为止**]
    D2 --> D3[不进候选集、不进 dry-run、不进写回<br/>屏幕上任何数字都不会变]
```

**今天唯一真正可用的是路径 A。** 路径 C 的两个角色字段（`BASELINE_CURRENT` /
`CANDIDATE_ADDITION`）已经定义，但没有任何消费方 —— 这是一个已知缺口。

路径 B 不推荐：它绕过了 Git，仓库与集群从此不一致，而 GitOps controller 的
下一次同步可能把它抹掉（取决于它是否在 Application 的管辖范围内）。

## 4. 删除一条已经生效的策略

```mermaid
flowchart TD
    S[某个文件不在本次候选集里] --> T{平台能解析它吗}
    T -->|不能| U1[UNPARSEABLE：永不提供删除]
    T -->|能| V{它现在还在集群里吗<br/>Live}
    V -->|不在| U2[NOT_APPLIED：删掉无影响，仍需人确认]
    V -->|在| W{观测窗口锚点里有它吗<br/>InWindow}
    W -->|没有| U3[IMPACT_UNKNOWN：算不出影响，不提供删除]
    W -->|有| U4[DELETABLE：给出删除影响四类计数]
    U4 --> Y[人勾选确认]
    U2 --> Y
    Y --> Z[重新出计划，确认进指纹]
    Z --> AA[推分支：新增/更新 + 删除，同一个 commit]
    AA --> AB[人合并 → controller prune 掉集群里的对象]
```

`IMPACT_UNKNOWN` 那一支是 GitOps 下的常态时序：平台在窗口 W 出计划、人合并、
controller 在 W 之后才下发 —— 下次出计划时对象是活的，但窗口快照里还没有它。
此时按窗口口径重放，算的是"删掉一个当时并不存在的东西"，恒为无变化。

## 5. 两种漂移，回答两个不同的问题

```mermaid
flowchart LR
    P[平台最后写过的 commit] -.比对.-> R[策略仓库当前内容]
    R -.比对.-> K[集群里实际跑着的对象]
    P --- D1[driftResult<br/>仓库被别人改过吗]
    K --- D2[clusterDriftResult<br/>GitOps 落下去了吗]
```

| 组合 | 含义 |
|---|---|
| `IN_SYNC` + `CONVERGED` | 一切正常 |
| `IN_SYNC` + `PENDING` | **仓库没被动过，但 controller 没同步** —— 合并了却没生效 |
| `DRIFTED` + `CONVERGED` | 有人改了仓库，且已经生效了 |
| 任意 + `CLUSTER_AHEAD` | 集群里有平台的对象、仓库里没有：有人手工 apply 或仓库被回退 |
| 任意 + `UNKNOWN` | 平台没看全 —— **不是"已生效"** |

## 6. 什么时候判定不可信

以下任一成立，`confidence` 就是 `DEGRADED`，**这份结论不得作为收紧策略的依据**：

- 观测窗口完整度不是 `COMPLETE`（采样率未知、有丢弃）
- 集群里存在平台不解释的其它策略平面（Cilium / AdminNetworkPolicy），
  **或平台没能确认有没有**（`otherPlanes = UNKNOWN`）
- 端点在 service mesh 里，L4 身份不可信

## 7. 已知缺口

| 缺口 | 后果 |
|---|---|
| 导入的策略不进任何计算 | 平台上"贴一条策略"这个动作今天没有效果 |
| dry-run 只算候选集，不算「已有 ∪ 候选」 | 预测的世界与 apply 之后的世界不是同一个；方向偏保守，但数字不准 |
| CiliumNetworkPolicy / AdminNetworkPolicy 只检测不解释 | 那类集群只能拿到 `DEGRADED` 判定 |
| 没有"接管"路径 | 候选集生效后，被它取代的旧策略不会自动进删除清单 |
