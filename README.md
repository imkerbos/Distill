# Distill

> Kubernetes NetworkPolicy 意图分析与安全发布平台。

Distill 读取集群里**已经发生的事实**（资产、身份、流量），据此回答三个问题：

1. 现在这套 NetworkPolicy 到底放行了什么、拦住了什么；
2. 应该是什么样 —— 生成候选策略与 Baseline，每条都带推导依据；
3. 改成那样会发生什么 —— 逐条 dry-run 回放，判定与置信度分列。

**平台不 apply 策略。** 确认后的策略走 GitOps：平台推分支，人审合并，由 Argo/Flux 落地。
平台永不持有集群 NetworkPolicy 写权限 —— 这是设计约束，不是权限配置选项，
理由见 [docs/security-model.md](docs/security-model.md)。

## 状态

进行中，未发布。当前形态：

| 能力 | 状态 |
|---|---|
| 资产 / 身份采集（拉取式 + 集群内 agent 推送式） | 可用 |
| NetworkPolicy 导入与求值回放 | 可用。语义正确性由 golden test 覆盖；与真实 CNI 的交叉验证需自行跑 `make conformance`（kind + Cilium），它**不在** `make check` 里 |
| 候选策略与 Baseline 生成、YAML 导出 | 可用 |
| dry-run 影响预览、逐条人工确认 | 可用 |
| Git 写回（推分支） | 可用，默认 dry-run |
| 漂移检测、Enforcing 门禁 | 未实现 |

## 快速开始

见 [docs/getting-started.md](docs/getting-started.md)。最短路径：

```bash
docker compose up -d          # 后端 :10100（含 Go 工具链 + air 热更新）
cd web && npm install && npm run dev   # 前端 :4000
```

打开 <http://localhost:4000>，用 `admin` / `admin123` 登录（**仅本地 demo**，
见 [configs/demo.yaml](configs/demo.yaml)）。

## 仓库结构

```
cmd/                三个可执行文件：distill-api / distill-collector / distill-agent
internal/           全部实现。replay / cluster / snapshot 三个包保持零 I/O
migrations/         MySQL 迁移，每个版本 up + down 成对
web/                TypeScript + React + Vite 前端
build/              Dockerfile 与 air 配置
deploy/kubernetes/  平台侧部署清单模板
configs/            启动必需配置（运行期配置在 platform_setting 表里）
scripts/            门禁脚本
test/conformance/   对真实 kind + Cilium 集群的求值一致性测试
docs/               公开文档（docs/internal 与 docs/superpowers 不入库）
```

## 文档

| 文档 | 内容 |
|---|---|
| [docs/getting-started.md](docs/getting-started.md) | 本地起环境、登录、常见坑 |
| [docs/user-guide.md](docs/user-guide.md) | 使用手册：登记集群 → 接数据 → dry-run → 写回 Git |
| [docs/architecture.md](docs/architecture.md) | 三个二进制的职责边界、数据流、分层铁律 |
| [docs/security-model.md](docs/security-model.md) | 权限边界、凭据处置、dry-run 与 GitOps 写回 |
| [docs/data-model.md](docs/data-model.md) | 表与迁移约定，`cluster_id` 主键铁律 |
| [docs/configuration.md](docs/configuration.md) | 配置文件 vs 数据库设置，环境变量覆盖 |
| [docs/api.md](docs/api.md) | HTTP API 与认证授权模型 |
| [docs/deployment.md](docs/deployment.md) | 平台部署到 Kubernetes、采集接入、agent 与 token 处置 |
| [docs/development.md](docs/development.md) | 质量门禁、四层测试、如何跑 |
| [CONTRIBUTING.md](CONTRIBUTING.md) | 分支、提交、PR 规范 |
| [SECURITY.md](SECURITY.md) | 漏洞报告流程 |

## 许可

[Apache-2.0](LICENSE)。
