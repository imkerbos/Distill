# Changelog

本文件记录对用户可见的变更。格式参考 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，
版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

项目尚未发布任何版本，全部变更记在 `Unreleased` 下。

## [Unreleased]

### Added

- 资产与身份采集：拉取式（`distill-collector`，只读 kubeconfig）与推送式
  （`distill-agent`，集群内 DaemonSet，含 conntrack 流量来源）两条链路
- NetworkPolicy 导入与求值回放引擎，`verdict` 与 `confidence` 分列
- 候选策略与 5 类 Baseline 生成，推导依据落库，支持 YAML 导出
- dry-run 影响预览与逐条人工确认
- Git 写回：绑定校验、写回计划、推分支（默认 dry-run）
- 账号与角色（viewer / admin）、平台设置页、集群 agent token 签发与吊销
- 平台侧生产镜像（`build/Dockerfile.api`、`build/Dockerfile.collector`）与
  Kubernetes 部署清单模板（`deploy/kubernetes/`）

### Security

- `scripts/check-push-purity.sh`：机械拦截 `distill-agent` 链接平台状态库
- 所有受保护路由默认拒绝；登录端点限流；请求体上限按子树声明
