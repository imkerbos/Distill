package registry

import (
	"errors"
	"fmt"
	"net/netip"
)

// ErrInvalid 表示输入不合法。
var ErrInvalid = errors.New("invalid registry input")

// ValidateCluster 校验一个集群注册请求。
//
// 网段在入库前校验而非推导时才发现：一个写错的网段会让 Baseline 生成
// 一条永远匹配不上的规则，而那条规则外观完全正常 —— 等到上线后监控
// 静默中断才暴露，那时恰好看不到数据。
func ValidateCluster(c Cluster) error {
	if c.ID == "" {
		return fmt.Errorf("%w: cluster id is required", ErrInvalid)
	}
	if c.DisplayName == "" {
		return fmt.Errorf("%w: display name is required", ErrInvalid)
	}
	if !c.State.Valid() {
		return fmt.Errorf("%w: unregistered onboard state %q", ErrInvalid, c.State)
	}
	if err := checkCIDR("podCIDR", c.PodCIDR); err != nil {
		return err
	}
	if err := checkCIDR("nodeCIDR", c.NodeCIDR); err != nil {
		return err
	}
	for i, a := range c.APIServers {
		if a.Host == "" {
			return fmt.Errorf("%w: apiserver[%d] host is required", ErrInvalid, i)
		}
		if a.Port <= 0 || a.Port > 65535 {
			return fmt.Errorf("%w: apiserver[%d] port %d out of range", ErrInvalid, i, a.Port)
		}
		if err := checkCIDR(fmt.Sprintf("apiserver[%d] cidr", i), a.CIDR); err != nil {
			return err
		}
	}
	for i, s := range c.HealthCheckSources {
		if err := checkCIDR(fmt.Sprintf("healthCheck[%d] cidr", i), s); err != nil {
			return err
		}
	}
	return validateGit(c.Git)
}

// validateGit 要求绑定要么完整要么不填。
//
// 填一半的绑定会在轮 3 变成一次指向不存在路径的写入尝试，
// 而报出来的错误只会说「路径不存在」，与真正的配置错误无法区分。
func validateGit(g *GitBinding) error {
	if g == nil {
		return nil
	}
	if g.RepoURL == "" || g.Branch == "" || g.PolicyPath == "" {
		return fmt.Errorf(
			"%w: git binding needs repoUrl, branch and policyPath together", ErrInvalid)
	}
	return nil
}

// checkCIDR 校验一个网段，错误信息带上字段名。
//
// 带字段名而非只说「非法网段」：一个集群有四类网段，
// 只报「非法」会让操作者逐个试。
func checkCIDR(field, value string) error {
	if value == "" {
		return fmt.Errorf("%w: %s is required", ErrInvalid, field)
	}
	if _, err := netip.ParsePrefix(value); err != nil {
		return fmt.Errorf("%w: %s %q is not a valid CIDR", ErrInvalid, field, value)
	}
	return nil
}
