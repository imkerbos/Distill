package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/imkerbos/Distill/internal/response"
)

// handleAgentConfig 里那句「归属取不到就拒绝」防的是一次装配错误：有人把
// 这条路由挂到了 RequireAgent 之外。从外部测试装配不出这个形状 —— 真实
// 路由器里它永远在中间件之后，于是那个分支不可达，删掉它对外行为一模一样。
//
// 这里直接把 handler 挂成裸的，才谈得上验证它。这是**内部**测试，因为
// handler 不导出；换成外部测试就只能靠"路由器里它一直好好的"来间接说明，
// 而那什么也没说明。
func TestAgentConfigRefusesWhenItCannotTellWhichCluster(t *testing.T) {
	rec := httptest.NewRecorder()
	handleAgentConfig()(rec, httptest.NewRequest(http.MethodGet, "/config", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 — 一个答不出自己该采哪个集群的配置，"+
			"交出去只会让 agent 采错东西", rec.Code)
	}
	if !containsCode(rec.Body.String(), response.CodeAgentUnauthenticated) {
		t.Errorf("body = %s, want code %d", rec.Body.String(), response.CodeAgentUnauthenticated)
	}
}

// containsCode 判断响应体里带的是不是这个业务码。
func containsCode(body string, code response.Code) bool {
	want := `"code":` + itoa(int(code))
	for i := 0; i+len(want) <= len(body); i++ {
		if body[i:i+len(want)] == want {
			return true
		}
	}
	return false
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
