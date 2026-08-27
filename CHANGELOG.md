# Changelog

本文件记录对用户可见的变更。格式参考 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，
版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

项目尚未发布任何版本，全部变更记在 `Unreleased` 下。

## [Unreleased]

### Added

- **AdminNetworkPolicy 一族的求值**：采集 ANP/BANP 并按 API 定义的有序短路次序
  （ANP → NetworkPolicy → BANP）求值。默认**不解释** —— 装了 CRD 不等于 CNI 执行它，
  要解释需在集群登记里显式声明并写明理由。平台解释不了的部分判 `UNKNOWN`，不当作
  "不命中"
- 集群登记新增「CNI 真的会执行哪些第二策略平面」声明、业务周期、以及交给平台管理的
  系统命名空间；三者都要求同时给出理由
- 系统命名空间保护：默认不为 `kube-system` 一类生成候选策略
- 资产与身份采集：拉取式（`distill-collector`，只读 kubeconfig）与推送式
  （`distill-agent`，集群内 DaemonSet，含 conntrack 流量来源）两条链路
- NetworkPolicy 导入与求值回放引擎，`verdict` 与 `confidence` 分列
- 候选策略与 5 类 Baseline 生成，推导依据落库，支持 YAML 导出
- dry-run 影响预览与逐条人工确认
- Git 写回：绑定校验、写回计划、推分支（默认 dry-run）
- 账号与角色（viewer / admin）、平台设置页、集群 agent token 签发与吊销
- 平台侧生产镜像（`build/Dockerfile.api`、`build/Dockerfile.collector`）与
  Kubernetes 部署清单模板（`deploy/kubernetes/`）

### Fixed

- 集群下线不再留下可用的 agent 凭据：认证层按集群是否已下线拒绝，下线本身在同一个
  事务里吊销该集群的全部 token
- dry-run 与判定那一屏改用同一份求值模型。此前 dry-run 看不见 AdminNetworkPolicy，
  于是 `WOULD_BREAK` 会把一条已被拦住的连接算成"会被这次覆盖打断"，而那个数是写回
  门禁的判据
- 集群表单不再在保存时清空它没有携带的登记字段
- 「这个集群还没有可用的采集数据」不再渲染成读取故障，并给出去处

### Security

- `scripts/check-push-purity.sh`：机械拦截 `distill-agent` 链接平台状态库
- 采集器只读自证从 3 类策略对象扩到 7 类，补上 AdminNetworkPolicy 一族与 Calico 私有策略
- 所有受保护路由默认拒绝；登录端点限流；请求体上限按子树声明

### Upgrading

- **先更新采集身份的权限，再滚二进制。** 本版采集 `policy.networking.k8s.io` 下的
  `adminnetworkpolicies` 与 `baselineadminnetworkpolicies`。顺序反了，每一轮采集都会
  变成 `PARTIAL` —— apiserver 先判 RBAC 再解析资源，未授权的资源即使 CRD 没装也返 403。
  两种接入形态都适用，权限清单见 `docs/deployment.md`
- 本版带 schema 迁移（`distill-api` 启动时自动执行）。回滚镜像不回滚 schema，
  见 `docs/deployment.md` 的回滚一节
