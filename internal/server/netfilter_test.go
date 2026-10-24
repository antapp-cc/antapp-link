package server

import (
	"path/filepath"
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
	// 入向只放已有连接的回程：节点机只会被动接受服务端 relay 的拨号
	mustContain(t, rules, "FORWARD -d 10.10.0.0/24 -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT")
}

func TestRulesClampMSS(t *testing.T) {
	// 内层 MTU 只有 1400，不 clamp 就会出现「小包通、网页打不开」的黑洞。
	// 但必须限定来源网段与出口网卡：不限定的 clamp 会波及云服上所有 FORWARD 的
	// TCP SYN（容器、其他转发业务），等于替别人改了 MSS。
	mustContain(t, ruleStrings(Default(), "eth0"),
		"-t mangle -A FORWARD -s 10.10.0.0/24 -o eth0 -p tcp --tcp-flags SYN,RST SYN "+
			"-j TCPMSS --clamp-mss-to-pmtu")

	cfg := Default()
	cfg.Tunnel.Network = "10.20.0.0/24"
	mustContain(t, ruleStrings(cfg, "ens3"),
		"-t mangle -A FORWARD -s 10.20.0.0/24 -o ens3 -p tcp --tcp-flags SYN,RST SYN "+
			"-j TCPMSS --clamp-mss-to-pmtu")
}

// 只加新规则不够：升级机器上旧版那条无限定的 clamp 还排在前面，限定等于没生效。
// Up 会按 legacyRules 逐条 -D 清掉旧形态，所以它必须在这里。
func TestLegacyRulesRemoveUnscopedClamp(t *testing.T) {
	want := "mangle FORWARD -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu"
	for _, r := range legacyRules(Default()) {
		joined := strings.Join(append([]string{r.Table, r.Chain}, r.Args...), " ")
		if joined == want {
			return
		}
	}
	t.Fatal("legacyRules 里没有旧的无限定 clamp 规则，升级机器上它会被留下来继续 clamp")
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

// mode=udp 时数据包走 UDP，端口与控制连接相同。缺了这条 INPUT 放行，客户端的
// 认证探测包在服务端就被丢掉，UDP 模式永远连不上（客户端又不会偷偷回退 TCP）。
func TestRulesOpenUDPTunnelPort(t *testing.T) {
	cfg := Default()
	cfg.Mode = "udp"
	rules := ruleStrings(cfg, "eth0")

	mustContain(t, rules, "-A INPUT -p udp --dport 62233 -j ACCEPT")
	// 控制连接仍走 TCP：只加规则，不替换
	mustContain(t, rules, "-A INPUT -p tcp --dport 62233 -j ACCEPT")
}

// 用配置里的实际监听端口，不能把默认 62233 写死进规则。
func TestRulesUDPPortFollowsListen(t *testing.T) {
	cfg := Default()
	cfg.Mode = "udp"
	cfg.Listen = "0.0.0.0:45000"
	rules := ruleStrings(cfg, "eth0")

	mustContain(t, rules, "-A INPUT -p udp --dport 45000 -j ACCEPT")
	for _, s := range rules {
		if strings.Contains(s, "-p udp --dport 62233") {
			t.Errorf("udp 规则用了默认端口而不是配置里的监听端口：%s", s)
		}
	}
}

// tcp 模式下服务端并不监听 UDP，别顺带开一个没人管的 UDP 口。
func TestRulesTCPModeHasNoUDPTunnelPort(t *testing.T) {
	for _, s := range ruleStrings(Default(), "eth0") {
		if strings.Contains(s, "-p udp --dport 62233") {
			t.Errorf("tcp 模式下不该有 UDP 隧道端口规则：%s", s)
		}
	}
}

// 删除规则时要能去掉 -o：Up 和 Down 之间网卡可能改名，带旧网卡名的 -D 删不掉
// MASQUERADE / MSS clamp，规则会永久残留。
func TestStripOutIface(t *testing.T) {
	got := stripOutIface([]string{"-s", "10.10.0.0/24", "-o", "eth0", "-j", "MASQUERADE"})
	want := []string{"-s", "10.10.0.0/24", "-j", "MASQUERADE"}
	if len(got) != len(want) {
		t.Fatalf("参数数量不对: %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 个参数应为 %q，实际 %q", i, want[i], got[i])
		}
	}
	// 没有 -o 的参数原样返回
	if got := stripOutIface([]string{"-p", "tcp", "-j", "ACCEPT"}); len(got) != 4 || got[3] != "ACCEPT" {
		t.Fatalf("无 -o 时应原样返回，实际 %v", got)
	}
}

// 换端口段时靠 /run 的记录清旧段：写进去能读回来，删掉后读到空。
func TestForwardRangeRoundTrip(t *testing.T) {
	old := forwardRangePath
	forwardRangePath = filepath.Join(t.TempDir(), "forward-range")
	defer func() { forwardRangePath = old }()

	if got := readForwardRange(); got != "" {
		t.Fatalf("文件不存在时应返回空，实际 %q", got)
	}
	if err := writeForwardRange("31400:31409"); err != nil {
		t.Fatalf("写失败: %v", err)
	}
	if got := readForwardRange(); got != "31400:31409" {
		t.Fatalf("应读回写进去的段，实际 %q", got)
	}
}
