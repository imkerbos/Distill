-- metrics 抓取端登记（design doc 2026-08-18-metrics-scrape-evidence §3.2）。
--
-- 本文件是新增的第 19 号迁移，不改动 000001–000018 中任何一条已应用的迁移。
--
-- **为什么抓取端是登记出来的，不是推出来的**：Pod 上的 prometheus.io/scrape
-- 注解只说了「谁愿意被抓」，说不出「谁来抓」。而生成的规则是被抓端的 ingress，
-- from 是抓取端的 namespaceSelector + podSelector —— 少了抓取端这条规则写不出来。
--
-- 靠「monitoring 命名空间里叫 prometheus 的那个」去猜，是一张硬编码常量表，
-- CLAUDE.md §3 明确禁止。猜错的后果不是报错，是生成一条 podSelector 选不中任何
-- Pod 的 ingress —— 看起来齐备、实际什么都没放行，而监控在下发之后静默中断。
--
-- 与 cluster_health_check_source 完全同源：那一列的注释里写过为什么网段要登记
-- 而不是写死（「网段会变，硬编码的常量表不会跟着变，且没人知道它当初是怎么来的」）。
--
-- **一个集群可以有多个抓取端**：Prometheus 与一个 agent 型采集器并存是常见形态。
-- 主键因此含 namespace 与标签指纹，而不是每集群一行。
--
-- labels 存 JSON 对象而非字符串：它要当 podSelector 用，而 "a=b,c=d" 这种写法
-- 在标签值里出现逗号或等号时会静默解析错，且没有任何症状。
--
-- labels_key 是 labels 的规范化文本，只用来做主键 —— MySQL 不支持在 JSON 列上
-- 直接建主键。它由 Go 侧算出（键排序后拼接），不由 SQL 生成：两处各算一次
-- 就有了两个可能分歧的定义。
CREATE TABLE cluster_metrics_scraper (
  cluster_id VARCHAR(64)  NOT NULL,
  namespace  VARCHAR(63)  NOT NULL,
  labels_key VARCHAR(512) NOT NULL,
  labels     JSON         NOT NULL,
  PRIMARY KEY (cluster_id, namespace, labels_key)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
