package registry_test

import (
	"errors"
	"testing"

	"github.com/imkerbos/Distill/internal/predict"
	"github.com/imkerbos/Distill/internal/registry"
)

// writebackBinding 是一个已校验的绑定，policyPath 就是包含判断的那条边界。
func writebackBinding() registry.GitBinding {
	return registry.GitBinding{
		RepoID:     "repo-policies",
		PolicyPath: "clusters/prod-asia-1",
	}
}

// samplePlan 是一份合法的写回计划，各用例在它之上只改一个字段。
func samplePlan() registry.WritebackPlan {
	return registry.WritebackPlan{
		Branch: "distill/prod-asia-1-20260814T101500Z",
		Files: []registry.WritebackFile{
			{Path: "clusters/prod-asia-1/payment.yaml", Content: "kind: NetworkPolicy\n"},
			{Path: "clusters/prod-asia-1/api.yaml", Content: "kind: NetworkPolicy\n"},
		},
		Counts: map[predict.ChangeKind]int{
			predict.ChangeWouldBreak: 0,
			predict.ChangeWouldOpen:  2,
			predict.ChangeUnchanged:  41,
			predict.ChangeUnknown:    3,
		},
		Extraneous: []string{"clusters/prod-asia-1/legacy.yaml"},
	}
}

// mustPlan 走构造函数，因此拿到的指纹与生产路径上的是同一条。
func mustPlan(t *testing.T, p registry.WritebackPlan) registry.WritebackPlan {
	t.Helper()
	got, err := registry.NewWritebackPlan(writebackBinding(), p)
	if err != nil {
		t.Fatalf("NewWritebackPlan() error = %v, want nil", err)
	}
	return got
}

// 指纹覆盖全部待写文件的路径与内容、目标分支、重算后的计数
// （design doc 2026-08-14 §4）。少覆盖任何一项，操作者确认的就可能不是
// 他看过的那一份 —— 他在屏幕上批准一份计划，推出去的却是另一份。
//
// 逐字段改动、每一次都必须换指纹：只断言"改了内容指纹会变"的用例，对一份
// 换掉了目标分支或换掉了计数的计划照样是绿的。
func TestFingerprintChangesWithEveryFieldItMustCover(t *testing.T) {
	base := mustPlan(t, samplePlan())
	if base.Fingerprint == "" {
		t.Fatal("Fingerprint is empty")
	}

	// 每个变体只动一个字段。命名即断言的理由：指纹没变时，报出来的是
	// 「哪一项没进指纹」，而不是「指纹没变」。
	variants := map[string]func(p *registry.WritebackPlan){
		"file content": func(p *registry.WritebackPlan) {
			p.Files[0].Content = "kind: NetworkPolicy # 改了一个字节\n"
		},
		"file path": func(p *registry.WritebackPlan) {
			p.Files[0].Path = "clusters/prod-asia-1/payment-v2.yaml"
		},
		"file added": func(p *registry.WritebackPlan) {
			p.Files = append(p.Files, registry.WritebackFile{
				Path: "clusters/prod-asia-1/extra.yaml", Content: "kind: NetworkPolicy\n",
			})
		},
		"file removed": func(p *registry.WritebackPlan) {
			p.Files = p.Files[:1]
		},
		"branch": func(p *registry.WritebackPlan) {
			p.Branch = "distill/prod-asia-1-20260814T110000Z"
		},
	}
	// 四类计数逐个改：把 WOULD_BREAK 单独测掉、其余三类不管，等于让
	// "现在阻断的会被放开 2 条还是 20 条"可以在确认之后静悄悄地变。
	for _, k := range predict.AllChangeKinds() {
		kind := k
		variants["count "+string(kind)] = func(p *registry.WritebackPlan) {
			p.Counts[kind] = p.Counts[kind] + 7
		}
	}

	for name, mutate := range variants {
		t.Run(name, func(t *testing.T) {
			p := samplePlan()
			mutate(&p)
			got := mustPlan(t, p)
			if got.Fingerprint == base.Fingerprint {
				t.Errorf("fingerprint unchanged after %s changed: %s —— "+
					"操作者确认的会是他没看过的那一份", name, got.Fingerprint)
			}
		})
	}
}

// 两份内容相同的计划指纹必须相等：这条等式正是"没有变化就不推"
// （design doc §6 幂等）赖以成立的东西。不相等的话，一次什么都没改的
// 写回也会建分支、发提交，而 Git 历史里那串空提交会把"平台真的改过
// 什么"埋掉。
func TestFingerprintIsEqualForPlansWithTheSameContent(t *testing.T) {
	a := mustPlan(t, samplePlan())
	b := mustPlan(t, samplePlan())
	if a.Fingerprint != b.Fingerprint {
		t.Errorf("同内容两份计划指纹不等：%s vs %s", a.Fingerprint, b.Fingerprint)
	}

	// 文件顺序不是内容：同一批文件换个次序生成，仍是同一份要写进仓库的
	// 东西。顺序进指纹会让一次纯粹的遍历次序变化被报成"计划变了"。
	shuffled := samplePlan()
	shuffled.Files[0], shuffled.Files[1] = shuffled.Files[1], shuffled.Files[0]
	if got := mustPlan(t, shuffled); got.Fingerprint != a.Fingerprint {
		t.Errorf("换文件次序后指纹变了：%s vs %s", got.Fingerprint, a.Fingerprint)
	}
}

// 指纹不读调用方传进来的 Fingerprint 字段：读了的话，一个自带指纹的请求
// 就能让任意内容"对得上"，二次确认那道门当场失效。
func TestNewWritebackPlanIgnoresAnIncomingFingerprint(t *testing.T) {
	forged := samplePlan()
	forged.Fingerprint = "0000000000000000000000000000000000000000000000000000000000000000"
	got := mustPlan(t, forged)
	if got.Fingerprint == forged.Fingerprint {
		t.Error("构造出的指纹等于调用方自带的那个 —— 指纹成了调用方可控的值")
	}
	if got.Fingerprint != mustPlan(t, samplePlan()).Fingerprint {
		t.Error("自带指纹影响了算出来的指纹")
	}
}

// 文件路径必须落在绑定的 policyPath 之内（design doc §4，规范 §14/§15）。
// 一个能写出 policyPath 之外的计划，等于让平台改写仓库里任意文件 ——
// CI 配置、别的团队的 manifest 都在射程内。
//
// 逐形态列出而不是只测一个 ../：拒绝一种写法不代表拒绝这个问题，
// 而放过其中任何一种，效果与根本没有这道检查完全一样。
func TestPlanRefusesAPathOutsideThePolicyPath(t *testing.T) {
	outside := map[string]string{
		"父目录":           "clusters/prod-asia-1/../../.gitlab-ci.yml",
		"以 .. 开头":       "../.gitlab-ci.yml",
		"纯 ..":          "..",
		"绝对路径":          "/etc/passwd",
		"仓库根绝对路径":       "/clusters/prod-asia-1/api.yaml",
		"反斜杠穿越":         `clusters\..\..\.gitlab-ci.yml`,
		"反斜杠分隔":         `clusters\prod-asia-1\api.yaml`,
		"URL 编码穿越":      "clusters/prod-asia-1/%2e%2e/%2e%2e/.gitlab-ci.yml",
		"URL 双重编码穿越":    "clusters/prod-asia-1/%252e%252e/.gitlab-ci.yml",
		"同前缀的兄弟目录":      "clusters/prod-asia-1-old/api.yaml",
		"同前缀的兄弟文件":      "clusters/prod-asia-1.yaml",
		"另一个集群":         "clusters/prod-eu-1/api.yaml",
		"policyPath 本身": "clusters/prod-asia-1",
		"当前目录写法":        "./clusters/prod-asia-1/api.yaml",
		"内嵌当前目录":        "clusters/prod-asia-1/./api.yaml",
		"重复斜杠":          "clusters/prod-asia-1//api.yaml",
		"结尾斜杠":          "clusters/prod-asia-1/sub/",
		"控制字符":          "clusters/prod-asia-1/api.yaml\n../../ci.yml",
		"空路径":           "",
	}
	for name, p := range outside {
		t.Run(name, func(t *testing.T) {
			plan := samplePlan()
			plan.Files = []registry.WritebackFile{{Path: p, Content: "kind: NetworkPolicy\n"}}
			got, err := registry.NewWritebackPlan(writebackBinding(), plan)
			if !errors.Is(err, registry.ErrInvalid) {
				t.Fatalf("NewWritebackPlan(path=%q) error = %v, want ErrInvalid —— "+
					"这条路径写得出 policyPath 之外", p, err)
			}
			// 拒绝的计划不能带着指纹回去：一份能被确认、又不受包含约束的
			// 计划，与没有这道检查等价。
			if got.Fingerprint != "" {
				t.Errorf("被拒绝的计划仍带指纹 %q", got.Fingerprint)
			}
		})
	}
}

// policyPath 之内的路径必须被接受，包括更深的子目录：一道把合法路径也
// 拒掉的检查，会在轮 4 上线时表现成"平台什么都写不出去"，而排查的人
// 只看得到一句"路径不合法"。
func TestPlanAcceptsPathsInsideThePolicyPath(t *testing.T) {
	for _, p := range []string{
		"clusters/prod-asia-1/api.yaml",
		"clusters/prod-asia-1/payment/egress.yaml",
		"clusters/prod-asia-1/a/b/c/d.yaml",
	} {
		plan := samplePlan()
		plan.Files = []registry.WritebackFile{{Path: p, Content: "kind: NetworkPolicy\n"}}
		if _, err := registry.NewWritebackPlan(writebackBinding(), plan); err != nil {
			t.Errorf("NewWritebackPlan(path=%q) error = %v, want nil", p, err)
		}
	}
}

// 首尾斜杠是操作者常见的写法（gitverify.normalizePolicyPath 已经这样处理），
// 包含判断必须跟它一致 —— 两处对同一个 policyPath 得出不同的边界，
// 意味着校验通过的绑定在写回时写不出任何文件。
func TestPlanNormalisesThePolicyPathTheSameWayVerificationDoes(t *testing.T) {
	for _, policyPath := range []string{
		"clusters/prod-asia-1",
		"/clusters/prod-asia-1",
		"clusters/prod-asia-1/",
		"/clusters/prod-asia-1/",
	} {
		b := writebackBinding()
		b.PolicyPath = policyPath
		plan := samplePlan()
		plan.Files = []registry.WritebackFile{
			{Path: "clusters/prod-asia-1/api.yaml", Content: "kind: NetworkPolicy\n"},
		}
		if _, err := registry.NewWritebackPlan(b, plan); err != nil {
			t.Errorf("NewWritebackPlan(policyPath=%q) error = %v, want nil", policyPath, err)
		}
	}
}

// 一个能穿越的 policyPath 同样要拒：绑定是入库的操作者输入，
// "clusters/../" 这种取值会把边界拉回仓库根，包含判断随之作废。
func TestPlanRefusesAPolicyPathThatEscapesTheRepository(t *testing.T) {
	for _, policyPath := range []string{"..", "../elsewhere", "clusters/../..", "/", "%2e%2e"} {
		b := writebackBinding()
		b.PolicyPath = policyPath
		if _, err := registry.NewWritebackPlan(b, samplePlan()); !errors.Is(err, registry.ErrInvalid) {
			t.Errorf("NewWritebackPlan(policyPath=%q) error = %v, want ErrInvalid", policyPath, err)
		}
	}
}

// 计数是封闭枚举，不接受自由文本键：一个 "WOULD_MAYBE_BREAK" 落进计划，
// 界面照样渲染，统计口径当场失去意义（CLAUDE.md §3）。
func TestPlanRefusesCountsOutsideTheClosedEnum(t *testing.T) {
	plan := samplePlan()
	plan.Counts["WOULD_PROBABLY_BREAK"] = 1
	if _, err := registry.NewWritebackPlan(writebackBinding(), plan); !errors.Is(err, registry.ErrInvalid) {
		t.Errorf("NewWritebackPlan(unknown count kind) error = %v, want ErrInvalid", err)
	}
}

// 四类计数必须齐全：缺一类不是"那一类为零"，而是"没算过那一类"，
// 而写回前重算的全部意义就在这四个数字上（design doc §4）。
func TestPlanRefusesMissingCounts(t *testing.T) {
	plan := samplePlan()
	delete(plan.Counts, predict.ChangeWouldBreak)
	if _, err := registry.NewWritebackPlan(writebackBinding(), plan); !errors.Is(err, registry.ErrInvalid) {
		t.Errorf("NewWritebackPlan(missing WOULD_BREAK) error = %v, want ErrInvalid", err)
	}
}

// 没有目标分支的计划不成立：分支进指纹，而一个空分支名意味着操作者确认的
// 那份计划没有"推到哪里"这个答案。
func TestPlanRefusesAnEmptyBranch(t *testing.T) {
	plan := samplePlan()
	plan.Branch = ""
	if _, err := registry.NewWritebackPlan(writebackBinding(), plan); !errors.Is(err, registry.ErrInvalid) {
		t.Errorf("NewWritebackPlan(branch=\"\") error = %v, want ErrInvalid", err)
	}
}

// 同一条路径出现两次的计划是没有答案的：两份内容里哪一份最终落到仓库里，
// 取决于写入者的遍历次序。指纹相等因此不再意味着写出去的东西相同。
func TestPlanRefusesDuplicatePaths(t *testing.T) {
	plan := samplePlan()
	plan.Files = []registry.WritebackFile{
		{Path: "clusters/prod-asia-1/api.yaml", Content: "a\n"},
		{Path: "clusters/prod-asia-1/api.yaml", Content: "b\n"},
	}
	if _, err := registry.NewWritebackPlan(writebackBinding(), plan); !errors.Is(err, registry.ErrInvalid) {
		t.Errorf("NewWritebackPlan(duplicate path) error = %v, want ErrInvalid", err)
	}
}

// 构造函数不改调用方手上的那份计划：Files 被就地排序的话，调用方（以及
// 并发读同一份计划的展示路径）看到的次序会在算指纹的一瞬间变掉（规范 §25）。
func TestNewWritebackPlanDoesNotMutateTheInputPlan(t *testing.T) {
	in := samplePlan()
	firstPath := in.Files[0].Path
	if _, err := registry.NewWritebackPlan(writebackBinding(), in); err != nil {
		t.Fatalf("NewWritebackPlan() error = %v", err)
	}
	if in.Files[0].Path != firstPath {
		t.Errorf("入参的 Files 被就地重排：%q -> %q", firstPath, in.Files[0].Path)
	}
}

// FingerprintOf 对一份合法计划与构造函数算出的是同一个值：两者算法一旦
// 分叉，二次确认比的就是两个不同的东西，而这正是它要防的那件事。
func TestFingerprintOfMatchesTheConstructedPlan(t *testing.T) {
	got := mustPlan(t, samplePlan())
	if registry.FingerprintOf(got) != got.Fingerprint {
		t.Errorf("FingerprintOf() = %s, plan.Fingerprint = %s",
			registry.FingerprintOf(got), got.Fingerprint)
	}
}
