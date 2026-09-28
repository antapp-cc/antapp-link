package server

import (
	"fmt"
	"net"
	"strings"
)

// ChainName 是自定义链名。用它而不是往 PREROUTING 里塞散装规则，
// 是为了 down 时能整体清干净，不留残渣。
const ChainName = "ANTAPP_LINK"

// daemonProcessName 是本守护进程的进程名：端口占用检查时用它区分
// 「自家转发器的监听」与「外部程序的占用」。
const daemonProcessName = "antapp-linkd"

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

		// 隧道客户端的 DNS 查询发往网关上的 dnsmasq（filter-AAAA）——
		// v4-only 隧道绝不能返回 AAAA，否则客户端拿 v6 地址直连死路。
		{Chain: "INPUT", Args: []string{"-i", cfg.Tunnel.Device, "-p", "udp", "--dport", "53", "-j", "ACCEPT"}},
		{Chain: "INPUT", Args: []string{"-i", cfg.Tunnel.Device, "-p", "tcp", "--dport", "53", "-j", "ACCEPT"}},

		// 转发端口段对公网开放：服务端转发器（relay）在本机监听这些端口，
		// 收到连接立即应答、再经隧道转给节点机。
		{Chain: "INPUT", Args: []string{"-p", "tcp", "--dport", portRange, "-j", "ACCEPT"}},
	}
}
