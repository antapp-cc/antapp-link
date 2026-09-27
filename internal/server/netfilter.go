package server

import (
	"fmt"
	"net"
	"strings"
)

// ChainName 是自定义链名。用它而不是往 PREROUTING 里塞散装规则，
// 是为了 down 时能整体清干净，不留残渣。
const ChainName = "ANTAPP_LINK"

// Rule 是一条 iptables 规则。Table 为空表示 filter 表。
type Rule struct {
	Table string
	Chain string
	Args  []string
}

func (r Rule) String() string {
	parts := []string{"iptables", "-t", TableOrDefault(r.Table), "-A", r.Chain}
	parts = append(parts, r.Args...)
	return strings.Join(parts, " ")
}

func TableOrDefault(t string) string {
	if t == "" {
		return "filter"
	}
	return t
}

// Rules 产出全部规则。纯函数，不执行任何命令 —— 这样规则内容在任何平台上都能被单测，
// 而云服上真正生效的定义与测试断言的永远是同一份。
func Rules(cfg Config, wanIface string) []Rule {
	network := cfg.Tunnel.Network
	client := cfg.Tunnel.ClientIP
	portRange := fmt.Sprintf("%d:%d", cfg.ForwardPorts.Start, cfg.ForwardPorts.End)
	_, listenPort, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		listenPort = "62233"
	}

	return []Rule{
		// 客户端借云服的干净出口出网
		{Table: "nat", Chain: "POSTROUTING", Args: []string{"-s", network, "-o", wanIface, "-j", "MASQUERADE"}},
		{Chain: "FORWARD", Args: []string{"-s", network, "-j", "ACCEPT"}},
		{Chain: "FORWARD", Args: []string{"-d", network, "-j", "ACCEPT"}},

		// 内层 MTU 只有 1400，不 clamp 就会出现「小包通、网页打不开」的黑洞
		{Table: "mangle", Chain: "FORWARD", Args: []string{
			"-p", "tcp", "--tcp-flags", "SYN,RST", "SYN", "-j", "TCPMSS", "--clamp-mss-to-pmtu"}},

		{Chain: "INPUT", Args: []string{"-p", "tcp", "--dport", listenPort, "-j", "ACCEPT"}},

		// 端口转发交给内核，只做 TCP。
		//
		// 曾经 TCP 和 UDP 各一条，后来按需求去掉了 UDP：Pi Node 那边只用 TCP，
		// 多开一条 UDP 规则等于平白多一个对外暴露的面。
		{Table: "nat", Chain: ChainName, Args: []string{
			"-p", "tcp", "--dport", portRange, "-j", "DNAT", "--to-destination", client}},
		{Table: "nat", Chain: "PREROUTING", Args: []string{"-p", "tcp", "--dport", portRange, "-j", ChainName}},
	}
}

// DNATRules 单独取出来，方便日志和 status 只显示与端口转发相关的部分。
func DNATRules(cfg Config) []Rule {
	var out []Rule
	for _, r := range Rules(cfg, "") {
		if r.Chain == ChainName {
			out = append(out, r)
		}
	}
	return out
}
