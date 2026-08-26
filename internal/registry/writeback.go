package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/imkerbos/Distill/internal/predict"
)

// WritebackFile 是计划里的一个待写文件。
//
// 内容是渲染好的完整文件，不是补丁：写回只新增与更新整份文件
// （design doc 2026-08-14 §3），而一份"改哪几行"的描述要靠仓库当前内容
// 才解释得了，那正是操作者在屏幕上看不到的东西。
type WritebackFile struct {
	// Path 是仓库根起算的路径，必须落在绑定的 policyPath 之内。
	Path string `json:"path"`
	// Content 是文件的完整内容。
	Content string `json:"content"`
}

// DeletionClass 是一个多余文件的处置分类（design doc 2026-08-24 §4.2）。
//
// 封闭枚举，不用自由文本：界面按它决定给不给删除入口，而一个界面要靠字符串
// 比较去猜的分类，迟早会出现一个谁也没定义过的取值被当成"可以删"。
type DeletionClass string

const (
	// DeletionDeletable 表示能解析、删除影响也算得出来，可以被确认删除。
	DeletionDeletable DeletionClass = "DELETABLE"
	// DeletionNotApplied 表示仓库里有、集群里没有：删掉它对集群没有影响。
	//
	// 仍然要人确认，不自动删：平台判断"集群里没有"依据的是采集结果，而一次
	// 采集故障与"这个对象真的不存在"在数据里长得一样。
	DeletionNotApplied DeletionClass = "NOT_APPLIED"
	// DeletionImpactUnknown 表示时间窗内没有观测能支撑一次删除影响预测。
	//
	// **不提供删除入口。** 一句"大概没事"不是一次评估，而删除恰恰是不能凭
	// 大概去做的那一类。
	DeletionImpactUnknown DeletionClass = "IMPACT_UNKNOWN"
	// DeletionUnparseable 表示平台没能把它解析成 NetworkPolicy。
	//
	// 别人放的文件、模板、被手工改坏的策略都落在这里。**永不提供删除**：
	// 一个平台看不懂的文件，它对集群的作用平台也算不出来。
	DeletionUnparseable DeletionClass = "UNPARSEABLE"
)

// confirmable 报告这一类是否允许被确认删除。
func (c DeletionClass) confirmable() bool {
	return c == DeletionDeletable || c == DeletionNotApplied
}

// WritebackExclusion 是这一轮被排除出写回的一个主体，以及排除的依据
// （design doc 2026-08-25-trust-engineering §3.4）。
//
// **排除而不是整次拒绝。** 平台判定与集群实际执行分歧超阈，说明这个主体的
// 候选规则很可能缺了现在正通着的放行 —— 而 dry-run 看不出来，因为在平台的
// 世界里那些连接本来就不通。把整个集群的写回卡住，实际后果是这个平台在任何
// 带第二策略平面的集群上永远出不了计划，而那正是它要服务的集群；运营上这会
// 逼人去调阈值绕过门禁。风险局部化、写下来、留在提交信息里，比一道会被绕过去
// 的门更可靠。
type WritebackExclusion struct {
	// Namespace 与 Workload 是被排除的主体。
	//
	// namespace 粒度下 Workload 为空：那一层的候选是折叠过的，只排除其中
	// 一个 workload 无从表达，因此整个 namespace 一起排除 —— 保守方向。
	Namespace string `json:"namespace"`
	Workload  string `json:"workload"`
	// UnderPermissiveRate 是这个主体上"平台判 DENY、集群实际放行"的占比。
	//
	// 报出具体数字而不是一个布尔：0.06 与 0.9 都超阈，但前者值得去看明细、
	// 后者说明平台在这个主体上基本不成立，两者的处置不是一回事。
	UnderPermissiveRate float64 `json:"underPermissiveRate"`
}

// Label 返回这个主体在文案里的写法。
func (e WritebackExclusion) Label() string {
	if e.Workload == "" {
		return e.Namespace
	}
	return e.Namespace + "/" + e.Workload
}

// WritebackDeletion 是策略路径下一个本次候选集不包含的文件，以及平台对它的
// 处置结论（design doc 2026-08-24 §4）。
type WritebackDeletion struct {
	// Path 是仓库根起算的路径。
	Path string `json:"path"`
	// Class 是处置分类。
	Class DeletionClass `json:"class"`
	// Documents 是从这个文件里解析出的 NetworkPolicy 份数。
	Documents int `json:"documents"`
	// Counts 是删掉它之后的四类计数；只有 DELETABLE 有。
	//
	// NOT_APPLIED 不带计数，而不是带一份全零：全零读起来是"评估过、影响是零"，
	// 而那一类的事实是"集群里根本没有这些对象"，两句话在界面上应当分开说。
	Counts map[predict.ChangeKind]int `json:"counts,omitempty"`
}

// WritebackPlan 是一次写回的完整计划：将要写什么、写到哪、写之前重算出的
// 数字，以及仓库里多余的文件（design doc 2026-08-14 §4）。
//
// 计划是操作者二次确认的对象，因此它必须是自足的 —— 屏幕上那份计划之外
// 的任何东西都不该影响推送出去的结果。
type WritebackPlan struct {
	// RepoID 是这次写回要写到的那个策略仓库（design doc §4）。
	//
	// 由 NewWritebackPlan 从绑定里取，入参里的取值一律丢弃：它是**进指纹**
	// 的东西，读调用方给的那一份就等于让调用方自己声明写到哪儿。
	//
	// 不进指纹的话，在出计划与推送之间把绑定改指到另一个仓库，操作者读过的
	// 每一句话都还成立、指纹照样对得上，推送却落到了另一个仓库 —— 而"推到
	// 哪里"正是他没有明示批准过的那一维。仓库地址本身不进计划：它是内部
	// 地址，标识足以判等（§7 不含内部地址那条同源）。
	RepoID string `json:"repoId"`
	// Files 是将要新增或更新的文件。平台从不删除仓库里的文件（§3）。
	Files []WritebackFile `json:"files"`
	// Branch 是目标分支，永远是新建的 distill/* 分支，不是绑定里那条（§2）。
	Branch string `json:"branch"`
	// CommitMessage 是提交信息：集群、时间窗、重算后的四类计数、发起者账号、
	// 平台版本（§7）。**不含**凭据、host key 或任何内部地址。
	//
	// 由计划携带、而不是由写入端现场拼：它是合并请求上的评审人**唯一会读的
	// 那句话**，据此决定合不合。不进计划就意味着它不在操作者确认的那份东西
	// 里，那时提交信息可以说任何话，而它会永久留在仓库历史上。
	//
	// 作者身份不在这里：提交作者永远是平台，发起者只在这段文字与审计行里
	// 出现（§7）。冒充操作者会让 Git 的作者字段失去意义。
	CommitMessage string `json:"commitMessage"`
	// Counts 是写回这一刻重新跑出来的四类计数（§4）。
	//
	// 写回请求不携带数字，这一份必须由平台重算：操作者确认规则的时刻与
	// 写回的时刻之间，集群、流量窗口、别人的确认都可能变了。
	Counts map[predict.ChangeKind]int `json:"counts"`
	// Deletions 是仓库路径下已有、但本次候选集不包含的文件，带平台对每一个的
	// 处置结论（design doc 2026-08-24 §4）。
	//
	// **不进指纹**，与 Extraneous 同一条理由：它描述的是仓库现状，不是这次要
	// 写出去的内容。真正进指纹的是 Confirmed —— 操作者从这份清单里挑出来的
	// 那几条。
	Deletions []WritebackDeletion `json:"deletions"`
	// Confirmed 是操作者确认要删除的路径，**进指纹**。
	//
	// 它是这份计划里唯一一项来自请求的内容，因此判据必须由平台这一刻重算出的
	// Deletions 给出：路径必须出现在那份清单里、且那一类允许被删。少了这道
	// 判断，一个构造出来的请求可以删掉策略目录下的任意文件。
	Confirmed []string `json:"confirmed"`
	// Extraneous 是仓库路径下已有、但本次候选集不包含的文件，交人工处置。
	//
	// 它必须来自平台真的枚举过一次仓库：留空在界面上读起来是"没有多余文件"，
	// 而那是一句没有人算过的断言，且偏在让人放心的方向（§4）。
	Extraneous []string `json:"extraneous"`
	// Exclusions 是这一轮被排除出写回的主体，带排除依据。
	//
	// **不进指纹**，与 Extraneous 同一条理由：它描述的是"这次没写谁"，而
	// 后果已经体现在 Files 里 —— 那些主体的文件根本不在其中，内容指纹已经
	// 覆盖。但它**必须进提交信息**：一份少了三个 workload 的策略集，如果
	// 不说明，评审人读到的就是"这个集群只有这些 workload"。
	Exclusions []WritebackExclusion `json:"exclusions"`
	// ExistingBranches 是仓库上现存的 distill/* 分支（§2）。
	//
	// 报的是"存在"，不是"未合并"：判断合并与否要在部署分支的历史里找它的
	// tip，而写回全程只做 Depth 1 的浅克隆。攒着几条分支这个信号仍然给得出，
	// 但不冒充平台没算过的结论 —— 界面上同样要写明这一点。
	ExistingBranches []string `json:"existingBranches"`
	// Fingerprint 是这份计划的内容指纹，由 NewWritebackPlan 填。
	//
	// 推送必须由操作者对着它二次确认（§4）：确认的必须是他真正看过的
	// 那一份，不是"当时那一份的后续版本"。
	Fingerprint string `json:"fingerprint"`
}

// Writeback 是一次写回事件的审计内容（design doc 2026-08-14 §9）。
//
// 不含文件内容：体积不可控，而策略内容本身已经在 Git 里 —— 审计要回答的
// 是"谁在什么时候把哪一份推到了哪条分支"，不是把推出去的东西再存一份。
type Writeback struct {
	// Branch 是目标分支。
	Branch string `json:"branch"`
	// Files 是这次计划涉及的文件数。
	Files int `json:"files"`
	// Counts 是重算后的四类计数。
	Counts map[predict.ChangeKind]int `json:"counts"`
}

// WritebackStore 是写回所需的持久化接口。
//
// 收窄成两个方法而不是往 Store 上加（与 settings.Provider、
// auth.AccountSource 同一个理由）：写回路径只需要"记一次计划"与"记一次
// 推送并落 commit"，而 Store 还带着集群、账号、设置的全部写方法。
// mysqlregistry.Store 同时满足两者。
type WritebackStore interface {
	// RecordWritebackPlan 为一次出计划写一条审计行，动作 PLAN_POLICY_WRITEBACK。
	//
	// 与推送分成两个动作（design doc §9）：出计划是常规动作，推送是一次
	// 仓库变更，混成一条会让"谁真的改了仓库"淹没在计划请求里。
	RecordWritebackPlan(ctx context.Context, actor Actor, clusterID string, w Writeback) error
	// SetLastWrittenCommit 写入绑定的 last_written_commit，同事务写审计，
	// 动作 PUSH_POLICY_WRITEBACK。绑定不存在时返回 ErrNotFound。
	//
	// 记的是平台推上去的那个 commit，不是合并后的（design doc §8）：合并
	// 由人做，平台不知道也不该假装知道 —— 漂移检测拿它比对的是"平台最后
	// 交出去的是什么"。
	SetLastWrittenCommit(ctx context.Context, actor Actor, clusterID, commitSHA string, w Writeback) error
}

// NewWritebackPlan 校验一份写回计划并算出它的指纹。
//
// 校验与算指纹是同一个动作，不拆成两步：指纹是操作者二次确认的凭据，
// 而一份能拿到指纹、却没有被约束在 policyPath 之内的计划，与根本没有这道
// 检查等价 —— 拆开之后，"忘了先调校验"就是一条无声的越权写入路径。
//
// 入参里的 Fingerprint 一律丢弃、重算：读它就意味着调用方能自带一个指纹让
// 任意内容"对得上"，二次确认那道门当场失效（规范 §4：不信任输入）。
func NewWritebackPlan(b GitBinding, p WritebackPlan) (WritebackPlan, error) {
	if err := ValidateGitBinding(b); err != nil {
		return WritebackPlan{}, err
	}
	root, err := writebackRoot(b.PolicyPath)
	if err != nil {
		return WritebackPlan{}, err
	}
	if p.Branch == "" {
		return WritebackPlan{}, invalid("写回计划的目标分支不能为空")
	}
	if err := validateCounts(p.Counts); err != nil {
		return WritebackPlan{}, err
	}

	// 仓库标识来自绑定，不来自入参：与 Fingerprint 同一条理由，一个能自带
	// 仓库标识的调用方等于自己指定写到哪儿，而那正是指纹要钉住的那一维。
	p.RepoID = b.RepoID

	seen := make(map[string]struct{}, len(p.Files))
	for i, f := range p.Files {
		if err := checkWithinPolicyPath(root, f.Path); err != nil {
			return WritebackPlan{}, err
		}
		if _, dup := seen[f.Path]; dup {
			// 同一条路径出现两次时，最终落到仓库里的是哪一份取决于写入者的
			// 遍历次序 —— 指纹相等于是不再意味着写出去的东西相同。
			return WritebackPlan{}, invalidf("files[%d] 路径 %q 重复", i, f.Path)
		}
		seen[f.Path] = struct{}{}
	}

	if err := validateConfirmedDeletions(root, p); err != nil {
		return WritebackPlan{}, err
	}

	p.Fingerprint = FingerprintOf(p)
	return p, nil
}

// FingerprintOf 计算一份写回计划的内容指纹。
//
// 覆盖目标仓库、全部待写文件的路径与内容、目标分支、提交信息、重算后的
// 四类计数（design doc 2026-08-14 §4）。少覆盖任何一项，操作者屏幕上看过
// 的那份计划就能在确认与推送之间悄悄换掉 —— 他确认的是一份自己从没读过的
// 计划。路径与内容必须同时进：只覆盖内容的话，同一份 YAML 换个落点就能写到
// 另一个文件上；只覆盖路径的话，文件里写什么都无所谓。
//
// **Extraneous 与 ExistingBranches 不进指纹**：它们描述的是仓库里平台不会
// 碰的东西，交人工处置（§2、§3），不是这次要写出去的内容。把它们算进来，
// 别人往策略目录里加一个无关文件、或另一个集群建了一条 distill/* 分支，就会
// 让一份已确认的计划作废，而那次推送本身没有任何变化。
//
// 文件按 (路径, 内容) 排序后再哈希：文件次序不是内容，同一批文件换个遍历
// 次序仍是同一份要写进仓库的东西，而"内容相同则指纹相同"正是"没有变化就
// 不推"（§6 幂等）赖以成立的等式。排序在副本上做，不动调用方的切片。
//
// 各字段按「长度 + ':' + 内容」写入，不用分隔符：文件内容是任意字节，
// 任何固定分隔符都可能出现在内容里，那时 ["a","bc"] 与 ["ab","c"] 会碰撞成
// 同一个指纹 —— 朝着不安全的方向失败，且不报错。
func FingerprintOf(p WritebackPlan) string {
	h := sha256.New()
	write := func(s string) {
		_, _ = h.Write([]byte(strconv.Itoa(len(s))))
		_, _ = h.Write([]byte{':'})
		_, _ = h.Write([]byte(s))
	}

	// 仓库标识排在最前：它回答的是"推到哪儿"，而其余各项回答的是"推什么"。
	// 不覆盖它，一份内容与分支都对得上的计划可以落到另一个仓库。
	write(p.RepoID)
	write(p.Branch)
	// 提交信息必须进指纹：它是这份计划里唯一会**永久落进仓库历史**、又被
	// 合并请求上的评审人当作判断依据的一段文字。不覆盖它，一份已确认的计划
	// 就能在确认与推送之间换掉这句话 —— 文件与分支都对得上，评审人读到的
	// 理由却不是操作者批准的那个。
	write(p.CommitMessage)

	// 计数按 AllChangeKinds 的固定次序写，不遍历 map：map 的迭代次序随机，
	// 直接遍历会让同一份计划两次算出不同的指纹。
	for _, k := range predict.AllChangeKinds() {
		write(string(k))
		write(strconv.Itoa(p.Counts[k]))
	}

	// 被确认的删除进指纹（design doc 2026-08-24 §4.4）：操作者批准的是
	// "写这几个文件**并删掉那几个**"，少覆盖后半句，一次删除就能在确认与
	// 推送之间被加进去或换掉。排序后写入 —— 与文件同理，次序不是内容。
	confirmed := make([]string, len(p.Confirmed))
	copy(confirmed, p.Confirmed)
	sort.Strings(confirmed)
	write(strconv.Itoa(len(confirmed)))
	for _, path := range confirmed {
		write(path)
	}

	files := make([]WritebackFile, len(p.Files))
	copy(files, p.Files)
	sort.Slice(files, func(i, j int) bool {
		if files[i].Path != files[j].Path {
			return files[i].Path < files[j].Path
		}
		// 路径相同时按内容定序：NewWritebackPlan 已经拒了重复路径，但
		// FingerprintOf 是导出的，一个未经校验的输入不该让排序结果不确定。
		return files[i].Content < files[j].Content
	})
	write(strconv.Itoa(len(files)))
	for _, f := range files {
		write(f.Path)
		write(f.Content)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ValidateWriteback 校验一条写回审计记录。
//
// 计数在这里再校验一次而不是信任计划：审计行是事后唯一的凭据，一条
// 带着自由文本计数键的记录会让统计口径当场失去意义（CLAUDE.md §3）。
func ValidateWriteback(w Writeback) error {
	if w.Branch == "" {
		return invalid("写回审计的目标分支不能为空")
	}
	if w.Files < 0 {
		return invalidf("写回审计的文件数 %d 不能为负", w.Files)
	}
	return validateCounts(w.Counts)
}

// ValidateCommitSHA 校验一个完整的 Git commit SHA。
//
// 复用 ValidatePolicyImport 那条「不接受缩写」的规则（见 gitCommitSHALength）：
// last_written_commit 是漂移检测的基准，而缩写在仓库变大后可能歧义 ——
// 一个「平台最后交出去的是这个」的标记指向两个提交，比没有标记更糟。
func ValidateCommitSHA(sha string) error {
	if !isHexSHA(sha) {
		return invalidf("commit SHA %q 不是 %d 位十六进制的完整 commit SHA",
			sha, gitCommitSHALength)
	}
	return nil
}

// validateCounts 要求四类计数齐全且只有登记过的取值。
//
// 缺一类不是"那一类为零"，而是"没算过那一类" —— 而写回前重算的全部意义
// 就在这四个数字上（design doc §4）。
func validateCounts(counts map[predict.ChangeKind]int) error {
	for k := range counts {
		if !k.Valid() {
			return invalidf("计数 %q 不在已登记的变化类型内", k)
		}
	}
	for _, k := range predict.AllChangeKinds() {
		if _, ok := counts[k]; !ok {
			return invalidf("四类计数缺少 %q", k)
		}
	}
	return nil
}

// writebackRoot 把绑定的 policyPath 规范成包含判断用的仓库内根路径。
//
// 首尾斜杠按 gitverify.normalizePolicyPath 同样的方式去掉：操作者填
// "/policies/prod" 或 "policies/prod/" 都是常见写法，两处对同一个
// policyPath 得出不同的边界，会让一个校验通过的绑定在写回时写不出任何文件。
//
// 空根（policyPath 是 "/" 或全是斜杠）一律拒绝：那等于把边界拉回仓库根，
// 包含判断随之作废，而这道判断挡的正是"改写仓库里任意文件"。
func writebackRoot(policyPath string) (string, error) {
	root, err := cleanRepoPath("policyPath", strings.Trim(policyPath, "/"))
	if err != nil {
		return "", err
	}
	return root, nil
}

// checkWithinPolicyPath 断言一条待写路径落在 root 之内。
//
// 比较在**路径段边界**上做，不是拿原始串比前缀（规范 §15）：
// root="clusters/prod-asia-1" 时，"clusters/prod-asia-1-old/api.yaml" 与
// "clusters/prod-asia-1.yaml" 都以 root 开头，却是另一个集群、另一个团队
// 的文件。要求 root + "/" 前缀之后，这两种写法落不进来。
//
// 相等（路径就是 root 本身）同样拒绝：policyPath 是策略的**根路径**
// （GitBinding.PolicyPath），把一份文件写到一个目录的位置上不是一次策略
// 更新，是一次会让整个目录消失的写入。
func checkWithinPolicyPath(root, p string) error {
	cleaned, err := cleanRepoPath("files 路径", p)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(cleaned, root+"/") {
		return invalidf("files 路径 %q 不在绑定的 policyPath %q 之内 —— "+
			"一个能写出 policyPath 之外的计划，等于让平台改写仓库里任意文件", p, root)
	}
	return nil
}

// cleanRepoPath 把一条仓库内路径规范成用于比较的形式，并拒掉一切不是
// 「一条干净的仓库内相对路径」的写法（规范 §14、§15）。
//
// 一律拒绝、不做修补：修补意味着操作者屏幕上看到的路径与真正写下去的路径
// 可以不同，而路径正是指纹覆盖的东西之一。写法必须唯一，判等才有意义。
func cleanRepoPath(field, p string) (string, error) {
	if p == "" {
		return "", invalidf("%s 不能为空", field)
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			// 带换行的路径在任何按行处理的地方都会裂成两段 —— 提交信息、
			// 日志、写回报告都按行走。
			return "", invalidf("%s %q 含控制字符", field, p)
		}
	}
	// 百分号一律拒绝，不做解码：%2e%2e%2f 与 %252e%252e 这类写法要么是一次
	// 穿越尝试，要么是一个没人想要的文件名。解码之后再判断，就要回答"解几层"
	// 这个没有正确答案的问题 —— 多解一层是漏，少解一层也是漏。
	if strings.Contains(p, "%") {
		return "", invalidf("%s %q 含百分号：仓库内路径不接受任何编码形式", field, p)
	}
	// Git 的路径分隔符只有 /。反斜杠既可能是 ..\ 这种穿越写法，也可能在
	// Windows 检出时变成一层目录 —— 两种都不是这里想要的东西。
	if strings.Contains(p, `\`) {
		return "", invalidf(`%s %q 含反斜杠：仓库内路径的分隔符只有 /`, field, p)
	}
	if strings.HasPrefix(p, "/") {
		return "", invalidf("%s %q 是绝对路径", field, p)
	}
	// path.Clean 的结果与原串不同，说明原串里有 ./、//、结尾斜杠或 ../ ——
	// 同一个文件因此有多种写法，而指纹覆盖的是路径本身：接受其中任意一种
	// 都会让两份写出同样文件的计划得到不同的指纹。
	if cleaned := path.Clean(p); cleaned != p {
		return "", invalidf("%s %q 不是规范写法（应为 %q）", field, p, cleaned)
	}
	// Clean 之后仍以 .. 开头的，是真正走出仓库的那一类："..""../x" 都是
	// 自己的规范形式，前一条判断放行它们。
	if p == ".." || strings.HasPrefix(p, "../") {
		return "", invalidf("%s %q 越过了仓库根", field, p)
	}
	if p == "." {
		return "", invalidf("%s 不能为空", field)
	}
	return p, nil
}

// DriftResult 是一次漂移检测的结论，封闭枚举。
//
// 漂移的定义是「**我们写进去的那条路径下的内容**，与我们写的时候不一样了」，
// 不是「分支往前走了」（design doc 2026-08-18-drift-detection §2）：别人在别的
// 路径上提交是常态，把它报成漂移会让这个信号每天都在响 —— 而一个每天都响的
// 信号会被整体忽略。
type DriftResult string

const (
	// DriftInSync 表示那条路径下的内容与我们写的时候一致。
	DriftInSync DriftResult = "IN_SYNC"
	// DriftDrifted 表示内容变了。
	DriftDrifted DriftResult = "DRIFTED"
	// DriftNeverWritten 表示这个绑定从来没被推送过，没有可比对的锚点。
	DriftNeverWritten DriftResult = "NEVER_WRITTEN"
	// DriftAnchorMissing 表示锚点那个提交在仓库里找不到了。
	//
	// 与 DriftDrifted 分开：这说明有人改写过历史（force push / rebase），
	// 那条历史里我们那次提交连同它的审计线索一起没了 —— 处置是去查谁改写了
	// 历史，不是重推一次。
	DriftAnchorMissing DriftResult = "ANCHOR_MISSING"
	// DriftUnknown 表示够不到仓库、认证失败、或读不出那棵树。
	//
	// **绝不能塌成 DriftInSync。** 一次网络抖动读成"一致"，操作者就以为
	// 下发的东西还在，而它可能早被人删了（安全规范 §49）。
	DriftUnknown DriftResult = "UNKNOWN"
)

// Valid 判断该结论是否已登记。
//
// 用显式 switch 而非查表：新增一个常量却忘了加进这里，switch 让这处遗漏在
// review 时是看得见的一行。
func (d DriftResult) Valid() bool {
	switch d {
	case DriftInSync, DriftDrifted, DriftNeverWritten, DriftAnchorMissing, DriftUnknown:
		return true
	default:
		return false
	}
}

// validateConfirmedDeletions 判定每一条确认删除都出自本计划、且那一类允许被删。
//
// 判据取自平台这一刻重算出的 Deletions，不取请求：一条能自带"这个文件可以删"
// 的请求，等于让调用方自己授权一次删除。同一条纪律让 Counts 也由平台重算
// （2026-08-14 design doc §4）。
func validateConfirmedDeletions(root string, p WritebackPlan) error {
	offered := make(map[string]DeletionClass, len(p.Deletions))
	for _, d := range p.Deletions {
		offered[d.Path] = d.Class
	}
	seen := make(map[string]struct{}, len(p.Confirmed))
	for i, path := range p.Confirmed {
		if err := checkWithinPolicyPath(root, path); err != nil {
			return err
		}
		if _, dup := seen[path]; dup {
			return invalidf("confirmed[%d] 路径 %q 重复", i, path)
		}
		seen[path] = struct{}{}
		class, ok := offered[path]
		if !ok {
			return invalidf("confirmed[%d] %q 不在本次计划的可处置清单里", i, path)
		}
		if !class.confirmable() {
			return invalidf("confirmed[%d] %q 属于 %s，平台不提供删除", i, path, class)
		}
	}
	return nil
}
