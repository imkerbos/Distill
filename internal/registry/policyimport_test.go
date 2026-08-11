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

// 同一份 spec 两次解析必须得到同一个哈希，否则「内容变没变」无从判断。
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
