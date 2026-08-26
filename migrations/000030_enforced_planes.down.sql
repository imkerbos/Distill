-- 回滚：删掉这两列。回滚之后平台退回"不解释任何第二平面、探测到就整片
-- 降级"——那是**更保守**的方向，不是安全性回退。
ALTER TABLE cluster
  DROP COLUMN enforced_planes_reason,
  DROP COLUMN enforced_planes;
