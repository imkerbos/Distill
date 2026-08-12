package gcpsecrets

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/googleapis/gax-go/v2/apierror"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/imkerbos/Distill/internal/secrets"
)

// classifyAccessError 是白盒测试：API 调用本身在这里够不着（没有 GCP
// 凭据），但「拿到一个错误之后怎么分类」是纯逻辑，与调用无关。把它单独
// 拎出来测，是为了让这一层不必等到生产才第一次被执行。

// 只有 NotFound 是 secrets.ErrNotFound。
func TestNotFoundMapsToErrNotFound(t *testing.T) {
	err := status.Error(codes.NotFound, "secret projects/p/secrets/x not found")
	if got := classifyAccessError(err); !errors.Is(got, secrets.ErrNotFound) {
		t.Fatalf("classifyAccessError(NotFound) = %v, want secrets.ErrNotFound", got)
	}
}

// 其余状态码都不是 secrets.ErrNotFound。
//
// 这条挡的是「取不到」与「查不了」被混成一类：前者是平台侧把引用写错了，
// 后者可能是权限没开或链路故障，处置人不同。PermissionDenied 尤其不能算
// NotFound —— Secret Manager 对无权访问的资源同样会说不存在，我们自己
// 这一层至少不能再主动把它们抹平。
func TestNonNotFoundStatusesAreNotErrNotFound(t *testing.T) {
	for _, code := range []codes.Code{
		codes.PermissionDenied,
		codes.Unauthenticated,
		codes.Unavailable,
		codes.DeadlineExceeded,
		codes.ResourceExhausted,
		codes.Internal,
		codes.InvalidArgument,
	} {
		got := classifyAccessError(status.Error(code, "boom"))
		if got == nil {
			t.Errorf("classifyAccessError(%v) = nil, want an error", code)
			continue
		}
		if errors.Is(got, secrets.ErrNotFound) {
			t.Errorf("classifyAccessError(%v) = secrets.ErrNotFound, want it kept distinct from a missing secret", code)
		}
	}
}

// 非状态错误（例如上下文取消、传输层错误）也不是 secrets.ErrNotFound。
func TestPlainErrorsAreNotErrNotFound(t *testing.T) {
	got := classifyAccessError(errors.New("dial tcp: i/o timeout"))
	if errors.Is(got, secrets.ErrNotFound) {
		t.Fatal("classifyAccessError(plain error) = secrets.ErrNotFound, want a lookup failure")
	}
}

// 底层错误文本不得随错误一起往上走。
//
// gitverify 会把解析错误归进一个封闭枚举，而云 SDK 的文本里带着资源路径、
// 项目号和重试细节。这条断言检查两件事：文本没被拼进来，错误链里也没挂着
// 原错误 —— 少了后半句，一个 %w 就能让文本从 errors.Unwrap 那一侧漏出去。
func TestUnderlyingTextDoesNotRideAlong(t *testing.T) {
	const leak = "projects/123456789/secrets/distill-git-prod-asia-1 caller lacks permission"
	underlying := status.Error(codes.PermissionDenied, leak)

	got := classifyAccessError(underlying)
	if strings.Contains(got.Error(), leak) {
		t.Errorf("classifyAccessError() = %q, want the underlying text kept out of it", got)
	}
	if strings.Contains(got.Error(), "123456789") {
		t.Errorf("classifyAccessError() = %q, want the project number kept out of it", got)
	}
	if errors.Is(got, underlying) {
		t.Errorf("classifyAccessError() still unwraps to the underlying error; the text would leak through the chain")
	}
}

// 被包了一层的状态错误仍要被认出来。
//
// GAPIC 层不总是把 *status.Error 原样返回：它会包进 *apierror.APIError
// 再往上传。用 status.Code() 那种只做一次外层断言的写法，包了一层就退化
// 成 Unknown，NotFound 会被静默归到「解析失败」那一侧 —— 症状是运维照着
// 「凭据取不到」这条线索去查，而实际原因是引用写错了。
func TestWrappedNotFoundIsStillRecognised(t *testing.T) {
	wrapped := fmt.Errorf("AccessSecretVersion: %w", status.Error(codes.NotFound, "no such secret"))
	if got := classifyAccessError(wrapped); !errors.Is(got, secrets.ErrNotFound) {
		t.Fatalf("classifyAccessError(wrapped NotFound) = %v, want secrets.ErrNotFound", got)
	}
}

// GAPIC 真正会传上来的那个类型也要被认出来。
//
// 上一条用 fmt.Errorf 包一层是构造出来的形状；这一条用 apierror.APIError
// —— 客户端库实际返回的类型 —— 走一遍，免得分类逻辑只对测试里自制的
// 错误有效。
func TestAPIErrorNotFoundIsRecognised(t *testing.T) {
	ae, ok := apierror.FromError(status.Error(codes.NotFound, "no such secret"))
	if !ok {
		t.Fatal("apierror.FromError() = !ok, want a status error to convert")
	}
	if got := classifyAccessError(ae); !errors.Is(got, secrets.ErrNotFound) {
		t.Fatalf("classifyAccessError(*apierror.APIError NotFound) = %v, want secrets.ErrNotFound", got)
	}
	perm, ok := apierror.FromError(status.Error(codes.PermissionDenied, "denied"))
	if !ok {
		t.Fatal("apierror.FromError() = !ok, want a status error to convert")
	}
	if got := classifyAccessError(perm); errors.Is(got, secrets.ErrNotFound) {
		t.Fatal("classifyAccessError(*apierror.APIError PermissionDenied) = secrets.ErrNotFound, want a lookup failure")
	}
}

// 生产解析器必须在够到客户端之前先校验引用。
//
// 这条测的不是 ValidateRef 的行为（那有它自己的测试），而是**这个调用点
// 还在**。HANDOFF.md 记着同一个形状已经出现过四次：守卫本身测得很全，
// 却没有任何一条断言证明生产代码还在调用它 —— 删掉那三行，整个套件照绿。
//
// 用 nil client 构造是刻意的：它让断言变得锋利。校验还在，函数在拼资源
// 路径之前就返回 ErrInvalidRef；校验被删，第一件事就是解引用一个 nil
// 客户端，测试当场变红。换句话说，这条测试没有「碰巧通过」的路径。
func TestResolveRejectsABadRefBeforeTouchingTheClient(t *testing.T) {
	r := &Resolver{project: "my-proj", prefix: "distill-"}

	for _, ref := range []string{
		"",
		"../../other-project/secrets/prod",
		"projects/other/secrets/prod/versions/latest",
		"UPPER",
	} {
		got, err := r.Resolve(context.Background(), ref)
		if !errors.Is(err, secrets.ErrInvalidRef) {
			t.Errorf("Resolve(%q) = %v, want secrets.ErrInvalidRef", ref, err)
		}
		if got != nil {
			t.Errorf("Resolve(%q) returned %d bytes, want none", ref, len(got))
		}
	}
}
