# 贡献指南

这个平台会向生产 Kubernetes 集群推送网络策略建议。一次错误的推荐或一次错误的回放结论，
会直接变成生产阻断。下面的规则据此制定 —— 它们不是风格偏好。

## 开发流程

```
feat/* 分支 → lint + test 全绿 → PR → 合并 → 回归
```

- **不在 `main` 上直接改。** 一律拉 `feat/*`（或 `fix/*` / `chore/*`）分支。
- 新增功能必须带测试；修 bug 必须**先写复现测试**，再改实现。
- 不允许"先合了再修"。

## 质量门禁

合并前必须真实跑过，并在 PR 描述里贴输出：

```bash
make check          # = lint + test + purity
cd web && npm run check && npm run build
```

`npx tsc --noEmit` 在本仓库**不检查任何东西**（根 tsconfig 是 solution-style，`files: []`），
类型检查一律用 `npx tsc -b`。详见 [docs/development.md](docs/development.md)。

## 提交信息

- [Conventional Commits](https://www.conventionalcommits.org/)，英文，subject ≤ 72 字符。
- 正文解释**为什么**，不复述 diff 做了什么。

```
feat(replay): treat named ports as unresolved when the pod spec is stale
fix(baseline): derive LB health check for LoadBalancer/NodePort services
```

## 代码约定

- 单个文件保持单一职责。文件变大是职责过多的信号，不是需要更多注释的信号。
- 注释解释**为什么**，不解释**做了什么**。
- 新代码风格跟随周边既有代码：命名、注释密度、惯用法保持一致。
- 求值引擎、数据采集、策略生成、发布流程之间通过明确接口通信，**不共享内部状态**。

### 分层铁律

框架只能待在边缘层。`chi` / `koanf` / `slog` 只出现在 `cmd/`、`internal/httpapi`、
`internal/config`、`internal/log`。

以下包必须保持纯净 —— 零 I/O、零框架依赖：

- `internal/replay` —— 求值引擎，全平台正确性的根
- `internal/cluster` —— 网段分类与检测
- `internal/snapshot` —— 快照数据结构与纯逻辑

判定方式是查**直接** import，不是 `go list -deps`：

```bash
go list -f '{{join .Imports "\n"}}' ./internal/replay | \
  grep -E 'net/http|database/sql|k8s.io/client-go|cloud.google.com|chi|koanf'
```

应无输出。`go list -deps` 走整个传递图，而 `metav1 → pkg/watch → pkg/util/net → x/net/http2 → net/http`
是用 Kubernetes 类型的必然代价，不代表本包执行 I/O。

## 正确性约束

改到下面这些地方时，PR 会被按更高标准 review：

- **求值层必须有 golden test。** 每条 NetworkPolicy 语义规则至少一组用例：
  selector 组合、ipBlock/except、命名端口、端口范围、双向判定、多策略 additive、
  空 selector、未选中 Pod、hostNetwork Pod。这一层靠人工 review 保证不了。
- **任何写集群的代码路径必须先有 dry-run 实现，且 dry-run 是默认值。**
- **不得为提高覆盖率或指标好看而放宽判定严谨性。** 无法确定就返回 `UNKNOWN`，
  判定不可信就标 `DEGRADED`。
- `verdict` 与 `confidence` 永远是两个独立字段，不得合并。
- `unknown_reason` 必须是封闭枚举，不得使用自由文本。新增原因要同步更新枚举与统计口径。
- Baseline 策略必须带推导依据落库，不得硬编码常量表。

## 数据层

- **所有身份类表主键必须含 `cluster_id`。** 不同集群 Pod CIDR 可能重叠，缺失会 join 到
  错误的 Pod 上**且不报错**。新增表时这是 review 的第一检查项。
- schema 变更必须带迁移脚本，且要能回滚（up + down 成对）。
- 涉及历史查询的代码，join 键必须是 `(cluster_id, ..., timestamp)`，禁止用当前状态解释历史数据。
- 对账类 join 一律用 workload 而非 pod、用 identity 而非 IP。

## 成本

任何新增日志采集或新增查询路径，PR 描述里必须给出**量级估算**。
这个平台的失控方向是账单，不是性能。新增 BigQuery 查询要说明分区裁剪策略。

## 依赖

- Go 版本固定 1.25，**不得被依赖上拉到 1.26**。
- `k8s.io/api` 与 `k8s.io/apimachinery` 锁在 **v0.35.0**，两者版本必须一致。
  **永远不用 `@latest`** —— v0.36.x 要求 Go 1.26。
- 选型已定不等于现在就加进 `go.mod`：等第一个真正的消费方出现时再引入，
  并在 PR 描述里说明用途。

## 文档

设计决策写进 `docs/`（内部设计稿在不入库的 `docs/internal/`、`docs/superpowers/specs/`），
不散落在代码注释里。**与文档冲突的实现，先改文档再改代码，不允许静默偏离。**
