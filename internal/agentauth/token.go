// Package agentauth 生成与校验集群 agent 的访问 token。
//
// **纯逻辑，零 I/O**：不读库、不读文件、不打日志、不依赖任何框架。这一层
// 决定一次推送算哪个集群的数据，而算错的后果是身份表被写进别的集群，
// 之后 join 落到错误的 Pod 上且不报错（CLAUDE.md §4）。正确性只能靠测试
// 保证，不能靠调用它的那一层保证。
package agentauth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"strings"
)

// Prefix 是所有 agent token 的固定前缀。
//
// 存在的理由不是解析（解析靠分段），是**可扫描性**：一把不慎落进日志、
// 落进仓库、落进工单的 token，前缀是唯一能让扫描器认出「这是一把凭据」
// 的线索。
const Prefix = "dstl_"

// agentIDBytes 是公开段的字节数，hex 编码后 16 个字符
// （与 registry.AgentIDLen 对齐）。
//
// 它不是秘密，只用来定位记录，所以不需要长；但它是唯一索引的键，
// 碰撞会让签发失败 —— 而不是认错人，那由秘密段兜底。
const agentIDBytes = 8

// secretBytes 是秘密段的字节数。
//
// 256 bit。这个长度让暴力破解在任何现实算力下都不成立，因此本包不需要、
// 也刻意不使用任何慢哈希（design doc 2026-08-18 §3.2）：慢哈希买的是
// 低熵口令的破解成本，而这里没有低熵可言，装上它只会给每一次摄入请求
// 加一次固定 CPU 开销。
const secretBytes = 32

// Issue 生成一把新 token，并返回它的公开段与应当入库的哈希。
//
// 明文 token 只在这一次返回里出现：调用方把它交给操作者之后，平台就只
// 剩哈希（规范 §19、§33）。丢了只能重签一把、把旧的吊销 —— 找回不可能，
// 这正是期望性质。
//
// 随机数取不到时直接失败，不退化成任何别的来源：一把熵不足的 token
// 与一把正常 token 长得一模一样（规范 §32）。
func Issue() (token, agentID string, hash []byte, err error) {
	idRaw := make([]byte, agentIDBytes)
	if _, err := rand.Read(idRaw); err != nil {
		return "", "", nil, err
	}
	secret := make([]byte, secretBytes)
	if _, err := rand.Read(secret); err != nil {
		return "", "", nil, err
	}

	agentID = hex.EncodeToString(idRaw)
	token = Prefix + agentID + separator + base64.RawURLEncoding.EncodeToString(secret)
	sum := sha256.Sum256([]byte(token))
	return token, agentID, sum[:], nil
}

// separator 分开公开段与秘密段。
//
// 单独取名而不是就地写字面量：签发与解析各写一次，改一处漏一处的症状
// 是「新签发的 token 一律认不过」，而那时两边都看起来没错。
const separator = "_"

// Parse 取出 token 里的公开段。形状不对时第二个返回值为 false。
//
// **只做形状检查，不做任何比对。** 调用方拿这个 agentID 去查记录，再由
// Matches 判定这把 token 是不是那条记录的。两步分开，是为了让「查不到
// 记录」与「token 不对」在调用方那里可以走同一个响应，而在日志里分得开
// （规范 §22）——合进一步就再也分不开了。
func Parse(token string) (agentID string, ok bool) {
	rest, found := strings.CutPrefix(token, Prefix)
	if !found {
		return "", false
	}
	id, secret, found := strings.Cut(rest, separator)
	if !found || secret == "" || len(id) != agentIDBytes*2 {
		return "", false
	}
	if _, err := hex.DecodeString(id); err != nil {
		return "", false
	}
	return id, true
}

// Matches 判断 token 是否与入库的哈希对应。
//
// **常数时间比对**（补充规范 §35）：普通的 bytes.Equal 会按字节提前返回，
// 让攻击者用响应时间一位一位试出哈希。
//
// **`token == ""` 这一半是有行为的**：空串的摘要是一个固定值，写入侧只要
// 出过一次把它落库的 bug，一个根本没带 Authorization 头的请求就会认证
// 通过。token_test.go 里有一条用例专门钉住它。
//
// **`len(hash) != sha256.Size` 这一半没有测试覆盖，而且覆盖不了**：
// ConstantTimeCompare 在长度不等时本来就返回 0，所以删掉它，Matches 对外
// 行为一模一样 —— 黑盒测试无法通过删掉这行来验证它（同 internal/secrets
// 的 pathWithinDir，理由也同一条）。它防的是将来有人把下面那行换成
// bytes.Equal 或某种长度无关的比较。**改这个函数时不要顺手删掉它**，
// 它的正确性只能靠 review 时肉眼确认。
func Matches(token string, hash []byte) bool {
	if token == "" || len(hash) != sha256.Size {
		return false
	}
	sum := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(sum[:], hash) == 1
}
