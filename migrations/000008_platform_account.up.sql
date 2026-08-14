-- 平台账号从配置文件里的单个引导账号变成库里的记录（design doc
-- 2026-08-14 §3）。
--
-- 主键不含 cluster_id，这是 CLAUDE.md §4 那条规则的**有意例外**，与
-- git_repo 同源（design doc 2026-08-14 §3）：那条规则防的是不同集群的
-- Pod CIDR 重叠导致 join 到错误的 Pod 上且不报错。账号不是集群作用域的
-- 数据 —— 它在任何集群存在之前就存在、跨全部集群生效，把 cluster_id 塞
-- 进主键等于宣称一个账号属于某个集群。
--
-- password_hash 是 bcrypt 的输出，明文不入库、不进日志、不进审计前后值、
-- 不出现在任何响应里（design doc §3，规范 §19、§20、§21）。长度按 bcrypt
-- 的输入上限取 72：读模型里根本没有这个字段，它只在「写入新哈希」「校验
-- 旧密码」这两处以参数形式出现。
--
-- disabled_at 与 deleted_at 是两件事：前者是可逆的暂停，后者是软删除。
-- 两者都不移除审计行 —— 那些行记录着这个账号做过什么。用户名不复用：
-- 软删除的行仍占着主键，新账号因此不会继承旧账号在审计里的身份。
CREATE TABLE platform_account (
  username      VARCHAR(64)  NOT NULL,
  password_hash VARCHAR(72)  NOT NULL,
  role          VARCHAR(32)  NOT NULL,   -- ADMIN | VIEWER
  disabled_at   DATETIME(6)  NULL,
  created_at    DATETIME(6)  NOT NULL,
  updated_at    DATETIME(6)  NOT NULL,
  deleted_at    DATETIME(6)  NULL,
  PRIMARY KEY (username)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- **不种任何账号。** 第一个管理员由引导账号在界面上建出来（design doc
-- §2）。种一个默认账号等于把一组公开的默认口令写进仓库，而这个仓库是
-- 公开可读的：那条口令会在任何人部署它之前就已经泄漏，且没有任何迹象
-- 表明它被用过（规范 §33）。
