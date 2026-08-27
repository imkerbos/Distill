package collectstore

import (
	"context"
	"fmt"

	"sigs.k8s.io/yaml"

	npav1 "sigs.k8s.io/network-policy-api/apis/v1alpha1"
)

// observedAdminPolicy 是一条落库的管理面策略：种类、名字、原文。
type observedAdminPolicy struct {
	kind     string
	name     string
	manifest string
}

// readAdminPoliciesAt 读出锚点那一次采集看到的 ANP 与 BANP 原文。
//
// 与 readPoliciesAt 同一取法：按 observed_at 取那一刻的那一份，不取"现在"的。
// 拿当前策略解释历史窗口，会让一条上周还不存在的 Deny 参与上周的判定
// （CLAUDE.md §4）。
func (r *Reader) readAdminPoliciesAt(ctx context.Context, d described) ([]observedAdminPolicy, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT policy_kind, name, manifest
		   FROM observed_admin_policy
		  WHERE cluster_id = ? AND observed_at = ?
		  ORDER BY policy_kind, name
		  LIMIT ?`,
		d.clusterID, d.anchor, maxSnapshotRows+1)
	if err != nil {
		return nil, fmt.Errorf("collectstore: read observed admin policies: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []observedAdminPolicy
	for rows.Next() {
		var p observedAdminPolicy
		if err := rows.Scan(&p.kind, &p.name, &p.manifest); err != nil {
			return nil, fmt.Errorf("collectstore: scan observed admin policy: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("collectstore: iterate observed admin policies: %w", err)
	}
	if len(out) > maxSnapshotRows {
		return nil, fmt.Errorf(
			"collectstore: cluster %s observed more than %d admin policies at %s",
			d.clusterID, maxSnapshotRows, d.anchor.UTC().Format("2006-01-02T15:04:05Z07:00"))
	}
	return out, nil
}

// parseAdminPolicies 把原文解析成求值层要的对象。
//
// **严格解码：认不出的字段一律报错，不静默丢弃。** 这一族带 Deny 且排在
// 标准 NetworkPolicy 之前，而 encoding/json 的默认行为是把不认识的字段
// 悄悄扔掉 —— 扔掉的可能正是那个把规则收窄的条件，于是一条本该只拦某个
// 端口的 Deny 被读成拦一切，或者反过来。平台宁可整体报错，让判定变成
// UNKNOWN，也不拿一份被删改过的策略去回答。
//
// 解析不了的原文整体报错、**不跳过**，与 parsePolicies 同一理由，且更硬：
// 跳过一条读不懂的 ANP，它的 Deny 就此消失，那条连接会被判成放行。
//
// 返回的 BANP 是集群级单例；库里出现多条说明采到的东西不是我们以为的形状，
// 同样报错。
func parseAdminPolicies(
	policies []observedAdminPolicy,
) ([]npav1.AdminNetworkPolicy, *npav1.BaselineAdminNetworkPolicy, error) {
	var (
		anps []npav1.AdminNetworkPolicy
		banp *npav1.BaselineAdminNetworkPolicy
	)
	for _, p := range policies {
		switch p.kind {
		case adminPolicyKindAdmin:
			var a npav1.AdminNetworkPolicy
			if err := yaml.UnmarshalStrict([]byte(p.manifest), &a); err != nil {
				return nil, nil, fmt.Errorf(
					"collectstore: stored AdminNetworkPolicy %s cannot be parsed: %w", p.name, err)
			}
			// 名字以库里那一列为准，与 parsePolicies 对 namespace 的处理同理：
			// 那是采集当时看到的事实，manifest 只是原文证据。
			a.Name = p.name
			anps = append(anps, a)
		case adminPolicyKindBaseline:
			if banp != nil {
				return nil, nil, fmt.Errorf(
					"collectstore: more than one BaselineAdminNetworkPolicy stored; it is a cluster-level singleton")
			}
			var b npav1.BaselineAdminNetworkPolicy
			if err := yaml.UnmarshalStrict([]byte(p.manifest), &b); err != nil {
				return nil, nil, fmt.Errorf(
					"collectstore: stored BaselineAdminNetworkPolicy %s cannot be parsed: %w", p.name, err)
			}
			b.Name = p.name
			banp = &b
		default:
			// 认不出的种类不能跳过：库里出现它说明写入侧长出了新东西，
			// 而这一层还不知道该怎么解释它。
			return nil, nil, fmt.Errorf(
				"collectstore: stored admin policy %s has an unrecognised kind %q", p.name, p.kind)
		}
	}
	return anps, banp, nil
}

// 两个种类取值与 snapshot.AdminPolicyKind 对齐。
//
// 这里写字面量而不是 import snapshot：collectstore 是读侧，snapshot 是写侧，
// 让读侧依赖写侧的类型会把两条链绑在一起。取值漂了由集成测试抓 —— 它读的
// 是真库里那一列。
const (
	adminPolicyKindAdmin    = "ADMIN_NETWORK_POLICY"
	adminPolicyKindBaseline = "BASELINE_ADMIN_NETWORK_POLICY"
)
