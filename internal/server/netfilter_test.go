package server

import (
	"strings"
	"testing"
)

func ruleStrings(cfg Config, wan string) []string {
	var out []string
	for _, r := range Rules(cfg, wan) {
		out = append(out, r.String())
	}
	return out
}

func mustContain(t *testing.T, rules []string, want string) {
	t.Helper()
	for _, s := range rules {
		if strings.Contains(s, want) {
			return
		}
	}
	t.Errorf("规则里找不到 %q，实际规则：\n%s", want, strings.Join(rules, "\n"))
}

// 只转发 TCP。曾经 TCP 和 UDP 都转，后来按需求去掉了 UDP ——
// Pi Node 只用 TCP，多留一条 UDP 规则等于平白多一个对外暴露的面。
func TestRulesForwardTCPOnly(t *testing.T) {
	rules := ruleStrings(Default(), "eth0")
	mustContain(t, rules, "-p tcp --dport 31400:31409 -j DNAT --to-destination 10.10.0.2")

	for _, s := range rules {
		if strings.Contains(s, "--dport 31400:31409") && strings.Contains(s, "-p udp") {
			t.Errorf("不该再有 UDP 转发规则：%s", s)
		}
	}
}

func TestRulesMasqueradeAndForward(t *testing.T) {
	rules := ruleStrings(Default(), "eth0")
	mustContain(t, rules, "-t nat -A POSTROUTING -s 10.10.0.0/24 -o eth0 -j MASQUERADE")
	mustContain(t, rules, "FORWARD -s 10.10.0.0/24 -j ACCEPT")
	mustContain(t, rules, "FORWARD -d 10.10.0.0/24 -j ACCEPT")
}

func TestRulesClampMSS(t *testing.T) {
	// 内层 MTU 只有 1400，不 clamp 就会「小包通、网页打不开」
	mustContain(t, ruleStrings(Default(), "eth0"),
		"-t mangle -A FORWARD -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu")
}

func TestRulesOpenTunnelPort(t *testing.T) {
	mustContain(t, ruleStrings(Default(), "eth0"), "-A INPUT -p tcp --dport 62233 -j ACCEPT")
}

// 隧道客户端的 DNS 查询发往网关上的 dnsmasq（filter-AAAA，v4-only 隧道
// 不能返回 AAAA，否则客户端拿 v6 直连死路），INPUT 必须放行。
func TestRulesAllowTunnelDNS(t *testing.T) {
	rules := ruleStrings(Default(), "eth0")
	mustContain(t, rules, "-A INPUT -i antapp0 -p udp --dport 53 -j ACCEPT")
	mustContain(t, rules, "-A INPUT -i antapp0 -p tcp --dport 53 -j ACCEPT")
}

func TestRulesFollowConfig(t *testing.T) {
	cfg := Default()
	cfg.Tunnel.ClientIP = "10.10.0.9"
	// 刻意用一段跟默认值不同的端口：这样才验得出「规则跟随配置」，
	// 而不是碰巧等于默认值。断言里再确认默认段没被写死进去。
	cfg.ForwardPorts = PortRange{Start: 31500, End: 31509}
	cfg.Listen = "0.0.0.0:443"
	rules := ruleStrings(cfg, "ens3")

	mustContain(t, rules, "-p tcp --dport 31500:31509 -j DNAT --to-destination 10.10.0.9")
	mustContain(t, rules, "-o ens3 -j MASQUERADE")
	mustContain(t, rules, "-A INPUT -p tcp --dport 443 -j ACCEPT")

	for _, s := range rules {
		if strings.Contains(s, "31400:31409") {
			t.Errorf("规则里不该出现硬编码的默认端口段：%s", s)
		}
	}
}

func TestRulesJumpIntoSingleChain(t *testing.T) {
	// 所有 DNAT 都在自定义链里，PREROUTING 只负责跳转 —— down 时才能整体清干净
	rules := Rules(Default(), "eth0")
	var dnatInside, jumpIn bool
	for _, r := range rules {
		s := r.String()
		if strings.Contains(s, "DNAT") {
			if r.Table != "nat" || r.Chain != ChainName {
				t.Errorf("DNAT 应位于 nat 表的 %s 链，实际 %s", ChainName, s)
			} else {
				dnatInside = true
			}
		}
		if r.Chain == "PREROUTING" && strings.HasSuffix(s, ChainName) {
			jumpIn = true
		}
	}
	if !dnatInside {
		t.Error("没有任何 DNAT 规则")
	}
	if !jumpIn {
		t.Error("PREROUTING 没有跳进自定义链，端口转发不会生效")
	}
}

func TestDNATRulesOnlyReturnsForwarding(t *testing.T) {
	rules := DNATRules(Default())
	if len(rules) != 1 {
		t.Fatalf("只转发 TCP，应该正好一条 DNAT，实际 %d 条", len(rules))
	}
	for _, r := range rules {
		if r.Chain != ChainName {
			t.Errorf("DNATRules 不该返回其它链的规则：%s", r.String())
		}
	}
}
