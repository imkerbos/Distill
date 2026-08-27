# 部署

两部分：**平台侧**（跑 `distill-api`，可选 `distill-collector`）与**被管集群侧**
（装 `distill-agent`）。清单模板在 [`deploy/kubernetes/`](../deploy/kubernetes/) 与
[`docs/deploy/`](deploy/)，所有 `REPLACE_ME` 都必须换掉。

---

## 一、平台侧

### 前置

- **一个 MySQL 8.4。** 集群内或托管实例都行。schema 不需要手工初始化 ——
  `distill-api` 启动时自己跑迁移。
- **一个镜像仓库。**

### 构建镜像

```bash
VERSION=$(git rev-parse --short HEAD)

docker build -f build/Dockerfile.api       --build-arg VERSION=$VERSION \
  -t <registry>/distill-api:$VERSION .

# 只有用拉取式采集才需要
docker build -f build/Dockerfile.collector --build-arg VERSION=$VERSION \
  -t <registry>/distill-collector:$VERSION .
```

运行阶段是 `gcr.io/distroless/static-debian12:nonroot`：自带公共 CA 根证书与非 root
用户，没有 shell、没有包管理器。`CGO_ENABLED=0` 是必需的而不是习惯 —— 里面没有 libc。

**镜像里必须有 `migrations/`。** `distill-api` 启动时读的是**相对路径** `migrations`
（[cmd/distill-api/main.go:73](../cmd/distill-api/main.go#L73)），不是 embed。
少了它，进程在启动的第一秒失败退出，而症状看起来像数据库连不上。`WORKDIR` 同理必须是 `/app`。

### 部署

```bash
kubectl apply -f deploy/kubernetes/00-namespace.yaml
kubectl apply -f deploy/kubernetes/10-serviceaccount.yaml
kubectl apply -f deploy/kubernetes/20-configmap.yaml

# 凭据不进 Git。30-secret.example.yaml 只是字段说明。
kubectl -n distill create secret generic distill-api-secrets \
  --from-literal=DISTILL_DATABASE__DSN='user:pass@tcp(mysql:3306)/distill?parseTime=true&loc=UTC' \
  --from-literal=DISTILL_AUTH__BOOTSTRAP_USER__USERNAME='admin' \
  --from-literal=DISTILL_AUTH__BOOTSTRAP_USER__PASSWORD_HASH='$2a$10$...'

kubectl apply -f deploy/kubernetes/40-deployment-api.yaml \
               -f deploy/kubernetes/50-service.yaml
kubectl apply -f deploy/kubernetes/60-networkpolicy.yaml   # 先按注释改 namespace 标签与 DB 位置
kubectl apply -f deploy/kubernetes/ingress.example.yaml    # 改完再 apply
```

环境变量名到配置键的映射：前缀 `DISTILL_` 去掉，双下划线变成点，全小写。
`DISTILL_DATABASE__DSN` → `database.dsn`。完整规则见 [configuration.md](configuration.md)。

### 五条不要改的

1. **`replicas: 1`。** 会话与登录限流的计数都在进程内存里。第二个副本的后果不是
   吞吐翻倍，而是一半请求带着另一个副本不认识的 cookie —— 表现为随机掉登录，
   同时限流阈值变成两倍。要横向扩，先把会话搬进共享存储；那是一次改动，不是一个副本数。
   更新策略同理用 `Recreate`：滚动更新会让新旧进程同时在线，短暂地制造出同一个局面。
2. **不给 `distill-api` 任何 RBAC。** 它不读写任何 Kubernetes 对象。给它加 Role
   是设计错误，不是权限配置问题 —— 那是平台开始持有集群权限的第一步。
3. **Ingress 必须是 https。** agent 拒绝以明文地址启动：明文让 token 在网络里裸奔，
   而那把 token 能往平台写这个集群的全部观测。
4. **Ingress 的请求体上限要放到 8m。** 一次流量摄入 1–2 MB，nginx 默认 1m 会把它拦在
   到达平台之前，agent 看到的是 413，很容易被读成"平台挂了"。
5. **内存 limit 不要压到 512Mi 以下。** Git 写回把整个策略仓库克隆进**内存**
   （go-git 的 memory storage），不落盘 —— 根文件系统只读因此是成立的，代价记在内存上。

### 探针

`/healthz` 在 `/api/v1` 之外，无需认证，只回答"进程活着"，**不检查数据库**。
数据库断了它仍然是绿的 —— 这是已知空缺，不要据此认为链路健康。
启动阶段的迁移可能要几十秒，用 `startupProbe` 兜住，就不必把 liveness 阈值放宽到失去意义。

### 升级与回滚

迁移在进程启动时执行（`m.Up()`，已是最新时不报错）。因此：

- **升级**：换镜像 tag，`Recreate` 起来，新进程自己把 schema 推上去。
- **回滚**：只回滚镜像**不一定够** —— 如果这一版带了迁移，旧代码面对新 schema
  的行为没有被测试覆盖。带迁移的版本要回滚，先用同版本镜像跑 `Rollback`
  （`internal/mysqlregistry.Rollback`）把 schema 降回去，再换镜像。
  每个版本的 `down` 脚本都有测试跑过 —— 一个从未被执行过的回滚脚本，
  在需要它的那天才第一次运行，等于没有回滚。
- **`data_source` 没有写路径，也不该有。** "把某个集群临时切回演示数据"不是一个
  安全位置，那是一句关于生产集群的假话。接线出问题时的回退手段只有一个：回滚这次部署。

### 备份

平台库存的是集群注册、采集数据、导入策略、覆盖决定与审计。**它不是策略的部署事实来源**
—— 那是 Git 仓库。所以：

- 数据库丢了，集群里跑着的策略不受影响，丢的是历史与判定依据。
- 按普通业务库做备份即可，重点是 `policy_*`（覆盖决定与写回记录）与审计表。

---

## 二、拉取式采集（distill-collector）

跑一次、采一个集群、落一次库，然后退出。**没有调度器，触发方式是手动** ——
所以清单是 Job 而不是 CronJob：

```bash
# 改 REPLACE_ME 与 -cluster 后
kubectl -n distill create -f deploy/kubernetes/70-job-collector.example.yaml
kubectl -n distill logs -f job/<生成的名字>
```

它需要目标集群的**只读 kubeconfig**，从凭据后端读（目录或 GCP Secret Manager，
在设置页配）。平台主服务存的是引用，没有任何一条把引用变成凭据的路径 ——
解析只发生在这个进程里。

#### 那个身份需要哪些权限

采集是**读什么、要什么**，没有一条多余的。少给一类，那一类会记成一次
`FORBIDDEN` 采集失败，整轮变成 `PARTIAL`：

```yaml
rules:
  - apiGroups: [""]
    resources: [namespaces, pods, nodes, services]
    verbs: [get, list]
  - apiGroups: ["discovery.k8s.io"]
    resources: [endpointslices]
    verbs: [get, list]
  - apiGroups: ["networking.k8s.io"]
    resources: [networkpolicies, ingresses]
    verbs: [get, list]
  - apiGroups: ["apps"]
    # 只为把 Pod 顺着 ownerRef 解到 Deployment，不落库。
    resources: [replicasets]
    verbs: [get, list]
  - apiGroups: ["policy.networking.k8s.io"]
    # 管理面策略。它带 Deny 且排在标准 NetworkPolicy 之前 —— 不读它，
    # 平台会把一条其实被 ANP 拦住的连接解释成放行。
    resources: [adminnetworkpolicies, baselineadminnetworkpolicies]
    verbs: [get, list]
```

**另外两组是选配，但不给会让整个集群的判定降级。** 拉取式采集还会探测集群里
有没有平台不解释的第二策略平面；探测不动时结论是「没查成」，而「没查过」与
「确认有」在可信度上同一档，于是这个集群的每一条判定都会标成 `DEGRADED`：

```yaml
  - apiGroups: ["cilium.io"]
    resources: [ciliumnetworkpolicies, ciliumclusterwidenetworkpolicies]
    verbs: [get, list]
  - apiGroups: ["crd.projectcalico.org"]
    resources: [globalnetworkpolicies, networkpolicies]
    verbs: [get, list]
```

集群没装对应 CRD 时这两组给了也无害 —— 授权与资源存不存在是两件事，见下面
那张表。

`selfsubjectaccessreviews` 不必显式授予：Kubernetes 默认把它给每一个通过认证
的主体（`system:basic-user`）。采集器启动时用它自证不持有任何策略写权限。

**升级时先更新权限，再滚二进制**，理由与 agent 那一节完全一样，见下文。

### 平台自己监听 TLS，不必依赖 Ingress

`server.tls_cert_file` 与 `server.tls_key_file` 同时给出时，`distill-api` 直接以
TLS 监听。**集群内通信同样可以、而且应该是 HTTPS** —— 它不需要公网、不需要公共
CA、不需要 Ingress，证书的 SAN 写成 `distill-api.distill.svc` 就成立，agent 那边
用 `-ca-file` 信任签它的那份。

这两项存在的理由是让「平台自己就能被安全访问」不依赖集群里装了什么。靠 Ingress
终结意味着这套部署多一个必须存在的组件，而 agent 只接受 https 地址 —— 没有它就
只剩下 `-allow-plaintext`，那等于让 agent token 明文过集群网络。

**两项必须同时给或同时不给。** 只写一半会在启动时被拒：它最坏的落法是静默退回
明文监听，一个以为自己在跑 TLS 的部署，token 却在明文过网。

留空即明文监听，那只该出现在本机开发里。

### 采集器跑在被采集集群内时：用 `tokenFile`，不要内嵌 token

采集器与目标集群同处一个集群时，kubeconfig 应当**引用投影卷**，而不是内嵌一份
`kubectl create token` 生成的静态 token：

```yaml
clusters:
  - name: target
    cluster:
      server: https://kubernetes.default.svc
      certificate-authority: /var/run/secrets/kubernetes.io/serviceaccount/ca.crt
users:
  - name: ro
    user:
      tokenFile: /var/run/secrets/kubernetes.io/serviceaccount/token
```

**内嵌 token 会过期**（`kubectl create token` 默认一小时，最长受集群配置限制），
而过期之后的症状是采集器报
`could not prove read-only access to the target cluster` —— 那句话读起来像 RBAC
配错了，会把人送去审计一份完全正常的权限。`tokenFile` 让 client-go 每次请求前
重读文件，kubelet 轮转之后自动跟上。

投影卷由 `automountServiceAccountToken`（默认开）挂在上面那个路径，不必在
Pod 里额外声明。跨集群采集拿不到这条路径，那时才需要一份带凭据的 kubeconfig。

要定时跑之前先想清楚两件事：失败重试的语义，以及并发 —— 同一个集群的两次采集
同时落库会撞 `observed_at` 主键。

---

## 三、被管集群侧（distill-agent）

跑在**被管集群**里的 DaemonSet，一个 workload 干两件事：

```
每个 Pod        轮询本节点 /proc/net/nf_conntrack，推流量
抢到 Lease 的   额外采一次整集群资产，推资产
```

每节点都采资产会让 API 压力乘以节点数，且多份数据撞 `observed_at` 主键；
只在一个 Pod 上读 conntrack 则其余节点全是盲区。这是"一个 workload"唯一正确的形状。

清单：[deploy/agent-daemonset.yaml](deploy/agent-daemonset.yaml)。

### 三个前提

1. **`hostNetwork: true`** —— `nf_conntrack` 按 network namespace 隔离，
   不共享 host netns 读到的是容器自己那张（几乎是空的）。
2. **`-platform-url` 必须是 https**，否则 agent 拒绝启动
   （本机调试要用明文得显式加 `-allow-plaintext`）。
3. **token 从 Secret 文件读，不走环境变量。**

### RBAC

只读整个集群 + 一把锁。`leases` 的三个动词是这个 agent **唯一的写权限**，
范围是一个 namespace 内的一个 Lease 对象 —— 碰不到任何工作负载、任何策略、任何 Secret。

**升级时先更新权限，再滚二进制 —— 两种接入形态都一样。** 采集范围增加一类资源时，
顺序反了会让每一轮采集都变成 `PARTIAL`。推送式改的是这份 ClusterRole，拉取式改的是
那份只读 kubeconfig 背后的身份。原因是 apiserver **先判 RBAC、再解析资源**：一个没有被授权的资源，
即使集群里根本没装它的 CRD，返回的也是 403 而不是 404。

采集器据此区分两件事，而这个区分正是它要保护的东西：

| 情况 | apiserver | 采集器记 | 为什么 |
|---|---|---|---|
| 有权限、集群没装该 CRD | 404 | 0 条，运行 `OK` | 确定的"这里不可能有这类对象" |
| 没有权限 | 403 | 一条 `FORBIDDEN` 失败，运行 `PARTIAL` | 我们并不知道有没有 |

把 403 也当成"没有"会让一次权限缺失表现成"这个集群干干净净"—— 对 ANP 这类带 Deny 的
策略平面，那意味着平台以为没人拦，实际上拦着。

### 镜像

```bash
docker build -f build/Dockerfile.agent --build-arg VERSION=$VERSION \
  -t <registry>/distill-agent:$VERSION .
```

运行阶段是 `scratch`：镜像里没有编译器、包管理器、shell —— 它们在一个只需要读 `/proc`
和发 HTTPS 的进程旁边只是攻击面。`-trimpath` 去掉构建机的绝对路径，那是部署布局信息，
不该跟着二进制进别人的集群。

平台证书由**内部 CA** 签发时用 `-ca-file` 指定；镜像里只带了公共 CA 根证书。

### 常用参数

| 参数 | 作用 |
|---|---|
| `-platform-url` | 平台地址，必须 https |
| `-token-file` | agent token 文件路径（Secret 挂载） |
| `-ca-file` | 自定义 CA 根证书 |
| `-assets-every` / `-flow-every` | 资产采集与流量推送的间隔 |
| `-conntrack-interval` / `-conntrack-polls` / `-conntrack-table` | conntrack 采样参数与表路径 |
| `-lease-name` / `-lease-namespace` | 选主用的 Lease |
| `-stale-after` / `-timeout` | 数据过期判定与单轮超时 |
| `-heartbeat-file` / `-healthcheck` | 存活探针配合用 |
| `-allow-plaintext` | **仅本地调试**：允许 http 平台地址 |

### token 处置

1. 在平台上为目标集群签发（集群页，或 `POST /api/v1/clusters/{clusterID}/agents`，admin）。
2. **一次性显示**，平台库里只留 SHA-256。丢了就吊销重发，没有找回。
3. 用 Secret 交付给集群，挂成文件；**不要写进环境变量、不要进 Git、不要进 PR 描述**。
4. 一把 token 只能向**一个集群**写数据，不能读平台的任何东西。吊销即刻生效。

---

## 部署检查清单

- [ ] `replicas: 1`，策略 `Recreate`
- [ ] `distill-api` 的 ServiceAccount 没有绑定任何 Role
- [ ] Ingress 是 https，且请求体上限 ≥ 8m
- [ ] DSN 带 `parseTime=true&loc=UTC`，不带 `multiStatements=true`
- [ ] 引导账号的密码 hash 从 Secret 注入，不在 ConfigMap 里
- [ ] NetworkPolicy 里的 ingress-controller namespace 标签与数据库位置按实际改过
- [ ] 备份覆盖 `policy_*` 与审计表
