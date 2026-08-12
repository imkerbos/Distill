package secrets

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DirResolver 从本地目录读取凭据，一个 ref 一个文件。
//
// 仅供本地开发使用：该目录必须 gitignored，生产走 Secret Manager
// 实现（未来任务）。见 spec §2.4。
type DirResolver struct {
	dir string
}

// NewDirResolver 构造一个从 dir 读取凭据的 Resolver。
func NewDirResolver(dir string) *DirResolver {
	return &DirResolver{dir: dir}
}

// Resolve 按 ref 读取 dir 下的同名文件。
//
// 校验与路径确认是两道独立的关卡：ValidateRef 限制字符集，
// 这里再用 pathWithinDir 确认拼出来的路径仍落在 dir 之内。
//
// 这一行 pathWithinDir 调用本身没有测试覆盖：ValidateRef 的字符集
// 不含 `/` 与 `.`，任何能走到这一行的 ref 已经不可能让 Join 逃出
// dir，所以黑盒测试无法通过删掉这行调用来验证它——删了不删，
// Resolve() 对外行为都一样，测试测不出来。这行调用防的是将来
// ValidateRef 被放宽；它的正确性由 pathWithinDir 自己的白盒测试
// （dir_internal_test.go）保证，但「Resolve 是否仍然调用它」这件事
// 只能靠改动这个函数的人 review 时肉眼确认，不要在改这个函数时
// 顺手删掉这一段。
func (r *DirResolver) Resolve(_ context.Context, ref string) ([]byte, error) {
	if err := ValidateRef(ref); err != nil {
		return nil, err
	}

	path, ok := pathWithinDir(r.dir, ref)
	if !ok {
		return nil, ErrInvalidRef
	}

	// path 已经过 ValidateRef 字符集校验与 pathWithinDir 目录包含性
	// 校验两道关卡，不是外部可控的任意路径——gosec 的静态分析看不到
	// 这两道校验的效果，只能就地标注。
	data, err := os.ReadFile(path) // #nosec G304
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		// 不用 %w：底层错误可能带路径信息，不能透传到 API 边界。
		// 见 Global Constraints 与 spec §2.5。
		return nil, fmt.Errorf("resolve secret ref: read failed")
	}
	return data, nil
}

// pathWithinDir 判断把 ref 拼进 dir 后，结果路径是否仍落在 dir 之内。
//
// 今天这个函数测不出「校验失效」以外的任何东西：ValidateRef 的字符集
// 已经把 `/` 和 `.` 都挡在外面，任何走到这里的 ref 本来就不可能逃出
// dir。它存在是为了防将来——一旦 refPattern 被放宽（哪怕只是重新
// 允许 `.`），这里仍然是挡住任意文件读的最后一道闸，而不是被悄悄
// 跳过的一段死代码。因此单独对它做白盒测试（见 dir_internal_test.go），
// 不依赖 ValidateRef 已经把输入收窄这件事。
func pathWithinDir(dir, ref string) (string, bool) {
	path := filepath.Clean(filepath.Join(dir, ref))
	base := filepath.Clean(dir)
	if path == base {
		return path, true
	}
	return path, strings.HasPrefix(path, base+string(filepath.Separator))
}
