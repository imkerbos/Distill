-- 回滚集群的 Kubernetes 凭据引用。
--
-- 丢的是**引用**，不是凭据：kubeconfig 从来只在 Secret Manager 里，这张表
-- 里存的一直只是它的短名。回滚之后 Secret Manager 里的每一条都原样还在，
-- 采集器连不上的原因是平台不再记得该去取哪一条，重新填一次短名即可恢复。
-- 这是这条 down 可以直接 DROP 的全部理由 —— 如果这一列存的是凭据本身，
-- 就不存在"重新填一次"这种恢复手段，那时该被质疑的是 up 而不是 down。
--
-- 列序一并还原：DROP 之后 cluster 的形状与 000009 之后逐字一致，
-- 回滚过去的旧代码读到的必须是它当初写下的那张表。
ALTER TABLE cluster
  DROP COLUMN kubeconfig_ref;
