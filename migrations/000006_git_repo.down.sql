-- 把三列搬回绑定表再删仓库表。回滚必须真的能跑：一个只写了 DROP 的 down
-- 脚本，在需要它的那天会把每个集群指向哪个仓库一起丢掉，而那一天恰恰是
-- 最不能丢的时候。
--
-- DEFAULT '' 只是为了让 NOT NULL 的列能加在已有行上，值随即被下面的 UPDATE
-- 覆盖；末尾再把默认值摘掉，形状与 000001 逐字一致 —— 回滚之后旧代码读到
-- 的必须是它当初写下的那张表，而不是一张多了默认值的近似表。
-- AFTER 是同样的理由：列序也一并还原。
ALTER TABLE cluster_git_binding
  ADD COLUMN repo_url       VARCHAR(512) NOT NULL DEFAULT '' AFTER cluster_id,
  ADD COLUMN branch         VARCHAR(128) NOT NULL DEFAULT '' AFTER repo_url,
  ADD COLUMN credential_ref VARCHAR(256) NULL AFTER policy_path;

-- credential_ref 搬回时空串还原成 NULL：000001 里这一列可空，而空串与 NULL
-- 在那张表上语义不同 —— NULL 是「没有绑定凭据」，空串会让「已配置但值为空」
-- 与「未配置」无法区分。
UPDATE cluster_git_binding b
  JOIN git_repo r ON r.repo_id = b.repo_id
   SET b.repo_url       = r.repo_url,
       b.branch         = r.branch,
       b.credential_ref = NULLIF(r.credential_ref, '');

-- 外键先于唯一索引删除：索引正被外键使用，反过来删不掉。
ALTER TABLE cluster_git_binding
  DROP FOREIGN KEY fk_git_binding_repo;

ALTER TABLE cluster_git_binding
  DROP INDEX uq_git_binding_repo,
  DROP COLUMN repo_id,
  ALTER COLUMN repo_url DROP DEFAULT,
  ALTER COLUMN branch DROP DEFAULT;

-- 没有任何集群绑定的仓库会随这张表一起消失：回滚之后「仓库」这个实体
-- 不再存在，只有绑定携带的那一份地址能被表达。审计行留在 audit_log 里，
-- 登记过什么、谁删的仍然追得到。
DROP TABLE git_repo;
