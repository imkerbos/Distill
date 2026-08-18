#!/usr/bin/env bash
#
# 装进被管集群的那个二进制不得带着平台状态库的访问路径
# （design doc 2026-08-18 §1.2）。
#
# **查的是整个传递依赖图（.Deps），不是直接 import。** 直接 import 干净但
# 某个中间包把状态库拖进来，编译产物里照样有它 —— 而这条性质关心的正是
# 编译产物：那个可执行文件会被交到别人手里。
#
# 这是硬门禁。它能成立，是因为推送式接入拆成了独立的 cmd/distill-agent；
# 在拆分之前同一个二进制同时承载两种形态，这个检查只能是一句警告。
# **不要把 pull 那条路径合回来** —— 合回来这一条就再也守不住了。
set -euo pipefail

readonly TARGET=./cmd/distill-agent
# database/sql 精确匹配，不含它的子包：google/uuid 为了实现 Scan/Value 会
# 拖进 database/sql/driver，那是一个只有接口定义的类型包，不含任何数据库
# 访问能力。把它一并禁掉只会逼出一条 `|| true`，而那会连真正该拦的一起放过。
readonly FORBIDDEN='^(github.com/imkerbos/Distill/internal/(mysqlregistry|snapshotstore)|github.com/go-sql-driver/mysql|database/sql)$'

hits=$(go list -f '{{join .Deps "\n"}}' "$TARGET" | grep -E "$FORBIDDEN" || true)

if [ -n "$hits" ]; then
  echo "FAIL: $TARGET links the platform's state store" >&2
  echo "" >&2
  echo "这个二进制会被装进别人的集群。带着状态库的访问路径，等于把平台" >&2
  echo "数据库的连接能力一起发出去 —— 配置里那个 DSN 只差一个挂载。" >&2
  echo "" >&2
  echo "$hits" >&2
  exit 1
fi

echo "OK: $TARGET holds no path to the platform database"
