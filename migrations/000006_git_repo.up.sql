-- 策略仓库从绑定上的三列变成独立实体（design doc 2026-08-13 §3.1、§6）。
--
-- 仓库要能在任何集群绑定它之前被登记、校验、改指向，这三件事都不该
-- 必须经由某个集群的绑定行才能发生。
--
-- 主键不含 cluster_id，这是 CLAUDE.md §4 那条规则的**有意例外**，理由写在
-- design doc §3.1 而不是留给下一个人猜：那条规则的成因是不同集群的 Pod
-- CIDR 可能重叠，缺了 cluster_id 会 join 到错误的 Pod 上且不报错。Git 仓库
-- 不是集群作用域的数据 —— 它在任何集群绑定它之前就存在，标识是 URL 而非
-- 网段。把 cluster_id 塞进它的主键，等于宣称一个仓库属于某个集群，而这
-- 正是本次拆分要消除的东西。
CREATE TABLE git_repo (
  repo_id        VARCHAR(64)  NOT NULL,
  repo_url       VARCHAR(512) NOT NULL,
  branch         VARCHAR(128) NOT NULL,
  credential_ref VARCHAR(256) NOT NULL DEFAULT '',
  verify_result  VARCHAR(32)  NOT NULL DEFAULT 'NOT_VERIFIED',
  verified_at    DATETIME(6)  NULL,
  created_at     DATETIME(6)  NOT NULL,
  updated_at     DATETIME(6)  NOT NULL,
  deleted_at     DATETIME(6)  NULL,
  PRIMARY KEY (repo_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 每条绑定搬出一个仓库，不去重（design doc §6）：当前关系就是 1:1，而
-- 去重要先断言「同一个 URL 一定是同一个仓库」，那句话在两条绑定各带一把
-- 自己的 deploy key 时并不成立 —— 合并会把两把凭据的隔离一起合掉。
--
-- repo_id 由 cluster_id 派生而不直接取 cluster_id：取值相同会诱使下一个人
-- 写出 git_repo.repo_id = cluster.cluster_id 这样的 join，而「仓库独立于
-- 集群存在」正是本次拆分的前提。带上前缀之后它只是一个不可 join 的标识串。
-- 下面两条语句用同一个表达式，因此不需要一张映射表；万一 cluster_id 长到
-- 让它超出 64 字符，两侧被同样截断、取值仍然一致，真撞上重复也是主键报错，
-- 不会静默错配。
--
-- 迁移**不做数据修正**（design doc §6）：dev 库里那个 https:// 地址搬完仍然
-- 非法，操作者会在仓库页上看到并改掉它。静默改写用户存下来的值，比留一个
-- 他看得见、也改得掉的错误更糟。
--
-- verify_result 不继承绑定上的旧结论：旧结论是仓库级与路径级压在一起得出
-- 的（design doc §3.3），把它当作仓库级的判断落下来，等于替平台声明一件它
-- 从未单独判断过的事。新行一律 NOT_VERIFIED，等一次真正的仓库级校验。
INSERT INTO git_repo
  (repo_id, repo_url, branch, credential_ref, verify_result, verified_at,
   created_at, updated_at, deleted_at)
SELECT CONCAT('repo-', cluster_id),
       repo_url,
       branch,
       COALESCE(credential_ref, ''),
       'NOT_VERIFIED',
       NULL,
       UTC_TIMESTAMP(6),
       UTC_TIMESTAMP(6),
       NULL
  FROM cluster_git_binding;

ALTER TABLE cluster_git_binding
  ADD COLUMN repo_id VARCHAR(64) NULL AFTER cluster_id;

UPDATE cluster_git_binding SET repo_id = CONCAT('repo-', cluster_id);

-- repo_id 上的 UNIQUE 保证 1:1（design doc §3.2）。不加它，模型就默默允许
-- N:1，而 N:1 会让「每绑定一把凭据」那个决定失去隔离意义 —— 同一个仓库上
-- N 把 deploy key，泄漏任何一把的爆炸半径都是整个仓库。要放开 N:1 是一次
-- 显式决定，那时删掉这个约束并重新审视凭据粒度。
--
-- 外键不带 ON DELETE CASCADE：级联会让一次仓库清理静默解除某个集群的策略
-- 下发路径（design doc §4）。仍被绑定的仓库由 SoftDeleteGitRepo 直接拒绝。
ALTER TABLE cluster_git_binding
  MODIFY COLUMN repo_id VARCHAR(64) NOT NULL,
  ADD CONSTRAINT uq_git_binding_repo UNIQUE (repo_id),
  ADD CONSTRAINT fk_git_binding_repo FOREIGN KEY (repo_id) REFERENCES git_repo(repo_id),
  DROP COLUMN repo_url,
  DROP COLUMN branch,
  DROP COLUMN credential_ref;
