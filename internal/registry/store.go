package registry

import (
	"context"
	"errors"
)

// ErrNotFound 表示目标不存在。
//
// 定义在本包而非各实现里：handler 需要把它映射成业务错误码，
// 而边界层依赖某个具体存储实现的哨兵，会让将来换存储变成跨层改动。
var ErrNotFound = errors.New("registry target not found")

// Actor 是操作者身份，写入审计。
type Actor struct {
	// Username 是操作者登录名。
	Username string
}

// Store 是集群注册与策略导入的持久化接口。
//
// 只暴露完整的业务操作，不暴露事务句柄：任何能绕开审计的写路径，
// 都会在事故复盘时变成一段无法解释的历史 —— 而那正是最需要解释的时候。
type Store interface {
	// Clusters 返回全部未删除的集群。
	Clusters(ctx context.Context) ([]Cluster, error)
	// Cluster 按 ID 查一个未删除的集群。不存在时第二个返回值为 false。
	Cluster(ctx context.Context, id string) (Cluster, bool, error)
	// CreateCluster 注册一个集群，同事务写审计。
	CreateCluster(ctx context.Context, actor Actor, c Cluster) error
	// UpdateCluster 修改集群，同事务写审计。集群不存在时返回 ErrNotFound。
	UpdateCluster(ctx context.Context, actor Actor, c Cluster) error
	// SoftDeleteCluster 下线集群，同事务写审计。
	//
	// 软删除而非物理删除：级联删除会连带删掉覆盖决定，
	// 而那些行记录着谁在什么时候以什么理由启用了一条风险规则。
	SoftDeleteCluster(ctx context.Context, actor Actor, id string) error

	// PolicyImports 返回一个集群下未删除的导入策略。
	PolicyImports(ctx context.Context, clusterID string) ([]PolicyImport, error)
	// CreatePolicyImport 记录一条导入，同事务写审计。
	CreatePolicyImport(ctx context.Context, actor Actor, p PolicyImport) error
	// SoftDeletePolicyImport 删除一条导入，同事务写审计。
	SoftDeletePolicyImport(ctx context.Context, actor Actor, clusterID, importID string) error
}
