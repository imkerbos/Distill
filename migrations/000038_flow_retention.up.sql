-- 每个集群的流量保留水位：这个时刻**之前**的连接已经被清理掉了。
--
-- 存下来而不是按"现在减保留期"现算：清理是分批跑的，可能跑到一半停下
-- （记账停摆、进程重启、磁盘告急）。现算出来的水位会声称删干净了一段其实
-- 还在的数据，而反过来 —— 读取端据此报"已清理"，那段数据却查得到 ——
-- 两个方向都是在编造一个关于自己的事实。
--
-- **它的用处不是清理，是读取。** 清理之后再查那段时间会得到零条连接，
-- 而零条与"那时确实没有流量"长得一模一样；后者是下游会当作事实使用的结论
-- （与 identity 的 NOT_COVERED / NO_DATA 那条纪律逐字相同）。有了这个水位，
-- 读取端才答得出"这段被清理了"而不是"这段没有流量"。
--
-- 主键含 cluster_id（CLAUDE.md §4）：不同集群各有各的保留进度。
-- retained_from 为 NULL 表示从未清理过 —— 与"清理到了纪元 0"分得开。
CREATE TABLE flow_retention (
  cluster_id    VARCHAR(64) NOT NULL,
  retained_from DATETIME(6) NULL,
  updated_at    DATETIME(6) NOT NULL,
  PRIMARY KEY (cluster_id)
);
