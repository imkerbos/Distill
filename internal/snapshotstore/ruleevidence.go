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
	//
	// **uint64，与列的 BIGINT UNSIGNED 一致。** 用 int64 扫，一个超过 2^63
	// 的值不会读成一个错的数字——它让 Scan 整个失败，而这条读取路径挂了
	// 就等于预览挂了、记账跟着挂了。2026-08-29 实测：一次计数缺陷把这一列
	// 推过 2^63 之后，整条链停了 13 小时，而界面上唯一的症状是数字不再更新。
	Observations uint64 `json:"observations"`
	// Body 是规则体的持久化形状（policygen.MarshalRule）。
	//
	// 存它是为了让**跨窗口的规则集**取得回来：在此之前这张表只有指纹，
	// 而指纹是单向的，于是平台学了一天、导出时只拿得到最后一个窗口里跑过
	// 的那些（design doc 2026-08-29 §1）。
	//
	// 空表示调用方没给。落库时写 NULL，与"存了一个空规则"分得开——后者
	// 渲染出来是一条谁都不放行的策略。
	Body []byte `json:"-"`
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
			    first_seen, last_seen, windows, complete_windows, observations, rule_body)
			 VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, ?)
			 ON DUPLICATE KEY UPDATE
			   first_seen       = LEAST(first_seen, VALUES(first_seen)),
			   last_seen        = GREATEST(last_seen, VALUES(last_seen)),
			   windows          = windows + 1,
			   complete_windows = complete_windows + VALUES(complete_windows),
			   observations     = observations + VALUES(observations),
			   -- 规则体跟着覆盖：同一个指纹的规则体按构造必然相同，因此
			   -- 覆盖是无害的；而它顺手把本次迁移之前那些没有规则体的旧行
			   -- 补上——那些行只要再被观测到一次就恢复成能贡献规则的行。
			   rule_body        = COALESCE(VALUES(rule_body), rule_body)`,
			clusterID, r.Fingerprint, r.Namespace, r.Workload,
			from.UTC(), to.UTC(), completeCount, r.Observations, nullBody(r.Body),
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

// nullBody 把空规则体落成 NULL，而不是一个空 JSON。
//
// NULL 是"没有规则体"，'{}' 是"有一个内容为空的规则"——后者取回来会渲染成
// 一条谁都不放行的策略，朝切断的方向错。
func nullBody(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

// LearnedRule 是一条从证据表取回来的、跨窗口累积的规则。
type LearnedRule struct {
	Namespace   string
	Workload    string
	Fingerprint string
	// LastSeen 是最后一次观测到它的窗口末端。
	//
	// **必须一路带到界面与导出注释头。** 一条两天没见过的规则留在策略里，
	// 是一个要给人看的信号，不是一个可以藏起来的事实（design doc §3.2）。
	LastSeen time.Time
	// Observations 是累计观测次数，用来替代单窗口的 FlowCount。
	//
	// **uint64，不是 int64**：列是 BIGINT UNSIGNED。用 int64 扫，一个超过
	// 2^63 的值会让 Scan 直接失败，而失败会连带把整次预览打挂——这正是
	// 2026-08-29 那次记账停摆的第二个原因（第一个是重复计数把它涨到那么大）。
	Observations uint64
	// Body 是规则体，交给 policygen.UnmarshalRule 还原。
	Body []byte
}

// maxLearnedRules 是一次取回的规则条数上限。
//
// 超出即报错，**不截断**：截断会让策略集读起来比实际小，而"比实际小"意味着
// 少几条放行——正是应用之后造成阻断的那个方向（同 maxWindowConnections）。
const maxLearnedRules = 50_000

// LearnedRulesSince 取回 since 之后还被观测到过的规则。
//
// 这是"规则集不再被单个观测窗口限死"的取数一端：窗口决定 dry-run 算在哪段
// 流量上，这里决定策略集里有哪些规则。两者分开之后，观测多久就能学到多久
// （design doc 2026-08-29 §3.2）。
//
// **rule_body 为 NULL 的行不返回。** 那些是本次迁移之前记下的，只有计数、
// 没有规则体；把它们当成规则会渲染出一条空策略。它们仍然贡献 windows /
// observations 那几个计数，只要再被观测到一次就会被补上规则体。
func (s *Store) LearnedRulesSince(
	ctx context.Context, clusterID string, since time.Time,
) ([]LearnedRule, error) {
	if clusterID == "" {
		return nil, fmt.Errorf("snapshotstore: learned rules need a cluster")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT namespace, workload, fingerprint, last_seen, observations, rule_body
		   FROM rule_evidence
		  WHERE cluster_id = ? AND last_seen >= ? AND rule_body IS NOT NULL
		  ORDER BY namespace, workload, fingerprint
		  LIMIT ?`,
		clusterID, since.UTC(), maxLearnedRules+1)
	if err != nil {
		return nil, fmt.Errorf("snapshotstore: read learned rules: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []LearnedRule
	for rows.Next() {
		var r LearnedRule
		if err := rows.Scan(&r.Namespace, &r.Workload, &r.Fingerprint,
			&r.LastSeen, &r.Observations, &r.Body); err != nil {
			return nil, fmt.Errorf("snapshotstore: scan learned rule: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("snapshotstore: iterate learned rules: %w", err)
	}
	if len(out) > maxLearnedRules {
		return nil, fmt.Errorf(
			"snapshotstore: cluster %s has more than %d learned rules since %s; shorten the retention",
			clusterID, maxLearnedRules, since.UTC().Format(time.RFC3339))
	}
	return out, nil
}
