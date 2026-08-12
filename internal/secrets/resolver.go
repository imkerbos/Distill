// Package secrets 把 credential_ref 解析成凭据内容。
package secrets

import (
	"context"
	"errors"
	"regexp"
)

// ErrNotFound 表示引用本身合法，但目标不存在。
//
// 必须与「解析过程出错」区分：引用取不到是平台侧配置问题，
// 取到了但认证失败是仓库侧权限问题，两者的处置人不是同一个。
var ErrNotFound = errors.New("secret not found")

// ErrInvalidRef 表示引用不是合法的短名。
var ErrInvalidRef = errors.New("invalid secret ref")

// Resolver 把引用解析成凭据内容。
//
// 返回值是私钥字节，只在内存中存在：不落盘、不进日志、不进错误信息。
type Resolver interface {
	Resolve(ctx context.Context, ref string) ([]byte, error)
}

// refPattern 限制引用为 1..64 个小写字母、数字或连字符，首尾不得为连字符。
//
// 比 Secret Manager 自身的命名规则更严。收紧的理由是这个短名会被拼进
// 资源路径：允许的字符越多，能表达的越权路径越多。这里不是在做输入校验，
// 是在让越权配置无法被表达。
var refPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$`)

// ValidateRef 校验引用形态。
func ValidateRef(ref string) error {
	if !refPattern.MatchString(ref) {
		return ErrInvalidRef
	}
	return nil
}
