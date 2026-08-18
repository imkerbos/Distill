#!/usr/bin/env bash
#
# 推送式接入的前提：装进被管集群的那个二进制不得带着平台状态库的访问路径
# （design doc 2026-08-18 §1.2）。今天的采集器直连状态库，把它原样塞进
# 客户集群等于把口令发出去。
#
# **这个检查现在只是警告，不是门禁。** 原因写在下面，不要在没有拆包的
# 情况下把它改成 exit 1 —— 那只会让所有人加 `|| true`。
set -euo pipefail

hits=$(go list -f '{{join .Imports "\n"}}' ./cmd/distill-collector |
         grep -E 'mysqlregistry|snapshotstore|go-sql-driver' || true)

if [ -z "$hits" ]; then
  echo "OK: the collector binary holds no path to the platform database"
  exit 0
fi

cat >&2 <<'WHY'
WARNING: cmd/distill-collector still links the platform's state store.

这是**预期的、已知的**：同一个二进制同时承载 pull 与 push 两种形态，而 pull
模式按设计直连状态库（design doc §1.1 —— 只给得到只读 token 的集群，
uat-k8s-cluster-01 今天就是这一类，删掉 pull 等于让唯一接进来的真集群立刻
采不了）。

要让这个检查变成硬门禁，只有一条路：把 push 拆成独立的二进制
（cmd/distill-agent），共享的采集逻辑挪进 internal/。**那是一次结构性改动，
需要人明确决定**，记在 docs/TODO.md 的 G2 条目里。

在拆包之前，push 路径不碰状态库这件事由以下几点保证，且它们都有测试：
  - runPush 的调用图里没有 mysqlregistry / snapshotstore（见 mode.go）
  - dispatch 双向拒绝：push 模式给 -cluster 会拒，pull 模式给 -platform-url 也会拒
  - push 模式的 sink 是 httpSink，它只会 POST
WHY
echo "" >&2
echo "$hits" >&2
exit 0
