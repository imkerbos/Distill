package registry_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/imkerbos/Distill/internal/registry"
)

const goodPolicy = `
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-gateway
  namespace: payment
spec:
  podSelector:
    matchLabels:
      app: api
  policyTypes: [Ingress]
  ingress:
    - from:
        - ipBlock:
            cidr: 10.4.0.0/14
            except: [10.4.1.0/24]
      ports:
        - protocol: TCP
          port: 8080
`

func TestParseImportAcceptsAValidPolicy(t *testing.T) {
	got, err := registry.ParseImport(goodPolicy)
	if err != nil {
		t.Fatalf("ParseImport() error = %v", err)
	}
	if got.Namespace != "payment" || got.Name != "allow-gateway" {
		t.Errorf("parsed = %s/%s, want payment/allow-gateway", got.Namespace, got.Name)
	}
	if len(got.SpecHash) != 64 {
		t.Errorf("SpecHash = %q, want a 64-char sha256 hex", got.SpecHash)
	}
}

// 两份文档的 spec 逐字相同，但注释、metadata 字段顺序、以及
// metadata.annotations 都不同。哈希必须相同 —— 否则每次重新格式化
// 或补一条 annotation 都会被误读成「策略内容变了」。
//
// 这条测试是本文件真正的把关者：如果 specHash 改成对整份 YAML 取哈希，
// 两次解析同一字符串的旧测试仍然会通过（同一输入两次哈希天然相同），
// 但这条会失败，因为两份文档字节不同而 spec 相同。
const specNoiseA = `
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-gateway
  namespace: payment
spec:
  podSelector:
    matchLabels:
      app: api
  policyTypes: [Ingress]
  ingress:
    - from:
        - ipBlock:
            cidr: 10.4.0.0/14
            except: [10.4.1.0/24]
      ports:
        - protocol: TCP
          port: 8080
`

const specNoiseB = `
# imported from git, reviewed by the security team
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  annotations:
    owner: team-payment
    review-date: "2026-08-01"
  namespace: payment
  name: allow-gateway
spec:
  podSelector:
    matchLabels:
      app: api
  policyTypes: [Ingress]
  ingress:
    - from:
        - ipBlock:
            cidr: 10.4.0.0/14
            except: [10.4.1.0/24]
      ports:
        - protocol: TCP
          port: 8080
`

// specNoiseC 与 specNoiseB 仅一处不同：ports[0].port 从 8080 改成 9090。
// 这是 spec 内部的真实变化，哈希必须跟着变。
const specNoiseC = `
# imported from git, reviewed by the security team
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  annotations:
    owner: team-payment
    review-date: "2026-08-01"
  namespace: payment
  name: allow-gateway
spec:
  podSelector:
    matchLabels:
      app: api
  policyTypes: [Ingress]
  ingress:
    - from:
        - ipBlock:
            cidr: 10.4.0.0/14
            except: [10.4.1.0/24]
      ports:
        - protocol: TCP
          port: 9090
`

func TestParseImportHashIgnoresDocumentNoise(t *testing.T) {
	a, err := registry.ParseImport(specNoiseA)
	if err != nil {
		t.Fatalf("ParseImport(specNoiseA) error = %v", err)
	}
	b, err := registry.ParseImport(specNoiseB)
	if err != nil {
		t.Fatalf("ParseImport(specNoiseB) error = %v", err)
	}
	if a.SpecHash != b.SpecHash {
		t.Errorf("SpecHash differs for spec-equivalent documents: %s vs %s", a.SpecHash, b.SpecHash)
	}
}

func TestParseImportHashChangesWhenSpecChanges(t *testing.T) {
	b, err := registry.ParseImport(specNoiseB)
	if err != nil {
		t.Fatalf("ParseImport(specNoiseB) error = %v", err)
	}
	c, err := registry.ParseImport(specNoiseC)
	if err != nil {
		t.Fatalf("ParseImport(specNoiseC) error = %v", err)
	}
	if b.SpecHash == c.SpecHash {
		t.Errorf("SpecHash unchanged after a spec-internal edit (port 8080 -> 9090): %s", b.SpecHash)
	}
}

// 同一份 spec 两次解析必须得到同一个哈希，否则「内容变没变」无从判断。
// 这是三条里最弱的一条：哈希整份文档也能通过它。
func TestParseImportHashIsStable(t *testing.T) {
	a, _ := registry.ParseImport(goodPolicy)
	b, _ := registry.ParseImport(goodPolicy)
	if a.SpecHash != b.SpecHash {
		t.Errorf("SpecHash differs across parses: %s vs %s", a.SpecHash, b.SpecHash)
	}
}

func TestParseImportRejectsNonNetworkPolicy(t *testing.T) {
	_, err := registry.ParseImport(`
apiVersion: v1
kind: ConfigMap
metadata:
  name: x
  namespace: y
`)
	if !errors.Is(err, registry.ErrInvalid) {
		t.Errorf("err = %v, want ErrInvalid", err)
	}
}

func TestParseImportRejectsMissingNamespace(t *testing.T) {
	_, err := registry.ParseImport(`
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: x
spec:
  podSelector: {}
`)
	if !errors.Is(err, registry.ErrInvalid) {
		t.Errorf("err = %v, want ErrInvalid", err)
	}
}

// fixture 里那条 broken-ipblock（10.0.0/8）就是故意留的写坏策略。
// 导入时不拦，它会一路走到求值层产出 POLICY_MALFORMED，
// 而使用者会以为是平台的问题。
func TestParseImportRejectsMalformedIPBlock(t *testing.T) {
	for name, yamlText := range map[string]string{
		"cidr": `
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: x, namespace: y}
spec:
  podSelector: {}
  ingress:
    - from: [{ipBlock: {cidr: "10.0.0/8"}}]
`,
		"except": `
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: x, namespace: y}
spec:
  podSelector: {}
  ingress:
    - from: [{ipBlock: {cidr: "10.0.0.0/8", except: ["10.1/16"]}}]
`,
		// checkIPBlocks 同样要走 Spec.Egress[].To；没有这两个用例，
		// egress 那一半校验删掉也不会有测试发现。
		"egress-cidr": `
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: x, namespace: y}
spec:
  podSelector: {}
  egress:
    - to: [{ipBlock: {cidr: "10.0.0/8"}}]
`,
		"egress-except": `
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: x, namespace: y}
spec:
  podSelector: {}
  egress:
    - to: [{ipBlock: {cidr: "10.0.0.0/8", except: ["10.1/16"]}}]
`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := registry.ParseImport(yamlText)
			if !errors.Is(err, registry.ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
			if !strings.Contains(err.Error(), "ipBlock") {
				t.Errorf("err = %q, want it to name ipBlock", err)
			}
		})
	}
}
