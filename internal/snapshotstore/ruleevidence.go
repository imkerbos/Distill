package snapshotstore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// maxRuleEvidenceRows 是一次记账允许写入的规则条数上限。
//
// 超过即整次拒绝，不截断：一份被截掉一半的证据会让某些规则永远显示成
// "刚观察到"，而它们其实已经被看了很久 —— 朝着"证据看起来更弱"的方向错，
// 虽然安全，但会让操作者永远等不到那条规则变得可信。
const maxRuleEvidenceRows = 20000

// RuleEvidence 是一条候选规则背后的证据积累（design doc 2026-08-25 §4）。
type RuleEvidence struct {
	Fingerprint string    `json:"fingerprint"`
	Namespace   string    `json:"namespace"`
	Workload    string    `json:"workload"`
	FirstSeen   time.Time `json:"firstSeen"`
	LastSeen    time.Time `json:"lastSeen"`
	// Windows 是这条规则出现过的窗口数。跨越的窗口越多，它越不像一次偶然。
	Windows int `json:"windows"`
	// CompleteWindows 是其中完整度为 COMPLETE 的窗口数。
	//
	// 二十个"证明不了看全"的窗口说明的是"看了很多次"，不是"看全了"。
	// 两个数必须同时给出，否则前者会被读成后者。
	CompleteWindows int `json:"completeWindows"`
	// Observations 是累计观测到的连接次数。
	//
	// 与 Windows 分开：一个窗口里刷了十万次的规则，与十个窗口里各出现一次
	// 的规则，可靠性不是一回事，而一个合并出来的"证据分"会把两者混成一个数。
	Observations int64 `json:"observations"`
}

// EvidenceKey 是证据在返回 map 里的键：主体 + 规则指纹。
//
// **不能只用指纹。** 指纹只覆盖规则内容（policygen.FingerprintOf），
// 不含主体 —— "egress 到 kube-dns:53" 在每个 workload 上都是同一个指纹。
// 键的形状与 rule_override 的主键一致，两处对同一条规则的称呼必须相同，
// 否则人工确认与证据会指向不同的东西。
//
// 用 "/" 分隔：namespace 是 DNS 名、workload 取自标签值，两者都不含 "/"，
// 因此不会歧义。这个键会原样出现在 API 的 JSON 对象键上，可读性因此有价值 ——
// 一个含 NUL 的键在浏览器 devtools 里根本看不出内容。
func EvidenceKey(namespace, workload, fingerprint string) string {
	return namespace + "/" + workload + "/" + fingerprint
}

// RecordRuleEvidence 把一个观测窗口里出现过的规则记进证据表。
//
// **首末观测取窗口边界，不取记录时刻**：窗口才是"我们在看"的那段时间。
// 用记录时刻会把一次补采（回看很久以前的窗口）说成一次新观测，于是
// "这条规则观察了多久"被算长了 —— 朝着让人放心的方向错。
//
// 幂等靠 ON DUPLICATE KEY：同一个窗口被重跑时 windows 会多加一次，这是
// 已知的粗糙之处；它让证据显得比实际强一点点，因此**不能**用来放宽任何
// 门禁 —— 覆盖度是给人看的参考，不是判据（判据是学习期与一致率那两道）。
func (s *Store) RecordRuleEvidence(
	ctx context.Context, clusterID string, from, to time.Time,
	complete bool, rules []RuleEvidence,
) (err error) {
	if clusterID == "" {
		return fmt.Errorf("snapshotstore: rule evidence needs a cluster")
	}
	if len(rules) > maxRuleEvidenceRows {
		return fmt.Errorf(
			"snapshotstore: cluster %s carries %d candidate rules, over the %d limit; "+
				"refusing rather than recording part of them",
			clusterID, len(rules), maxRuleEvidenceRows)
	}
	if len(rules) == 0 {
		return nil
	}

	// 完整度是**整个窗口**的性质，不是单条规则的：一次摄入要么证明得了
	// 自己没漏，要么证明不了，而这与哪条规则出现过无关。
	completeCount := 0
	if complete {
		completeCount = 1
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("snapshotstore: begin rule evidence: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	for _, r := range rules {
		if _, err = tx.ExecContext(ctx,
			`INSERT INTO rule_evidence
			   (cluster_id, fingerprint, namespace, workload,
			    first_seen, last_seen, windows, complete_windows, observations)
			 VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)
			 ON DUPLICATE KEY UPDATE
			   first_seen       = LEAST(first_seen, VALUES(first_seen)),
			   last_seen        = GREATEST(last_seen, VALUES(last_seen)),
			   windows          = windows + 1,
			   complete_windows = complete_windows + VALUES(complete_windows),
			   observations     = observations + VALUES(observations)`,
			clusterID, r.Fingerprint, r.Namespace, r.Workload,
			from.UTC(), to.UTC(), completeCount, r.Observations,
		); err != nil {
			return fmt.Errorf("snapshotstore: upsert rule evidence: %w", err)
		}
	}
	return tx.Commit()
}

// LastRuleEvidenceWindowEnd 报告这个集群的证据记到了哪个窗口末端。
//
// 第二个返回值为 false 表示这个集群一条证据都没有 —— 与"记到了零值时刻"
// 必须分得开：零值早于任何真实窗口，塌成一个会让第一个窗口被当成"已经记过"
// 而永远跳过，于是这个集群的证据永远停在 0。
//
// 取 MAX(last_seen) 而不是另立一张记账进度表：last_seen 的写法就是
// `GREATEST(last_seen, VALUES(last_seen))`（见 RecordRuleEvidence），它的最大值
// 天然等于最后记进去的那个窗口末端。多一张表就多一个可以与证据本身分歧的
// 位置，而两者分歧时没有任何东西说得出该信哪一个。
func (s *Store) LastRuleEvidenceWindowEnd(
	ctx context.Context, clusterID string,
) (time.Time, bool, error) {
	var last sql.NullTime
	if err := s.db.QueryRowContext(ctx,
		`SELECT MAX(last_seen) FROM rule_evidence WHERE cluster_id = ?`,
		clusterID).Scan(&last); err != nil {
		return time.Time{}, false, fmt.Errorf(
			"snapshotstore: read the furthest accounted evidence window of cluster %s: %w",
			clusterID, err)
	}
	if !last.Valid {
		return time.Time{}, false, nil
	}
	return last.Time.UTC(), true, nil
}

// EvidenceRef 点名一条候选规则：主体加规则指纹。
//
// 三个字段一起才是键。指纹只覆盖规则内容（policygen.FingerprintOf），
// 「egress 到 kube-dns:53」在每个 workload 上都是同一个指纹 —— 只按指纹取，
// 一条规则会把集群里所有 workload 的同名证据都拖回来。
type EvidenceRef struct {
	Namespace   string
	Workload    string
	Fingerprint string
}

// evidenceReadChunk 是一条查询里最多点名多少条规则。
//
// 分批不是为了绕过 SQL 的参数上限（离得很远），是为了让单条语句的大小与
// 候选集规模脱钩：一个 workload 上万条规则的集群不该产出一条几 MB 的 SQL。
const evidenceReadChunk = 500

// RuleEvidenceOf 读出**这一批候选规则**的证据，按 EvidenceKey 索引。
//
// **按候选取，不是取整个集群。** 这张表只供展示、不解锁任何门禁
// （design doc 2026-08-25 §4），因此它绝不该有能力让策略预览失败 —— 而
// 此前它有：行数过上限就整个报错，而调用方正是 PolicyPreview。更糟的是
// 记账本身要先算一次预览，于是表一旦过线，预览失败 → 记不了账 → 没有任何
// 东西会让表变小，那个集群的策略页永久打不开，只能手工进库救。
//
// 按候选取之后，返回行数被候选数钉住，与表有多大无关；顺带也不再把预览
// 根本不会显示的证据拖进内存。
//
// 一次全取而不是按指纹逐条查：候选集一屏就是几十上百条规则，逐条查会把
// 一次预览变成上百次往返（规范 §24）。
func (s *Store) RuleEvidenceOf(
	ctx context.Context, clusterID string, refs []EvidenceRef,
) (map[string]RuleEvidence, error) {
	out := map[string]RuleEvidence{}
	if len(refs) == 0 {
		return out, nil
	}
	for start := 0; start < len(refs); start += evidenceReadChunk {
		end := min(start+evidenceReadChunk, len(refs))
		if err := s.readEvidenceChunk(ctx, clusterID, refs[start:end], out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// readEvidenceChunk 取一批规则的证据，结果并进 out。
func (s *Store) readEvidenceChunk(
	ctx context.Context, clusterID string, refs []EvidenceRef, out map[string]RuleEvidence,
) error {
	var (
		placeholders strings.Builder
		args         = make([]any, 0, 1+3*len(refs))
	)
	args = append(args, clusterID)
	for i, ref := range refs {
		if i > 0 {
			placeholders.WriteString(",")
		}
		placeholders.WriteString("(?,?,?)")
		args = append(args, ref.Namespace, ref.Workload, ref.Fingerprint)
	}
	// 拼的只有占位符，取值一概走参数。
	//nolint:gosec // placeholders 只由 "(?,?,?)" 拼成，不含任何调用方数据
	query := `SELECT fingerprint, namespace, workload, first_seen, last_seen,
	                 windows, complete_windows, observations
	            FROM rule_evidence
	           WHERE cluster_id = ?
	             AND (namespace, workload, fingerprint) IN (` + placeholders.String() + `)`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("snapshotstore: read rule evidence: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var e RuleEvidence
		if err := rows.Scan(&e.Fingerprint, &e.Namespace, &e.Workload,
			&e.FirstSeen, &e.LastSeen, &e.Windows, &e.CompleteWindows,
			&e.Observations); err != nil {
			return fmt.Errorf("snapshotstore: scan rule evidence: %w", err)
		}
		out[EvidenceKey(e.Namespace, e.Workload, e.Fingerprint)] = e
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("snapshotstore: iterate rule evidence: %w", err)
	}
	return nil
}
