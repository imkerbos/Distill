# 开发

## 质量门禁

合并前必须全绿，且必须**真实跑过**：

```bash
make check     # = lint + test + purity
```

| target | 内容 |
|---|---|
| `make lint` | `golangci-lint run ./...`（v2，配置见 `.golangci.yml`） |
| `make test` | `go test ./... -race -count=1` |
| `make purity` | `scripts/check-push-purity.sh` |
| `make build` | `go build ./...` |

`purity` 与 lint / test 并列进 `check`，因为它守的是一条**编译产物的性质**：
装进客户集群的那个二进制不得链接平台状态库 —— 而这种性质靠 review 守不住。

前端：

```bash
cd web
npm run check && npm run build
```

**`npx tsc --noEmit` 在这个项目里不检查任何东西** —— 根 tsconfig 是 solution-style
（`files: []`），类型错误也会静默退出 0。类型检查一律用 `npx tsc -b`。

## 四层测试

| 层 | 跑什么 | 需要什么 | 命令 |
|---|---|---|---|
| 单元 | 纯逻辑，含求值引擎的 golden test | 无 | `make test` |
| 集成 | MySQL 持久化层 | compose 里的 mysql | `make test-integration` |
| 一致性 | 求值结论与真实 CNI 的交叉验证 | kind + Cilium 集群 | `make conformance` |
| 端到端 | agent → 平台链路 | 本地 kind 集群 | 手动 |

### 集成测试

```bash
make test-integration
```

**跑在 `distill_test` 库上，不是 `distill`。** 测试会 truncate 业务表；打到 dev 库
会清掉种子集群与全部覆盖决定 —— 已经发生过一次，所以库名写死在 Makefile 里。

`-p 1` 是必需的：两个包清空的是同一个库里的同一批表，并行跑会互相删掉对方正在用的行，
表现为随机失败。

### 一致性测试

```bash
make conformance-up      # 起 kind + Cilium
make conformance         # 跑 harness
make conformance-down
```

未设 `DISTILL_CONFORMANCE_CONTEXT` 时，`make test` 里对应的子测试自行跳过 ——
所以 `check` 不依赖任何真实集群。

## 求值层的 golden test

`internal/replay` 是全平台正确性的根，要求接近 100% 正确，靠人工 review 保证不了。
每条 NetworkPolicy 语义规则至少一组用例：

- selector 组合
- `ipBlock` / `except`
- 命名端口
- 端口范围
- 双向判定（ingress + egress）
- 多策略 additive
- 空 selector
- 未选中的 Pod
- hostNetwork Pod

改到这一层的 PR，先加用例再改实现。

## 保持包纯净

```bash
go list -f '{{join .Imports "\n"}}' ./internal/replay | \
  grep -E 'net/http|database/sql|k8s.io/client-go|cloud.google.com|chi|koanf'
```

应无输出。同样适用于 `internal/cluster`、`internal/snapshot`。

查**直接** import，不要用 `go list -deps`：后者走整个传递图，而
`metav1 → pkg/watch → pkg/util/net → x/net/http2 → net/http` 是用 Kubernetes 类型的
必然代价，不代表本包执行 I/O。

## 依赖版本

| 项 | 版本 | 为什么钉死 |
|---|---|---|
| Go | 1.25（`go.mod` 写 `go 1.25.0`，CI pin `1.25`） | 不得被依赖上拉到 1.26 |
| `k8s.io/api` + `k8s.io/apimachinery` | v0.35.0，两者必须一致 | v0.36.x 要求 Go 1.26。**永远不用 `@latest`** |
| `golangci-lint` | v2.12.2（CI 里钉死） | 用 `latest` 会让上游发版那天把一次无关的 PR 变红，或反过来悄悄放宽某条规则 |

新增依赖等第一个真正的消费方出现时再引入 —— Go 会 tidy 掉未使用的依赖。

## CI

`.github/workflows/ci.yml`：push 到 `main` 与所有 PR 触发。带一个 MySQL 8.4 service，
跑 lint + `go test -race`。
