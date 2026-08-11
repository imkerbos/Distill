package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"

	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/yaml"
)

// ParsedPolicy 是一次成功解析的结果。
type ParsedPolicy struct {
	// Namespace 与 Name 取自 metadata。
	Namespace string
	Name      string
	// SpecHash 是 spec 部分的 SHA-256，用于识别内容是否变过。
	SpecHash string
	// Policy 是解析出的对象。
	Policy networkingv1.NetworkPolicy
}

// ParseImport 解析并校验一段 NetworkPolicy YAML。
//
// 校验在导入时完成而非求值时：一条写坏的 ipBlock 若被放进来，
// 会一路走到求值层产出 POLICY_MALFORMED，而使用者会把它读成
// 平台的缺陷，而不是自己那段 YAML 的问题。
func ParseImport(yamlText string) (ParsedPolicy, error) {
	var typeMeta struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
	}
	if err := yaml.Unmarshal([]byte(yamlText), &typeMeta); err != nil {
		return ParsedPolicy{}, fmt.Errorf("%w: cannot parse YAML: %w", ErrInvalid, err)
	}
	if typeMeta.Kind != "NetworkPolicy" {
		return ParsedPolicy{}, fmt.Errorf(
			"%w: kind is %q, want NetworkPolicy", ErrInvalid, typeMeta.Kind)
	}

	var p networkingv1.NetworkPolicy
	if err := yaml.Unmarshal([]byte(yamlText), &p); err != nil {
		return ParsedPolicy{}, fmt.Errorf("%w: cannot parse NetworkPolicy: %w", ErrInvalid, err)
	}
	if p.Namespace == "" {
		return ParsedPolicy{}, fmt.Errorf("%w: metadata.namespace is required", ErrInvalid)
	}
	if p.Name == "" {
		return ParsedPolicy{}, fmt.Errorf("%w: metadata.name is required", ErrInvalid)
	}
	if err := checkIPBlocks(p); err != nil {
		return ParsedPolicy{}, err
	}

	hash, err := specHash(p)
	if err != nil {
		return ParsedPolicy{}, err
	}
	return ParsedPolicy{
		Namespace: p.Namespace, Name: p.Name, SpecHash: hash, Policy: p,
	}, nil
}

// checkIPBlocks 校验全部 ipBlock 的 cidr 与 except。
func checkIPBlocks(p networkingv1.NetworkPolicy) error {
	check := func(b *networkingv1.IPBlock) error {
		if b == nil {
			return nil
		}
		if _, err := netip.ParsePrefix(b.CIDR); err != nil {
			return fmt.Errorf("%w: ipBlock cidr %q is not a valid CIDR", ErrInvalid, b.CIDR)
		}
		for _, e := range b.Except {
			if _, err := netip.ParsePrefix(e); err != nil {
				return fmt.Errorf("%w: ipBlock except %q is not a valid CIDR", ErrInvalid, e)
			}
		}
		return nil
	}
	for _, rule := range p.Spec.Ingress {
		for i := range rule.From {
			if err := check(rule.From[i].IPBlock); err != nil {
				return err
			}
		}
	}
	for _, rule := range p.Spec.Egress {
		for i := range rule.To {
			if err := check(rule.To[i].IPBlock); err != nil {
				return err
			}
		}
	}
	return nil
}

// specHash 计算 spec 的 SHA-256。
//
// 只哈希 spec 而非整份 YAML：注释、字段顺序、metadata 的
// annotation 变化都不该被读成「策略内容变了」。
func specHash(p networkingv1.NetworkPolicy) (string, error) {
	b, err := json.Marshal(p.Spec)
	if err != nil {
		return "", fmt.Errorf("%w: cannot hash spec: %w", ErrInvalid, err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
