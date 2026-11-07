package server

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/antapp-cc/antapp-link/internal/pki"
)

// ChainName 是自定义链名。用它而不是往 PREROUTING 里塞散装规则，
// 是为了 down 时能整体清干净，不留残渣。
const ChainName = "ANTAPP_LINK"

// daemonProcessName 是本守护进程的进程名：端口占用检查时用它区分
// 「自家转发器的监听」与「外部程序的占用」。
const daemonProcessName = "antapp-linkd"

// forwardRangePath 记录上一次 Up 铺的转发端口段。
//
// 换端口段时（--force / 改配置）只靠 cfg 是不知道旧段规则的：Up 只会 ensureRule 新段，
// Down 也只按新段删，旧段的 INPUT 放行会永久对公网开着。把上次的段记下来，Up 铺新段
// 前先清旧段。放 /run（tmpfs）：机器重启后 iptables 规则本身也没了，记录与现场一致。
var forwardRangePath = "/run/antapp-link/forward-range"

func readForwardRange() string {
	raw, err := os.ReadFile(forwardRangePath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

func writeForwardRange(r string) error {
	if err := os.MkdirAll(filepath.Dir(forwardRangePath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(forwardRangePath, []byte(r+"\n"), 0o644)
}

// stripOutIface 仅用于比较旧版规则形态；删除时必须保留实际规则的全部字段。
func stripOutIface(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "-o" && i+1 < len(args) {
			i++
			continue
		}
		out = append(out, args[i])
	}
	return out
}

func normalizedRuleArgs(args []string) string {
	var out []string
	var comment string
	for i := 0; i < len(args); i++ {
		if args[i] == "-m" && i+1 < len(args) && (args[i+1] == "tcp" || args[i+1] == "udp" || args[i+1] == "comment") {
			i++
			continue
		}
		if args[i] == "--ctstate" && i+1 < len(args) {
			states := strings.Split(args[i+1], ",")
			sort.Strings(states)
			out = append(out, args[i], strings.Join(states, ","))
			i++
			continue
		}
		if args[i] == "--comment" && i+1 < len(args) {
			comment = strings.Trim(args[i+1], "\"")
			i++
			continue
		}
		out = append(out, args[i])
	}
	return strings.Join(out, " ") + "|comment=" + comment
}

func sameRule(a, b Rule, anyWAN bool) bool {
	if TableOrDefault(a.Table) != TableOrDefault(b.Table) || a.Chain != b.Chain {
		return false
	}
	aa, bb := a.Args, b.Args
	if anyWAN {
		aa, bb = stripOutIface(aa), stripOutIface(bb)
	}
	return normalizedRuleArgs(aa) == normalizedRuleArgs(bb)
}

// ruleHasMarker 判断一条规则是否带本项目的注释标记。账本丢失（/run 是 tmpfs，重启
// 就没了）时，Down 只能靠这个标记认出自己铺过的规则。
func ruleHasMarker(r Rule) bool {
	for i := 0; i+1 < len(r.Args); i++ {
		if r.Args[i] == "--comment" && strings.Trim(r.Args[i+1], "\"") == "AntAppLink" {
			return true
		}
	}
	return false
}

// isMissingChainError 判断 iptables 的报错是不是「链不存在」。不同版本的英文措辞
// 不一致（No chain/target/match by that name、does not exist、No such file），
// 统一按关键词判断，避免换个 iptables 实现就误判成清理失败。
func isMissingChainError(msg string) bool {
	m := strings.ToLower(msg)
	for _, s := range []string{"no chain", "does not exist", "not exist", "no such", "不存在"} {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}

// parseNetfilterRules 解析 `iptables -S <chain>` 的输出。抽成纯函数是为了能在
// 非 Linux 上测：Down 的删除匹配完全依赖这里的字段切分。
func parseNetfilterRules(raw, table, chain string) []Rule {
	var rules []Rule
	for _, line := range strings.Split(raw, "\n") {
		fields := splitRuleFields(line)
		if len(fields) < 3 || fields[0] != "-A" || fields[1] != chain {
			continue
		}
		rules = append(rules, Rule{Table: table, Chain: chain, Args: fields[2:]})
	}
	return rules
}

// splitRuleFields 按空白切分，但保留双引号里的空格：`--comment "a b"` 是一个参数，
// 用 strings.Fields 会切成两段，down 时匹配不到、规则删不掉。
func splitRuleFields(line string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range line {
		switch {
		case r == '"':
			inQuote = !inQuote
		case (r == ' ' || r == '\t') && !inQuote:
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

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

	rules := []Rule{
		// 客户端借云服的干净出口出网
		{Table: "nat", Chain: "POSTROUTING", Args: []string{"-s", network, "-o", wanIface, "-j", "MASQUERADE"}},
		{Chain: "FORWARD", Args: []string{"-s", network, "-j", "ACCEPT"}},
		// 入向只放已有连接的回程：节点机只会被动接受服务端 relay 的拨号，公网不需要
		// 主动发起到 10.10.0.0/24 的新连接（无状态全放行比实际需要宽）。
		{Chain: "FORWARD", Args: []string{"-d", network, "-m", "conntrack",
			"--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"}},

		// 内层 MTU 只有 1400，不 clamp 就会出现「小包通、网页打不开」的黑洞。
		// 必须限定来源网段和出口网卡：不限定的话，云服上所有经过 FORWARD 的 TCP SYN
		// （容器、其他转发业务）都会被一起 clamp，改了别人的 MSS。
		{Table: "mangle", Chain: "FORWARD", Args: []string{
			"-s", network, "-o", wanIface,
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

	// mode=udp 时数据包走 UDP，和 TCP 控制连接共用同一个端口号（不同协议，不冲突）。
	// 少了这条，客户端发出的认证探测包在服务端就被 INPUT 丢掉，UDP 模式永远连不上 ——
	// 而客户端不会偷偷回退 TCP，用户只会看到「怎么都连不上」。
	//
	// 和其它 INPUT 放行一样走 ensureRule 追加。这依赖链上没有更早的 DROP/REJECT：
	// 现网机器 `iptables -S INPUT` 只有本项目这几条 ACCEPT、默认策略 ACCEPT，
	// 追加的规则一定命中。若目标机的 INPUT 前面有 DROP，本项目所有端口放行都会
	// 一起失效，不只是这一条。
	if mode, _ := pki.ParseMode(cfg.Mode); mode == pki.ModeUDP {
		rules = append(rules, Rule{Chain: "INPUT",
			Args: []string{"-p", "udp", "--dport", listenPort, "-j", "ACCEPT"}})
	}
	return rules
}

// legacyRules 是本项目历史上铺过、现在已经不用的规则形态，供 Up 在铺新规则前清理。
//
// 为什么需要它：Rules() 全部落在内置链（FORWARD/INPUT/POSTROUTING），而 Up 的「整链
// 清空重铺」清的是 nat 里那个自定义链 —— 它早就没人用了。于是升级机器上旧版留下的
// 规则会一直躺着。实测踩到的是入向那条无状态全放行（-d 10.10.0.0/24 -j ACCEPT）：
// 它与新规则的 --ctstate ESTABLISHED,RELATED 并存，旧的在前面，收紧等于没生效。
//
// 按形态硬删（-D 容忍不存在），新装机器上是空操作。
func legacyRules(cfg Config) []Rule {
	return []Rule{
		{Chain: "FORWARD", Args: []string{"-d", cfg.Tunnel.Network, "-j", "ACCEPT"}},
		// 旧版 clamp 没限定网段与出口网卡，会把云服上所有 FORWARD 的 TCP SYN 一起
		// clamp。新规则加了限定，但旧规则仍排在前面生效 —— 不删等于没改。
		{Table: "mangle", Chain: "FORWARD", Args: []string{
			"-p", "tcp", "--tcp-flags", "SYN,RST", "SYN", "-j", "TCPMSS", "--clamp-mss-to-pmtu"}},
	}
}
