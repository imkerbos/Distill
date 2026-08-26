-- 回滚：删掉这两列。回滚之后平台恢复成"为所有命名空间生成候选策略"，
-- 包括 kube-system —— 那是一次**安全性回退**，不是数据损坏。
ALTER TABLE cluster
  DROP COLUMN managed_system_namespaces_reason,
  DROP COLUMN managed_system_namespaces;
