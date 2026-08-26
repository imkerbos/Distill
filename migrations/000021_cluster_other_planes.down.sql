-- 回滚：去掉 other_planes 一列。
--
-- ccnp_present 不动：它在 up 里只被读、没被改，回滚之后判定退回「只认那个
-- 布尔」的老行为 —— 那是一次能力回退，不是数据损坏。
ALTER TABLE cluster DROP COLUMN other_planes;
