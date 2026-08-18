package registry

import (
	"sort"
	"strconv"
	"strings"

	"github.com/imkerbos/Distill/internal/snapshot"
)

// MetricsScraper 是一个集群里的 metrics 抓取端。
//
// **它是登记出来的，不是推导出来的**（design doc 2026-08-18 §3.2）：Pod 上的
// prometheus.io/scrape 注解只说了「谁愿意被抓」，说不出「谁来抓」，而生成的规则
// 是被抓端的 ingress —— from 是抓取端的 namespaceSelector + podSelector。
//
// 靠「monitoring 命名空间里叫 prometheus 的那个」去猜，是一张硬编码常量表
// （CLAUDE.md §3 禁止）。猜错的后果不是报错，是一条 podSelector 选不中任何 Pod
// 的 ingress —— 看起来齐备、实际什么都没放行，而监控在下发之后静默中断。
//
// 与 HealthCheckSources 同源：那里也是登记而非硬编码，理由同一条。
type MetricsScraper struct {
	// Namespace 是抓取端所在命名空间，用作 namespaceSelector。
	Namespace string `json:"namespace"`
	// Labels 是抓取端 Pod 的标签，用作 podSelector。
	//
	// 不许为空：空 podSelector 会选中那个命名空间里的每一个 Pod，而规则
	// 照样"生成成功"。
	Labels map[string]string `json:"labels"`
}

// LabelsKey 是这组标签的规范化文本，用作主键的一部分。
//
// MySQL 不能在 JSON 列上建主键，因此需要一个稳定的文本形式。**由 Go 侧算**，
// 不由 SQL 生成：两处各算一次就有了两个可能分歧的定义，而分歧的症状是同一个
// 抓取端被登记两次，于是同一条 ingress 规则生成两遍。
//
// 键排序后拼接，因此与 map 的遍历顺序无关。
func (s MetricsScraper) LabelsKey() string {
	if len(s.Labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(s.Labels))
	for k := range s.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('\n')
		}
		// 用换行与制表符分隔而不是逗号与等号：标签值里出现后者是合法的，
		// 而出现前者不是（Kubernetes 的标签值不允许控制字符）。
		b.WriteString(k)
		b.WriteByte('\t')
		b.WriteString(s.Labels[k])
	}
	return b.String()
}

// ValidateMetricsScraper 校验一条抓取端登记。
func ValidateMetricsScraper(s MetricsScraper) error {
	if s.Namespace == "" {
		return invalid("metrics 抓取端必须给出命名空间：缺了它，生成的 namespaceSelector 会匹配所有命名空间")
	}
	if len(s.Labels) == 0 {
		return invalid("metrics 抓取端必须给出 Pod 标签：空的 podSelector 会放行该命名空间里的每一个 Pod")
	}
	for k, v := range s.Labels {
		if k == "" || v == "" {
			return invalid("metrics 抓取端的标签键与值都不能为空")
		}
	}
	// 主键存 VARCHAR(512)，超了会在落库时被 MySQL 截断或报错。在这里拒绝，
	// 而不是让一次截断把两个不同的抓取端变成同一个。
	if len(s.LabelsKey()) > 512 {
		return invalid("metrics 抓取端的标签过长")
	}
	return nil
}

// scrapeAnnotationScrape / scrapeAnnotationPort 是被抓端声明用的注解键。
//
// 与 collect.ScrapeAnnotationKeys 是同一组键的两处使用：那里决定采什么，
// 这里决定怎么读。**不共享常量**是刻意的 —— collect 属于采集层，registry
// 属于注册层，让后者 import 前者会把一条 K8s 客户端的依赖拖进注册表。
// 两处漂移的代价是这一半读不出来，而那会让 METRICS_SCRAPE 恒定缺失；
// 由 ScrapeTargetSnapshots 的测试与 collect 的白名单测试各自钉住。
const (
	scrapeAnnotationScrape = "prometheus.io/scrape"
	scrapeAnnotationPort   = "prometheus.io/port"
)

// ScrapeTargetSnapshots 把「登记的抓取端」与「观测到的被抓端」拼成推导依据。
//
// 两半各有各的来源（design doc 2026-08-18 §4）：抓取端来自登记（观测不出来），
// 被抓端来自 Pod 自己的注解（登记不出来）。一条规则要能回答「凭什么放行这个
// podSelector 到这个端口」，两个半句都答得出来。
//
// **没有登记抓取端时返回空**，METRICS_SCRAPE 照旧进缺失清单：编一条占位规则
// 会让齐备性校验通过，而真正的抓取仍被挡（baseline.Set.Missing 的注释）。
//
// **声明不完整的 Pod 一律跳过，不补默认值。** 说了要抓却没给端口、端口不是
// 数字、端口越界，都跳过 —— 一条放行到猜出来的端口的规则，看起来齐备、实际
// 什么都没放行，而症状要到监控静默中断时才出现。
func (c Cluster) ScrapeTargetSnapshots(pods []snapshot.Pod) []snapshot.ScrapeTarget {
	if len(c.MetricsScrapers) == 0 {
		return nil
	}
	var out []snapshot.ScrapeTarget
	for _, s := range c.MetricsScrapers {
		for _, p := range pods {
			port, ok := scrapePortOf(p)
			if !ok {
				continue
			}
			labels := make(map[string]string, len(s.Labels))
			for k, v := range s.Labels {
				labels[k] = v
			}
			out = append(out, snapshot.ScrapeTarget{
				ClusterID:        c.ID,
				ScraperNamespace: s.Namespace,
				ScraperLabels:    labels,
				TargetNamespace:  p.Namespace,
				TargetPort:       port,
			})
		}
	}
	return out
}

// scrapePortOf 读出一个 Pod 声明的 metrics 端口；没有可用声明时 ok 为 false。
func scrapePortOf(p snapshot.Pod) (int32, bool) {
	if p.ScrapeAnnotations[scrapeAnnotationScrape] != "true" {
		return 0, false
	}
	raw, ok := p.ScrapeAnnotations[scrapeAnnotationPort]
	if !ok {
		return 0, false
	}
	// ParseInt 带位宽而不是 Atoi 再转换：后者在 32 位截断上要靠上面那个
	// 范围检查兜底，而 gosec 看不到那个检查的效果，只能就地标注 —— 与其
	// 加一行 nolint，不如让类型本身说清楚。
	port, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || port <= 0 || port > 65535 {
		return 0, false
	}
	return int32(port), true
}
