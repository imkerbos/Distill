package secrets

import (
	"context"
	"errors"
	"fmt"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ResourceName 把短名拼成 Secret Manager 的完整资源路径。
//
// project 与 prefix 来自配置，ref 是绑定里存的短名 —— 三者的来源不同，
// 这正是 spec §2.1 的全部意图：操作者能影响的只有最后一段，且这一段的
// 字符集已被 ValidateRef 收窄，因此他无法表达出配置划定的这个前缀之外的
// 任何资源路径。越权不是被检查出来的，是拼不出来的。
//
// 版本固定取 latest：轮换后不需要改绑定，见 spec §2.1。
func ResourceName(project, prefix, ref string) string {
	return fmt.Sprintf("projects/%s/secrets/%s%s/versions/latest", project, prefix, ref)
}

// GCPResolver 从 Google Secret Manager 解析凭据，是生产实现。
//
// 身份走 Application Default Credentials，在 GKE 上即 Workload Identity。
// 这里不提供、也不接受服务账号密钥文件路径：v4 spec §9.8 把密钥文件排除在
// 外 —— 不存在的密钥不会泄漏、不需要轮换、不会被误提交。
type GCPResolver struct {
	client  *secretmanager.Client
	project string
	prefix  string
}

// 编译期确认 GCPResolver 满足 Resolver。
var _ Resolver = (*GCPResolver)(nil)

// NewGCPResolver 构造一个 Secret Manager 解析器。
//
// project 与 prefix 都不接受空值。空 prefix 一样能拼出合法路径，但那样
// 围栏就只剩项目一层，任何短名都能指向项目里的任意 secret —— 这与 §2.1
// 想要的「结构上只够得着该够得着的那批」不是一回事。缺失在这里变成构造
// 失败，也就是启动失败。
func NewGCPResolver(ctx context.Context, project, prefix string) (*GCPResolver, error) {
	if project == "" || prefix == "" {
		return nil, fmt.Errorf("secret manager resolver: project and prefix are both required")
	}
	client, err := secretmanager.NewClient(ctx)
	if err != nil {
		// 构造失败说明这个环境根本拿不到身份，与某一次取值失败不是一回事，
		// 这里透传底层错误：它到不了 API 边界，只会出现在启动日志里。
		return nil, fmt.Errorf("secret manager resolver: create client: %w", err)
	}
	return &GCPResolver{client: client, project: project, prefix: prefix}, nil
}

// Resolve 取 ref 对应 secret 的最新版本。
//
// 返回的字节是私钥内容，只在内存中存在：不落盘、不进日志、不进错误信息。
func (r *GCPResolver) Resolve(ctx context.Context, ref string) ([]byte, error) {
	if err := ValidateRef(ref); err != nil {
		return nil, err
	}

	resp, err := r.client.AccessSecretVersion(ctx, &secretmanagerpb.AccessSecretVersionRequest{
		Name: ResourceName(r.project, r.prefix, ref),
	})
	if err != nil {
		return nil, classifyAccessError(err)
	}
	return resp.GetPayload().GetData(), nil
}

// classifyAccessError 把 Secret Manager 的错误压成两种结论。
//
// 只有 NotFound 映射成 ErrNotFound：引用取不到是平台侧配置问题，取到了
// 但认证失败是仓库侧权限问题，两者的处置人不同（见 resolver.go 与 §2.4）。
// PermissionDenied 不算 NotFound —— 服务账号没有访问权同样是平台侧问题，
// 但它不是「不存在」，混成一类会把「权限没开」说成「引用写错了」。
//
// 其余一律压成一句固定文本，不用 %w：gitverify 会把这些错误归进一个封闭
// 枚举，而云 SDK 的错误文本会带上资源路径、项目号与内部重试细节，那些东西
// 一旦搭着错误链走到 API 边界就出不去了。
//
// 状态码用 status.Code 取而不是自己做类型断言：GAPIC 层会把状态错误包进
// *apierror.APIError 再往上传，而当前 grpc 版本的 status.Code 会沿错误链
// 解包。这是依赖的行为不是本包的，所以 gcp_internal_test.go 里对「包了一层
// 的 NotFound」单独有一条断言 —— 哪天升级把解包去掉了，那条会红，而不是
// 悄悄把 NotFound 错分到「解析失败」那一侧。
func classifyAccessError(err error) error {
	if status.Code(err) == codes.NotFound {
		return ErrNotFound
	}
	return errors.New("resolve secret ref: access failed")
}
