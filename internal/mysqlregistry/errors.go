package mysqlregistry

import (
	"errors"
	"fmt"

	mysqldriver "github.com/go-sql-driver/mysql"

	"github.com/imkerbos/Distill/internal/registry"
)

// MySQL 的两个错误号，它们描述的是调用方的输入问题而不是服务故障。
//
// 数字写成常量而非散落在 switch 里：1062 与 1452 出现在多条写路径上，
// 而一个裸数字在 review 时无法与「重试次数」之类的常量区分。
const (
	// errDuplicateEntry 是主键或唯一键冲突（ER_DUP_ENTRY）。
	errDuplicateEntry uint16 = 1062
	// errNoReferencedRow 是外键指向了不存在的父行（ER_NO_REFERENCED_ROW_2）。
	errNoReferencedRow uint16 = 1452
)

// writeFailure 把一次写入失败翻译成领域错误。
//
// 翻译发生在捕获驱动错误的这一层，而不是 HTTP 层：边界层只认
// registry.ErrInvalid / registry.ErrNotFound，它不该为了分辨「重复注册」
// 而去 import go-sql-driver 并按错误号分支 —— 那等于把存储实现细节顺着
// 调用链一路带到响应映射里。
//
// 判据是「该不该计入服务错误率」：重复的集群 ID 与指向未注册集群的导入
// 都是操作者填错了东西，属于业务失败；返回 500 会让注册页在一次正常的
// 手滑上显示「服务器错误」，并把它计进服务可用性。
//
// duplicate 与 missingRef 是本平台自己写的文案，可以回传给调用方；
// 留空表示这条写路径不该出现对应的错误号，此时按内部错误处理。
func writeFailure(op, duplicate, missingRef string, err error) error {
	var me *mysqldriver.MySQLError
	if errors.As(err, &me) {
		switch {
		case me.Number == errDuplicateEntry && duplicate != "":
			return registry.NewInvalidError(duplicate)
		case me.Number == errNoReferencedRow && missingRef != "":
			return fmt.Errorf("%w: %s", ErrNotFound, missingRef)
		}
	}
	return fmt.Errorf("%s: %w", op, err)
}
