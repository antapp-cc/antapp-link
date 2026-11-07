package server

import (
	"strings"
	"testing"
)

// iptables -S 的真实输出会让默认值省略、把 -m 模块插到规则中间、给 comment 加引号。
// Down 的删除匹配完全依赖这里的字段切分，所以用固定样本在跨平台环境里测。
func TestParseNetfilterRules(t *testing.T) {
	raw := strings.Join([]string{
		`-A INPUT -p tcp --dport 31400:31409 -m comment --comment "AntAppLink" -j ACCEPT`,
		`-A INPUT -p tcp --dport 31400:31409 -m comment --comment "other tool" -j ACCEPT`,
		`-A INPUT -p tcp --dport 22 -j ACCEPT`,
		`-P INPUT ACCEPT`,
		``,
	}, "\n")

	rules := parseNetfilterRules(raw, "filter", "INPUT")
	if len(rules) != 3 {
		t.Fatalf("解析出 %d 条规则，期望 3: %+v", len(rules), rules)
	}
	if !ruleHasMarker(rules[0]) {
		t.Fatalf("带 AntAppLink 标记的规则没认出来: %+v", rules[0])
	}
	if ruleHasMarker(rules[1]) || ruleHasMarker(rules[2]) {
		t.Fatal("把别人的规则误认成自己的")
	}
	// 多词注释必须是一个参数，不能被 strings.Fields 切碎。
	if !hasArgPair(rules[1].Args, "--comment", "other tool") {
		t.Fatalf("多词注释被切碎: %+v", rules[1].Args)
	}
}

func TestSameRuleMatchesLedgerAgainstListing(t *testing.T) {
	ledger := Rule{Table: "nat", Chain: "POSTROUTING", Args: []string{
		"-s", "0.0.0.0/0", "-o", "eth0", "-j", "MASQUERADE",
		"-m", "comment", "--comment", "AntAppLink",
	}}
	raw := `-A POSTROUTING -s 0.0.0.0/0 -o eth0 -j MASQUERADE -m comment --comment "AntAppLink"`
	actual := parseNetfilterRules(raw, "nat", "POSTROUTING")
	if len(actual) != 1 {
		t.Fatalf("解析出 %d 条", len(actual))
	}
	if !sameRule(actual[0], ledger, false) {
		t.Fatalf("账本与实际规则对不上:\n实际 %+v\n账本 %+v", actual[0], ledger)
	}
	// 网卡名变了（升级/换网卡）时，anyWAN 比较仍要能对上。
	other := Rule{Table: "nat", Chain: "POSTROUTING", Args: []string{
		"-s", "0.0.0.0/0", "-o", "eth1", "-j", "MASQUERADE",
		"-m", "comment", "--comment", "AntAppLink",
	}}
	if sameRule(actual[0], other, false) {
		t.Fatal("不同 -o 不应在严格比较下相等")
	}
	if !sameRule(actual[0], other, true) {
		t.Fatal("anyWAN 比较应忽略 -o")
	}
}

func TestRuleHasMarkerIgnoresForeignComment(t *testing.T) {
	r := Rule{Chain: "INPUT", Args: []string{"-m", "comment", "--comment", "AntAppLink2", "-j", "ACCEPT"}}
	if ruleHasMarker(r) {
		t.Fatal("前缀相同的注释被误认成标记")
	}
}

func hasArgPair(args []string, key, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == key && args[i+1] == value {
			return true
		}
	}
	return false
}
