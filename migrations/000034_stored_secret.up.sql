-- stored_secret 存平台自己保管的凭据，**只存密文**。
--
-- 与 GitRepo.CredentialRef 那条注释并不冲突：那条守的是"能不能从数据库
-- dump 出凭据"，而这里落库的是 AES-GCM 密文，加密密钥（KEK）来自启动配置、
-- 不经过数据库。拿到一份完整转储得到的是一堆解不开的字节
-- （design doc 2026-09-01 §2）。
--
-- **没有 plaintext 列，也没有回显接口。** 私钥写进去之后，除平台自己使用
-- 之外没有任何路径能取出来——包括管理员的界面。要换只能覆盖。
CREATE TABLE stored_secret (
  -- ref 复用 secrets.ValidateRef 的字符集，与另外两个后端同一套命名。
  ref        VARCHAR(64)   NOT NULL,
  -- nonce 每条独立。AES-GCM 下 nonce 重用会同时毁掉机密性与完整性，
  -- 因此它跟着行走，不从别处推导。
  nonce      VARBINARY(12) NOT NULL,
  ciphertext VARBINARY(8192) NOT NULL,
  -- key_id 记这一行是哪一把 KEK 加的。
  --
  -- 从第一天就有：没有它，轮换 KEK 时无从判断哪些行还是旧密钥加的，
  -- 只能全表试解，而"试解失败"与"数据损坏"在结果上分不开。
  key_id     VARCHAR(32)   NOT NULL,
  created_at DATETIME(6)   NOT NULL,
  updated_at DATETIME(6)   NOT NULL,
  PRIMARY KEY (ref)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
