-- 回滚 metrics 抓取端登记。
--
-- 丢掉的是**登记，不是观测**：Pod 的抓取声明留在 observed_pod 里不受影响。
-- 回滚之后 METRICS_SCRAPE 退回缺失状态 —— 那是对的失败方向：说不出谁来抓，
-- 就不生成放行谁的规则。
--
-- 与 000014 那条同理，可由人重新登记恢复；不可恢复的只有「谁在什么时候登记的」，
-- 而那一条在 audit_log 里。
DROP TABLE cluster_metrics_scraper;
