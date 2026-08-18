package registry_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/imkerbos/Distill/internal/registry"
)

func TestAgentStateValid(t *testing.T) {
	for _, s := range []registry.AgentState{registry.AgentActive, registry.AgentRevoked} {
		if !s.Valid() {
			t.Errorf("AgentState(%q).Valid() = false, want true", s)
		}
	}
	// 空串与任何别的写法都不合法：这一列在库里只是 VARCHAR，封闭性只由
	// 这里保证。放行一个不认识的取值，认证层就要回答「这算不算 ACTIVE」，
	// 而那个问题没有安全的答案。
	for _, s := range []registry.AgentState{"", "active", "DISABLED", "REVOKED "} {
		if s.Valid() {
			t.Errorf("AgentState(%q).Valid() = true, want false", s)
		}
	}
}

// validAgent 返回一条各字段都合法的记录，供各用例只改一处后取反。
func validAgent() registry.ClusterAgent {
	return registry.ClusterAgent{
		ClusterID: "uat-k8s-cluster-01",
		AgentID:   "0011223344556677",
		TokenHash: make([]byte, 32),
		State:     registry.AgentActive,
		CreatedBy: "admin",
	}
}

func TestValidateClusterAgentAcceptsAWellFormedRecord(t *testing.T) {
	if err := registry.ValidateClusterAgent(validAgent()); err != nil {
		t.Fatalf("ValidateClusterAgent(valid) = %v, want nil", err)
	}
}

func TestValidateClusterAgentRequiresACluster(t *testing.T) {
	a := validAgent()
	a.ClusterID = ""
	err := registry.ValidateClusterAgent(a)
	if err == nil {
		t.Fatal("ValidateClusterAgent(no cluster) = nil — 一个不属于任何集群的 agent 主体，" +
			"它推上来的数据没有任何归属，而归属正是这张表存在的理由")
	}
	if !errors.Is(err, registry.ErrInvalid) {
		t.Errorf("error = %v, want it to be ErrInvalid", err)
	}
}

func TestValidateClusterAgentRequiresASHA256Hash(t *testing.T) {
	// 长度不是 32 说明存进来的不是 SHA-256。比对是按整段比的，长度不符
	// 恒不相等 —— 症状是「这把 token 怎么都认不过」，而成因在写入侧，
	// 从症状反推不到。必须在入库前就拒绝。
	for _, n := range []int{0, 16, 31, 33, 64} {
		a := validAgent()
		a.TokenHash = make([]byte, n)
		if err := registry.ValidateClusterAgent(a); err == nil {
			t.Errorf("ValidateClusterAgent(hash of %d bytes) = nil, want an error", n)
		}
	}
}

func TestValidateClusterAgentRejectsAnUnregisteredState(t *testing.T) {
	a := validAgent()
	a.State = "PAUSED"
	if err := registry.ValidateClusterAgent(a); err == nil {
		t.Error("ValidateClusterAgent(unregistered state) = nil, want an error")
	}
}

func TestValidateClusterAgentRejectsAMalformedAgentID(t *testing.T) {
	// agent_id 会被拼进查询、日志与吊销入口。它是平台自己生成的定长 hex，
	// 因此这里按定长 hex 判：放宽字符集等于把一个外部可控的字符串接进
	// 那些位置，而它本可以是封闭的。
	for _, id := range []string{
		"",                   // 空
		"00112233",           // 短
		"0011223344556677aa", // 长
		"0011223344556g77",   // 非 hex
		"0011223344556677\n", // 带控制字符
		"../../etc/passwd",
	} {
		a := validAgent()
		a.AgentID = id
		if err := registry.ValidateClusterAgent(a); err == nil {
			t.Errorf("ValidateClusterAgent(agentID %q) = nil, want an error", id)
		}
	}
}

func TestValidateClusterAgentErrorNeverEchoesTheHash(t *testing.T) {
	// 校验错误会走到 API 边界。哈希是离线爆破的输入，任何一条回传文案里
	// 都不该出现它（规范 §19、§20、§22）。
	a := validAgent()
	a.TokenHash = []byte("0123456789abcdef")
	err := registry.ValidateClusterAgent(a)
	if err == nil {
		t.Fatal("ValidateClusterAgent(short hash) = nil, want an error")
	}
	var invalid *registry.InvalidError
	if !errors.As(err, &invalid) {
		t.Fatalf("error = %v, want an *InvalidError", err)
	}
	if strings.Contains(invalid.Detail, "0123456789abcdef") {
		t.Errorf("Detail = %q — 校验文案里带出了哈希内容", invalid.Detail)
	}
}
