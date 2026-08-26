package registry_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/imkerbos/Distill/internal/registry"
)

// 业务周期以**秒**出现在 JSON 里，不是纳秒。
//
// time.Duration 的默认 JSON 形态是纳秒：一个 7 天的周期会变成
// 604800000000000，界面上显示成一串十几位数字，而没有人会想到那是纳秒。
func TestBusinessCycleSerializesAsSeconds(t *testing.T) {
	b, err := json.Marshal(registry.Cluster{
		ID: "c1", BusinessCycle: 7 * 24 * time.Hour, BusinessCycleReason: "月结",
	})
	if err != nil {
		t.Fatalf("Marshal() = %v", err)
	}
	if !strings.Contains(string(b), `"businessCycleSeconds":604800`) {
		t.Errorf("JSON 里没有以秒表达的业务周期：%s", b)
	}
	if strings.Contains(string(b), "604800000000000") {
		t.Error("业务周期被序列化成了纳秒")
	}
}
