package snapshotstore

import (
	"context"
	"fmt"
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

// RuleEvidenceOf 读出一个集群全部规则的证据，按 EvidenceKey 索引。
//
// 一次全取而不是按指纹逐条查：候选集一屏就是几十上百条规则，逐条查会把
// 一次预览变成上百次往返（规范 §24）。
func (s *Store) RuleEvidenceOf(
	ctx context.Context, clusterID string,
) (map[string]RuleEvidence, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT fingerprint, namespace, workload, first_seen, last_seen,
		        windows, complete_windows, observations
		   FROM rule_evidence WHERE cluster_id = ? LIMIT ?`,
		clusterID, maxRuleEvidenceRows+1)
	if err != nil {
		return nil, fmt.Errorf("snapshotstore: read rule evidence: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]RuleEvidence{}
	for rows.Next() {
		var e RuleEvidence
		if err := rows.Scan(&e.Fingerprint, &e.Namespace, &e.Workload,
			&e.FirstSeen, &e.LastSeen, &e.Windows, &e.CompleteWindows,
			&e.Observations); err != nil {
			return nil, fmt.Errorf("snapshotstore: scan rule evidence: %w", err)
		}
		out[EvidenceKey(e.Namespace, e.Workload, e.Fingerprint)] = e
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("snapshotstore: iterate rule evidence: %w", err)
	}
	if len(out) > maxRuleEvidenceRows {
		return nil, fmt.Errorf(
			"snapshotstore: cluster %s holds more than %d rule evidence rows",
			clusterID, maxRuleEvidenceRows)
	}
	return out, nil
}
