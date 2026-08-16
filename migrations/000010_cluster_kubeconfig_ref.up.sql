-- 集群的 Kubernetes 凭据引用（design doc 2026-08-16 §3.5）。
--
-- 存的是 Secret Manager 里的短名，**kubeconfig 本身不进这张表，也不进
-- 这个库的任何一张表**。形状照抄 git_repo.credential_ref：一个能从平台
-- 数据库 dump 出集群凭据的设计，等于把整个 fleet 的信任根搬进平台。
-- 这一列比 Git 那一列更重 —— kubeconfig 携带的是能对 apiserver 说话的
-- 身份，而不是一个仓库的读权限。
--
-- 长度与字符集不由这一列把关：secrets.ValidateRef 限死 1..64 个小写字母、
-- 数字与连字符（registry.ValidateCluster 调它）。VARCHAR(256) 与
-- git_repo.credential_ref 逐字一致，两列存的是同一种东西，形状不该分叉。
--
-- NOT NULL DEFAULT '' 而不是 NULL：这张表上"未配置"只有空串一种写法，
-- 与 git_repo.credential_ref 相同。cluster_git_binding 当年那条
-- NULL/空串的区分随 000006 一起消失了，不在这里复活它。
-- 已存在的集群一律落成空串 —— 空引用是合法的登记状态（集群可以先登记、
-- 凭据稍后再配），不是一个待修的缺陷。
ALTER TABLE cluster
  ADD COLUMN kubeconfig_ref VARCHAR(256) NOT NULL DEFAULT '' AFTER onboard_state;
