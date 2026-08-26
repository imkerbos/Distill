-- 回滚：删掉 CNI 列。回滚之后界面上少一个事实，判定不受影响 ——
-- 平台从不拿 CNI 做判断，那一列只供人读。
ALTER TABLE cluster DROP COLUMN cni;
