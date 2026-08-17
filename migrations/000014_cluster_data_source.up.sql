-- 集群的数据来源登记（design doc 2026-08-17 §2）：这个集群的读取数据是来自
-- internal/fixture 的合成数据集，还是来自它自己被采集到的资产与流量。
--
-- 本文件是新增的第 14 号迁移，不改动 000001–000013 中任何一条已应用的迁移。
-- golang-migrate 只按版本号判断某个库跑过哪一版：改一个已被记录的版本，那个库
-- 永远不会重跑修正后的文件，于是它与从零建起来的测试库静默分叉，而分叉本身
-- 不报任何错（见 docs/superpowers/HANDOFF.md 里 000012 的教训）。
--
-- **为什么要有这一列**：没有它，「读哪份数据」就只能由「有没有采集到数据」推断
-- 出来，而推断意味着一次采集故障会让一个真集群悄悄退回演示集群 —— 操作者拿到
-- 一份写着真集群名字、数字合理、流程走得通的完整报告，据此批准一次下发，而那些
-- 连接属于一个不存在的集群。页面上没有任何地方会显得不对。
--
-- **默认值是 COLLECTED，不是 FIXTURE。** 两个方向的错都会发生，但代价不对称：
-- 一个本该读 fixture 的集群被标成 COLLECTED，症状是「这个集群还没有可用的采集」
-- —— 难看，且立刻可见；反过来，一个真集群被标成 FIXTURE，症状是一份看不出问题
-- 的假报告。默认值必须朝前一个方向掉。同一个理由也让 INSERT 时忘记带这一列
-- 是安全的。
--
-- 长度取 VARCHAR(16)：两个取值最长 9 个字符，与 collection_run.status 一列同宽。
-- 不用 ENUM —— 本库其余封闭枚举一律存字符串、由 Go 侧的 Valid() 把关，多一种
-- 表达方式只会让「枚举在哪里定义」有两个答案。
ALTER TABLE cluster
  ADD COLUMN data_source VARCHAR(16) NOT NULL DEFAULT 'COLLECTED' AFTER ccnp_present;

-- 000002 种下的两个演示集群点名置为 FIXTURE。
--
-- 点名而不是「凡是已存在的行都算 FIXTURE」：dev 库里已经有真集群，把它们一并
-- 标成 FIXTURE 正是上面那个最坏结果。只有这两个 ID 是 fixture 里有完整、已知
-- 答案数据的集群（internal/fixture），其余任何行都只能是 COLLECTED。
UPDATE cluster SET data_source = 'FIXTURE'
 WHERE cluster_id IN ('prod-asia-1', 'prod-eu-1');
