package registry_test

import (
	"strings"
	"testing"

	"github.com/imkerbos/Distill/internal/registry"
)

// **默认为空 = 平台不解释任何第二平面。**
//
// 这是这个字段唯一安全的默认值：解释一个 CNI 其实不执行的平面，会让平台
// 以为某条连接被 Deny 拦了 → 判 DENY → 不为它生成放行规则 → 下发后真的
// 被拦断。而"漏解释一个真在执行的平面"的方向是保守的（照旧整片降级）。
func TestEnforcedPlanesDefaultsToNone(t *testing.T) {
	c := validCluster()
	if len(c.EnforcedPlanes) != 0 {
		t.Errorf("EnforcedPlanes 默认非空：%v", c.EnforcedPlanes)
	}
	if err := registry.ValidateCluster(c); err != nil {
		t.Errorf("默认（空）登记被拒：%v", err)
	}
}

// 声明必须带理由。
//
// 这是一次会改变判定的决定：声明之后平台会按那个平面的语义算，
// 而算错的方向是把一条通着的连接判成不通。没有理由的决定在事后复盘时
// 与"手滑填上去的"分不开 —— 与 ManagedSystemNamespaces 同一形状。
func TestEnforcedPlanesRequireAReason(t *testing.T) {
	c := validCluster()
	c.EnforcedPlanes = []registry.EnforcedPlane{registry.PlaneAdminNetworkPolicy}
	err := registry.ValidateCluster(c)
	if err == nil {
		t.Fatal("声明了执行平面却没写理由，登记被接受了")
	}
	if !strings.Contains(err.Error(), "理由") {
		t.Errorf("拒绝理由没指向该填的东西：%v", err)
	}

	c.EnforcedPlanesReason = "本集群跑原生 Calico v3.30，实测 ANP 生效"
	if err := registry.ValidateCluster(c); err != nil {
		t.Errorf("带了理由仍被拒：%v", err)
	}
}

// 取值必须在封闭枚举内。
//
// 自由文本会让"这个集群执行哪些平面"变成一次字符串匹配，而拼错的那一次
// 表现为"声明了却没生效"——静默、且方向看起来是安全的，因此没人会去查。
func TestEnforcedPlanesRejectUnknownValues(t *testing.T) {
	c := validCluster()
	c.EnforcedPlanes = []registry.EnforcedPlane{"SOMETHING_ELSE"}
	c.EnforcedPlanesReason = "随便写的"
	if err := registry.ValidateCluster(c); err == nil {
		t.Error("枚举外的取值被接受了")
	}
}

// 三个已登记的平面都接受。
func TestEnforcedPlanesAcceptsRegisteredValues(t *testing.T) {
	for _, p := range registry.AllEnforcedPlanes() {
		c := validCluster()
		c.EnforcedPlanes = []registry.EnforcedPlane{p}
		c.EnforcedPlanesReason = "实测过"
		if err := registry.ValidateCluster(c); err != nil {
			t.Errorf("%s 被拒：%v", p, err)
		}
	}
}

// **Enforces 只认已声明的那些。**
//
// 它是判定路径要问的那个问题（"这个平面要不要按语义算"），因此必须
// 只在操作者明示过时才答 true —— 探测到平面存在**不**代表 CNI 执行它。
func TestEnforcesOnlyAnswersTrueForDeclaredPlanes(t *testing.T) {
	c := validCluster()
	if c.Enforces(registry.PlaneAdminNetworkPolicy) {
		t.Error("没有任何声明时就答「执行 ANP」")
	}
	c.EnforcedPlanes = []registry.EnforcedPlane{registry.PlaneAdminNetworkPolicy}
	if !c.Enforces(registry.PlaneAdminNetworkPolicy) {
		t.Error("声明了却答不执行")
	}
	if c.Enforces(registry.PlaneCiliumNetworkPolicy) {
		t.Error("只声明了 ANP，却答也执行 CNP")
	}
}
