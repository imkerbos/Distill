package risk_test

import (
	"testing"

	"github.com/imkerbos/Distill/internal/risk"
)

// 提取前后行为必须逐项相同：这次改动的唯一目的是解开依赖环，
// 任何清单内容的变化都是回归，而不是重构。
func TestCatalogIsExactlyTheKnownNineEntries(t *testing.T) {
	want := []risk.Port{
		{Port: 22, Name: "SSH", Category: risk.AdminPlaintext},
		{Port: 23, Name: "Telnet", Category: risk.AdminPlaintext},
		{Port: 445, Name: "SMB", Category: risk.FileShare},
		{Port: 3306, Name: "MySQL", Category: risk.Database},
		{Port: 3389, Name: "RDP", Category: risk.AdminPlaintext},
		{Port: 5432, Name: "PostgreSQL", Category: risk.Database},
		{Port: 6379, Name: "Redis", Category: risk.Database},
		{Port: 9200, Name: "Elasticsearch", Category: risk.Database},
		{Port: 27017, Name: "MongoDB", Category: risk.Database},
	}
	got := risk.Catalog()
	if len(got) != len(want) {
		t.Fatalf("catalog size = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("catalog[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// 9090 与 9100 是 Prometheus 与 node-exporter 的标准端口。
// 为了让报告有内容而给正常端口贴风险标签，与伪造判定没有区别。
func TestMonitoringPortsAreNotRisky(t *testing.T) {
	for _, p := range []int32{9090, 9100, 443, 8080, 8443} {
		if _, ok := risk.Lookup(p); ok {
			t.Errorf("port %d classified as risky, want not risky", p)
		}
	}
}

func TestLookupReturnsCategory(t *testing.T) {
	rp, ok := risk.Lookup(3306)
	if !ok {
		t.Fatal("Lookup(3306) not found, want found")
	}
	if rp.Category != risk.Database || rp.Name != "MySQL" {
		t.Errorf("Lookup(3306) = %+v, want MySQL/DATABASE", rp)
	}
}
