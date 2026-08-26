# Distill 文档

| 文档 | 读它来回答 |
|---|---|
| [getting-started.md](getting-started.md) | 我怎么在本机把它跑起来 |
| [user-guide.md](user-guide.md) | 登录之后一步步怎么用，直到把策略推进 Git |
| [policy-lifecycle.md](policy-lifecycle.md) | 策略怎么生效：系统推荐 / 用户自定义 / 删除 / 漂移，各条路径的流程图 |
| [architecture.md](architecture.md) | 三个二进制各自负责什么，数据怎么流 |
| [security-model.md](security-model.md) | 谁持有什么权限，凭据怎么处置，为什么平台不能 apply |
| [data-model.md](data-model.md) | 表长什么样，迁移怎么写，为什么主键必须含 `cluster_id` |
| [configuration.md](configuration.md) | 哪些配置在文件里，哪些在数据库里，怎么覆盖 |
| [api.md](api.md) | HTTP 接口有哪些，认证与授权怎么做 |
| [deployment.md](deployment.md) | 集群 agent 怎么装，token 怎么处置 |
| [development.md](development.md) | 门禁怎么跑，四层测试各测什么 |

## 不在这里的东西

`docs/internal/` 与 `docs/superpowers/` **不入库**（见 `.gitignore`）：那里放的是
需求稿、实施计划、真实集群的操作记录与采集产物。它们含真实集群标识，只留在本机。

`.gitignore` 对 `docs/` 采取**默认拒绝**：新增公开文档需要显式加白名单，
新增内部文档什么都不用做。方向是刻意的 —— 漏加白名单的代价是文档没进库，
漏加忽略的代价是集群标识进了公开仓库。
