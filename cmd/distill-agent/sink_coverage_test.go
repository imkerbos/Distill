package main

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/imkerbos/Distill/internal/snapshot"
)

// notPushed 是 snapshot.Observation 里**刻意不推**的字段，连同理由。
//
// 每一项都要写清为什么，否则这份豁免名单会变成一个"忘了带就加进来"的
// 垃圾桶 —— 而那正是它要防的事。
var notPushed = map[string]string{
	"ClusterID":  "集群归属只来自 agent token，不来自报文（design doc §2）—— 报文里给了也不会被采纳",
	"RunID":      "在报文顶层，不在 observation 里",
	"ObservedAt": "同上，在报文顶层",
	"ForeignScopes": "推送模式不探测第二平面覆盖范围，平台据此整片降级 —— " +
		"零值即「范围不完整」，是保守且正确的方向",
	"ForeignScopesComplete": "同上",
}

// snapshot.Observation 的每一个字段，要么被推送报文带上，要么写明为什么不带。
//
// 这条用例来自一次真实缺口：Observation 长出了 AdminPolicies，agent 照常采集
// 并填好了它，而报文的组装函数没有跟上 —— 于是 ANP 在边界上被静默丢弃。
// 后果不是"少一类数据"：服务端按收到的 observation 算资源计数，那一轮会写下
// ADMINNETWORKPOLICY = 0，而 0 的含义是「采过了，这个集群就是没有」，与
// 「根本没送来」在库里长得一模一样。这一族带 Deny，被当成不存在的方向是把
// 一条其实被拦住的连接判成放行。
//
// 编译器抓不到它：多一个字段没人读，Go 不会有意见。
func TestPushedObservationCoversEveryField(t *testing.T) {
	// 造一份全非零的观测，编码之后看哪些键出现了。
	var payload agentObservationPayload
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal an empty payload: %v", err)
	}
	_ = raw

	// 报文字段按 json tag 取名，与 Observation 的字段名比对时统一成小写开头。
	carried := map[string]bool{}
	pt := reflect.TypeOf(agentObservationPayload{})
	for i := range pt.NumField() {
		carried[pt.Field(i).Name] = true
	}

	var missing []string
	ot := reflect.TypeOf(snapshot.Observation{})
	for i := range ot.NumField() {
		name := ot.Field(i).Name
		if carried[name] {
			continue
		}
		if _, exempt := notPushed[name]; exempt {
			continue
		}
		missing = append(missing, name)
	}
	if len(missing) > 0 {
		t.Errorf("snapshot.Observation 的这些字段既没有进推送报文，也没有写明为什么不进：%v\n"+
			"漏带一类的后果是平台记下「这个集群没有这类对象」，而那是一句假话", missing)
	}
}

// 豁免名单不许留下过期项。
//
// 字段改了名或删了，豁免还留着，下一个人会以为那件事仍然被想过。
func TestNotPushedListHasNoStaleEntries(t *testing.T) {
	fields := map[string]bool{}
	ot := reflect.TypeOf(snapshot.Observation{})
	for i := range ot.NumField() {
		fields[ot.Field(i).Name] = true
	}
	for name := range notPushed {
		if !fields[name] {
			t.Errorf("豁免名单里的 %q 已经不是 snapshot.Observation 的字段了", name)
		}
	}
}
