// Package settings 在使用处按需读取落库的运行期配置。
//
// 本包不碰 HTTP、不碰 SQL、不引入任何框架：它只是「读一份设置」这件事
// 本身。持久化在 registry 的实现里，装配在 cmd 里。
package settings

import (
	"context"
	"fmt"

	"github.com/imkerbos/Distill/internal/registry"
)

// Source 是设置的持久化来源。registry.Store 满足它。
//
// 收成一个只有读的接口，而不是直接依赖 registry.Store：本包只读设置，
// 而 registry.Store 还带着集群、导入与绑定的全部写方法 —— 让「读一份
// 设置」这条路径在类型上具备改写注册表的能力，没有任何理由。
type Source interface {
	Setting(ctx context.Context) (registry.PlatformSetting, error)
}

// Provider 是运行期配置的读取入口。
//
// 它刻意**不带任何缓存字段**。缓存放在这里会是一次看不出来的回归：所有
// 单次调用的行为都不变，差别只在第二次调用 —— 也就是操作者刚在设置页
// 保存完的那一次（design doc §1.1）。
type Provider struct {
	src Source
}

// New 构造一个从 src 现读设置的 Provider。
func New(src Source) *Provider {
	return &Provider{src: src}
}

// Current 返回当前的平台设置。
//
// **每次调用都读一次来源。** 轮 2 已经在启动快照上出过一次事故：集群注册
// 状态被读成一份内存清单，下线一个集群之后 /security 与 /policy-preview
// 继续服务它，直到进程重启。设置的后果更直接 —— 改了 SSH host key 或
// Git 超时，界面显示保存成功，运行期却纹丝不动。
//
// 代价是每次使用多一次单行主键命中，量级可忽略（design doc §8）。
func (p *Provider) Current(ctx context.Context) (registry.PlatformSetting, error) {
	s, err := p.src.Setting(ctx)
	if err != nil {
		return registry.PlatformSetting{}, fmt.Errorf("read platform setting: %w", err)
	}
	// 落库的行未必是经 UpdateSetting 写进去的：迁移种子行、手工 SQL、
	// 将来的 down/up 反复都绕得开那一层校验。这里再判一次，是因为这些
	// 字段的零值不是「用默认」—— 零 TTL 是会话立即过期，零超时是关掉
	// 超时保护，而两者都不会有任何症状指向设置本身。
	if err := registry.ValidatePlatformSetting(s); err != nil {
		return registry.PlatformSetting{}, fmt.Errorf("unusable platform setting: %w", err)
	}
	return s, nil
}
