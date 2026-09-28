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

// 端口转发是服务端应答式转发器（本机监听 + 经隧道转给节点机），
// 不再使用内核 DNAT —— DNAT 的握手必须走到节点机才完成，外部检查器
// 的延迟会把整条隧道往返算进去（实测 400ms，翻倍）。
func TestRulesNoDNAT(t *testing.T) {
	for _, s := range ruleStrings(Default(), "eth0") {
		if strings.Contains(s, "DNAT") || strings.Contains(s, ChainName) {
			t.Errorf("不应再有任何 DNAT / 自定义链规则：%s", s)
		}
	}
}

// 转发端口段要对公网放行：服务端转发器在本机监听这些端口并立即应答。
func TestRulesOpenForwardPorts(t *testing.T) {
	mustContain(t, ruleStrings(Default(), "eth0"), "-A INPUT -p tcp --dport 31400:31409 -j ACCEPT")
}

func TestRulesMasqueradeAndForward(t *testing.T) {
	rules := ruleStrings(Default(), "eth0")
	mustContain(t, rules, "-t nat -A POSTROUTING -s 10.10.0.0/24 -o eth0 -j MASQUERADE")
	mustContain(t, rules, "FORWARD -s 10.10.0.0/24 -j ACCEPT")
	mustContain(t, rules, "FORWARD -d 10.10.0.0/24 -j ACCEPT")
}

func TestRulesClampMSS(t *testing.T) {
	// 内层 MTU 只有 1400，不 clamp 就会出现「小包通、网页打不开」的黑洞
	mustContain(t, ruleStrings(Default(), "eth0"),
		"-t mangle -A FORWARD -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu")
}

func TestRulesOpenTunnelPort(t *testing.T) {
	mustContain(t, ruleStrings(Default(), "eth0"), "-A INPUT -p tcp --dport 62233 -j ACCEPT")
}

// 隧道客户端的 DNS 查询发往网关上的 dnsmasq（filter-AAAA，v4-only 隧道
// 不能返回 AAAA），INPUT 必须放行。
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

	mustContain(t, rules, "-A INPUT -p tcp --dport 31500:31509 -j ACCEPT")
	mustContain(t, rules, "-o ens3 -j MASQUERADE")
	mustContain(t, rules, "-A INPUT -p tcp --dport 443 -j ACCEPT")

	for _, s := range rules {
		if strings.Contains(s, "31400:31409") {
			t.Errorf("规则里不该出现硬编码的默认端口段：%s", s)
		}
	}
}
