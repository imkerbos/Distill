package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/imkerbos/Distill/internal/collectstore"
	"github.com/imkerbos/Distill/internal/fixture"
	"github.com/imkerbos/Distill/internal/registry"
	"github.com/imkerbos/Distill/internal/store"
)

// ErrNoCollectedReader 表示这个集群登记为 COLLECTED，而装配方没有交来读取
// 采集数据的 Reader。
//
// **单列一个哨兵，而不是退回 fixture。** 一个没有可用采集的集群，正确答案是
// 「没有数据」；回退到合成数据的后果是这个平台能造成的最严重后果 —— 操作者
// 读到一份写着真集群名字的完整报告，据此批准一次下发，而那些连接来自一个
// 虚构的集群，页面上没有任何症状会暴露它（design doc §2）。
var ErrNoCollectedReader = errors.New("no reader for a collected cluster yet")

// readerFor 按集群**登记的**数据来源装配一个 Reader。
//
// 只看 c.DataSource，不看「这个集群有没有数据」：推断意味着一次采集故障会让
// 一个真集群悄悄变回演示集群，而这件事在界面上完全看不出来（design doc §2）。
// 反过来也一样 —— 一个恰好在 fixture 里有数据的集群 ID，只要登记为 COLLECTED，
// 就不该拿到 fixture 的那份数据。
//
// collected 是采集侧的 Reader，取具体类型而不是 store.Reader：一个 nil 的
// 接口值与一个装着 nil 指针的接口值在 `== nil` 上不是一回事，而这里判空的
// 后果是「这个集群到底能不能被服务」。为 nil 时拒绝装配 —— 装配方没接上
// 采集侧读取面时，正确答案仍然是「没有数据」，不是 fixture（规范 §49）。
//
// default 分支拒绝而不是兜底：来源是封闭枚举，落到这里说明库里存着一个没有
// 登记过的取值，或者根本没登记。**失败方向朝关**（规范 §49）。
func readerFor(
	c registry.Cluster, reg store.ClusterSource, collected *collectstore.Reader,
) (store.Reader, error) {
	switch c.DataSource {
	case registry.DataSourceFixture:
		return newFixtureReader(reg), nil
	case registry.DataSourceCollected:
		if collected == nil {
			return nil, fmt.Errorf("%w: cluster %s", ErrNoCollectedReader, c.ID)
		}
		return collected, nil
	default:
		return nil, fmt.Errorf("cluster %s has an unregistered data source %q", c.ID, c.DataSource)
	}
}

// newFixtureReader 装出读 fixture 合成数据集的 Reader。
//
// **这是本进程里唯一一处 fixture.Load()**，而它总是把注册表包进
// fixtureOnlySource。于是「COLLECTED 集群不得通往 fixture」这一条不是靠上面
// 那个 switch 守住的 —— 那只是一个后人可以拨反的条件。真正守住它的是这份
// 收窄后的数据源本身，它同时决定两件事：
//
//   - 按集群解析的读方法（Topology / Quality / Security / PolicyPreview /
//     EnsureRuleExists / Flows(cluster=X)）解析不出这个集群，一律
//     ErrClusterNotFound；
//   - 不按集群解析的入口（Flow 按 ID 下钻、Flows 全量列表）取数时经过
//     FixtureReader.visibleFlows，而它要求一条记录的**两端**都在这份清单
//     里 —— 一条以 COLLECTED 集群为任一端的合成记录因此根本取不出来。
//
// 把 switch 拨反、把它交给一个 COLLECTED 集群，得到的仍然是「没有这个
// 集群的数据」，不是一份合成报告。
//
// 两道各自独立：拆掉任何一道，另一道仍然成立。这是纵深防御在这条链路上的
// 具体形态，不是重复。
func newFixtureReader(reg store.ClusterSource) *store.FixtureReader {
	return store.NewFixtureReader(fixture.Load(), fixtureOnlySource{reg: reg})
}

// fixtureOnlySource 把注册表收窄成「只有登记为 FIXTURE 的集群」。
//
// 收窄的是**数据读取**这一面，不是集群清单本身：集群列表页仍然走
// Deps.Registry，能看见全部集群（含 COLLECTED 的）。这里挡住的只是
// 「拿 fixture 的数字去回答一个真集群」。
type fixtureOnlySource struct {
	reg store.ClusterSource
}

// Clusters 只返回登记为 FIXTURE 的集群。
//
// 全部读路径的取数都拿这份清单做门禁（见 FixtureReader.visibleFlows），
// 因此它必须与 Cluster 用同一条判据收窄 —— 少收窄一处，就是留一个绕开
// 集群门禁的入口。
func (s fixtureOnlySource) Clusters(ctx context.Context) ([]registry.Cluster, error) {
	all, err := s.reg.Clusters(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]registry.Cluster, 0, len(all))
	for _, c := range all {
		if c.DataSource == registry.DataSourceFixture {
			out = append(out, c)
		}
	}
	return out, nil
}

// Cluster 只认登记为 FIXTURE 的集群，其余一律报「不存在」。
//
// 用 found=false 而不是另造一个错误：调用方是 FixtureReader，它对
// 「这个集群不归我管」只有这一种表达方式，而这条路径上真正要保证的是
// **绝不返回一份合成数据**，不是错误文案的分辨率。区分「没有采集」与
// 「集群不存在」属于采集侧 Reader 落地时的事（design doc §2）。
func (s fixtureOnlySource) Cluster(
	ctx context.Context, id string,
) (registry.Cluster, bool, error) {
	c, ok, err := s.reg.Cluster(ctx, id)
	if err != nil || !ok {
		return registry.Cluster{}, false, err
	}
	if c.DataSource != registry.DataSourceFixture {
		return registry.Cluster{}, false, nil
	}
	return c, true, nil
}

// RuleOverrides 原样透传。
//
// 不在这里收窄：人工决定是按集群主键取的，而调用方在读它之前已经用上面
// 那两个方法解析过集群 —— 一个 COLLECTED 集群走不到这里。再加一道过滤，
// 换来的不是安全，而是「覆盖决定为什么空了」这个问题多一个要排查的地方。
func (s fixtureOnlySource) RuleOverrides(
	ctx context.Context, clusterID string,
) ([]registry.RuleOverride, error) {
	return s.reg.RuleOverrides(ctx, clusterID)
}

// 编译期确认收窄后的数据源仍然满足 store 要的形状。
var _ store.ClusterSource = fixtureOnlySource{}
