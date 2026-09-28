package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/antapp-cc/antapp-link/internal/pki"
)

// Command 是一条要执行的外部命令。
type Command struct {
	Name string
	Args []string
}

// logf 是包级日志钩子。
//
// 这一层不持有 logger（logger 在 App 里），但「分流到底写进去多少条路由」
// 是排障时第一个要看的东西 —— 没有它，路由没生效就只能靠猜。
// 默认空实现，NewApp 时接上真实 logger；测试环境保持安静。
var logf = func(format string, args ...any) {}

func (c Command) String() string {
	parts := append([]string{c.Name}, c.Args...)
	for i, p := range parts {
		if strings.ContainsAny(p, " \t") {
			parts[i] = `"` + p + `"`
		}
	}
	return strings.Join(parts, " ")
}

// IfaceDNS 记下一张网卡原来的 DNS，断开时要还回去。
//
// 不去区分「原值来自 DHCP 还是手工配置」：把同样的地址设成静态，解析行为完全一致，
// 而判断来源要靠解析 netsh 的本地化输出（中文系统关键字对不上），不值得。
type IfaceDNS struct {
	Name  string   `json:"name"`
	Index int      `json:"index"`
	DNS   []string `json:"dns"`

	// DNSv6 只用于「封堵隧道外的 DNS」，不参与还原。
	//
	// 路由器的 IPv6 DNS 是 RA 下发的，netsh 的 ipv4 子命令管不到它、也清不掉，
	// 所以没法「改」，只能「封」。但不封的后果很严重：系统会在 IPv4 DNS 被改到
	// 隧道之后转而用 IPv6 那条，查询绕开隧道被劫持 —— 这正是
	// 「minepi.com 打不开、Edge 新标签页空白」的直接原因。
	DNSv6 []string `json:"dns_v6,omitempty"`
}

// Snapshot 是接管网络之前的现场。它必须能完整还原 —— 还原不了就是把用户搞断网。
type Snapshot struct {
	DefaultGateway string     `json:"default_gateway"`
	DefaultIfIndex int        `json:"default_if_index"`
	Interfaces     []IfaceDNS `json:"interfaces"`
	CapturedAt     string     `json:"captured_at"`

	// ServerNextHop 是到达云服所用的下一跳。
	//
	// 空字符串表示云服就在直连网段里（本地拿 WSL 当服务端验证时就是这种情况）。
	// 这时**不加**绕行路由：现成的直连路由已经比默认路由更具体，抢不走；
	// 硬加一条指向默认网关的 /32 反而会覆盖它，把承载隧道的连接自己掐死。
	ServerNextHop string `json:"server_next_hop,omitempty"`
}

// NetConfig 是接管网络需要的参数。
type NetConfig struct {
	AdapterName string
	ServerIP    string
	TunnelIP    string
	Gateway     string
	Prefix      int
	MTU         int
	DNS         []string
}

// ServerIPOf 从连接码里的 host:port 取出主机部分。
func ServerIPOf(inv pki.Invite) string {
	host, _, err := net.SplitHostPort(inv.Server)
	if err != nil {
		return inv.Server
	}
	return host
}

// BuildNetConfig 从连接码拼出网络配置。
//
// 参数直接取自连接码而不等服务端 HELLO_ACK：连接码里已经有同样的值，
// 这样「抓快照 → 落盘 → 改网络」可以一气呵成，不必先把隧道拉起来再改网络。
func BuildNetConfig(inv pki.Invite) NetConfig {
	return NetConfig{
		AdapterName: AdapterName,
		ServerIP:    ServerIPOf(inv),
		TunnelIP:    inv.TunnelIP,
		Gateway:     inv.Gateway,
		Prefix:      inv.Prefix,
		MTU:         inv.MTU,
		DNS:         inv.DNS,
	}
}

func MaskFromPrefix(prefix int) string {
	if prefix < 0 || prefix > 32 {
		return "255.255.255.0"
	}
	return net.IP(net.CIDRMask(prefix, 32)).String()
}

// PrepareCommands 是接管网络的第一阶段：把隧道网卡本身配好。
//
// 之所以与路由分成两阶段：接管路由要显式绑定隧道网卡的接口索引，而索引只有在
// 地址配好之后才拿得到。
func PrepareCommands(cfg NetConfig) []Command {
	return []Command{
		// 只配地址、不配网关：默认路由的接管交给下面两条 /1 路由，
		// 免得 netsh 顺手加的默认路由和我们的混在一起、还原时删不干净。
		{"netsh", []string{"interface", "ipv4", "set", "address",
			fmt.Sprintf("name=%s", cfg.AdapterName),
			"source=static",
			fmt.Sprintf("addr=%s", cfg.TunnelIP),
			fmt.Sprintf("mask=%s", MaskFromPrefix(cfg.Prefix))}},
		{"netsh", []string{"interface", "ipv4", "set", "subinterface",
			cfg.AdapterName, fmt.Sprintf("mtu=%d", cfg.MTU), "store=persistent"}},
		// 接口跃点压到最低：Windows 按接口跃点挑 DNS 服务器，
		// 不压低的话解析还可能落到本地网卡上。
		{"netsh", []string{"interface", "ipv4", "set", "interface",
			fmt.Sprintf("name=%s", cfg.AdapterName), "metric=1"}},
	}
}

// TunnelRoutes 是两条把全球 IPv4 空间对半切的 /1 路由，也就是 OpenVPN `redirect-gateway def1`
// 用的手法。**这是本方案能不能真正接管流量的关键**，原因见 RouteCommands 的注释。
func TunnelRoutes(cfg NetConfig, ifIndex int) []Command {
	via := []string{"metric", "1", "if", strconv.Itoa(ifIndex)}
	return []Command{
		{"route", append([]string{"add", "0.0.0.0", "mask", "128.0.0.0", cfg.Gateway}, via...)},
		{"route", append([]string{"add", "128.0.0.0", "mask", "128.0.0.0", cfg.Gateway}, via...)},
	}
}

// RouteCommands 是第二阶段：绕行路由、接管路由、DNS。
//
// 次序有两条硬约束：
//
//  1. 绕行路由必须排在接管路由之前。反了的话，承载隧道的 TCP 连接自己会被送进隧道，
//     形成自噬 —— 表现是「连不上，但没有任何报错」。
//  2. DNS 必须排在接管路由之后，否则解析会先落到还被污染的本地 DNS 上。
//
// 接管流量用两条 /1 路由而不是改写 0.0.0.0/0：默认路由的胜负还要跟跃点数较劲，
// 而本地网卡那条往往是 metric 0，新加的抢不过它 —— 实测就是这样，流量根本没进隧道，
// DNS 查询照旧从本地出去、解析回一个被污染的地址。而 /1 比 /0 更具体，按最长前缀
// 匹配直接胜出，与跃点数无关。
func RouteCommands(snap Snapshot, cfg NetConfig, ifIndex int) []Command {
	var cmds []Command

	if snap.ServerNextHop != "" && cfg.ServerIP != "" {
		cmds = append(cmds, Command{"route", []string{
			"add", cfg.ServerIP, "mask", "255.255.255.255", snap.ServerNextHop, "metric", "1"}})
	}
	cmds = append(cmds, TunnelRoutes(cfg, ifIndex)...)

	for _, iface := range snap.Interfaces {
		cmds = append(cmds, setDNSCommands(iface.Name, cfg.DNS)...)
	}
	cmds = append(cmds, Command{"ipconfig", []string{"/flushdns"}})

	// 改完 DNS 再上锁，否则中间那几毫秒系统还能往隧道外发查询。
	return append(cmds, dnsLockCommands(snap.Interfaces, cfg.DNS)...)
}

// dnsLockPrefix 是 DNS 锁定防火墙规则的显示名前缀，解锁时按前缀整体删除。
const dnsLockPrefix = "AntApp Link 锁定 DNS"

// dnsLockCommands 生成「DNS 只许走隧道」的防火墙规则。
//
// 为什么必须有这一步：只把网卡的 DNS 改成隧道地址是「建议」不是「强制」。
// 系统照样可以把查询发给路由器下发的 IPv6 DNS —— 那条路在隧道外面，会被劫持，
// 于是域名解析出假 IP、网站打不开，看起来就像「连上一会就没网了」。
// OpenVPN 用 block-outside-dns、Clash 用 dns-hijack:any:53 解决的是同一件事。
//
// **按目标地址封堵，不按网卡名封堵** —— 这一点是照着 OpenVPN 的做法来的：
// 它枚举每张非隧道网卡自己的 DNS 服务器地址，逐个封堵。实测 Windows 防火墙的
// -InterfaceAlias 对出站 DNS 查询**不起作用**（规则显示 Enabled，查询照样出去），
// 而 -RemoteAddress 立刻生效。按接口挡是条死路。
//
// 不能禁用网卡的 IPv6 来达成同一目的：Disable-NetAdapterBinding 会重置整张网卡、
// 清掉 DNS 配置，实测直接把机器弄成完全无法解析。
func dnsLockCommands(ifaces []IfaceDNS, tunnelDNS []string) []Command {
	// 隧道自己的 DNS 不能封，否则隧道内的查询也断了
	isTunnel := func(addr string) bool {
		for _, t := range tunnelDNS {
			if addr == t {
				return true
			}
		}
		return false
	}

	// 同一个 DNS 地址可能被多张网卡共用，去重后每条只封一次
	seen := map[string]bool{}
	var outside []string
	for _, iface := range ifaces {
		// IPv4 和 IPv6 都要封。只封 IPv4 时系统会转用 RA 下发的 IPv6 DNS，
		// 查询照样绕开隧道 —— 实测踩过这个坑。
		for _, d := range append(append([]string{}, iface.DNS...), iface.DNSv6...) {
			d = strings.TrimSpace(d)
			if d == "" || isTunnel(d) || seen[d] {
				continue
			}
			seen[d] = true
			outside = append(outside, d)
		}
	}
	if len(outside) == 0 {
		return nil
	}

	var out []Command
	for _, addr := range outside {
		for _, proto := range []string{"UDP", "TCP"} {
			display := fmt.Sprintf("%s (%s %s)", dnsLockPrefix, addr, proto)
			// 先删同名规则再建，重复执行是幂等的
			script := fmt.Sprintf(
				"try { Remove-NetFirewallRule -DisplayName '%s' -ErrorAction SilentlyContinue } catch { }; "+
					"try { New-NetFirewallRule -DisplayName '%s' -Direction Outbound -Action Block "+
					"-Protocol %s -RemotePort 53 -RemoteAddress '%s' -Profile Any -ErrorAction Stop | Out-Null } catch { }; "+
					"exit 0",
				display, display, proto, addr)
			out = append(out, Command{"powershell", []string{
				"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script}})
		}
	}
	return out
}

// dnsUnlockCommands 删掉全部锁定规则。按前缀删，不依赖当时封了哪些地址，
// 所以即使快照损坏也能把规则清干净。
func dnsUnlockCommands() []Command {
	return []Command{{"powershell", []string{
		"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command",
		fmt.Sprintf("try { Get-NetFirewallRule -DisplayName '%s*' -ErrorAction SilentlyContinue | "+
			"Remove-NetFirewallRule -ErrorAction SilentlyContinue } catch { }; exit 0", dnsLockPrefix)}}}
}

// RestoreCommands 与 RouteCommands 逆序：先撤接管路由，再撤绕行路由。
// 反过来会留下一个「流量正被送进一条已经不通的隧道」的窗口。
func RestoreCommands(snap Snapshot, cfg NetConfig) []Command {
	// 先解 DNS 锁定：万一后面的还原卡住，至少不会把用户留在「DNS 被锁死、
	// 但隧道已经拆掉」的状态 —— 那等于完全没法解析域名。
	cmds := dnsUnlockCommands()

	for _, iface := range snap.Interfaces {
		cmds = append(cmds, restoreDNSCommands(iface)...)
	}

	// 第一条是替旧版本擦屁股：早期版本改写的是 0.0.0.0/0，升级后不会再有新的一批，
	// 但用户机器上可能还留着。它只匹配下一跳等于本隧道网关的规则，不会误伤用户的默认路由。
	cmds = append(cmds,
		Command{"route", []string{"delete", "0.0.0.0", "mask", "0.0.0.0", cfg.Gateway}},
		Command{"route", []string{"delete", "0.0.0.0", "mask", "128.0.0.0", cfg.Gateway}},
		Command{"route", []string{"delete", "128.0.0.0", "mask", "128.0.0.0", cfg.Gateway}})

	if cfg.ServerIP != "" {
		// 删除不需要知道下一跳，所以快照损坏时也能把这条撤掉
		cmds = append(cmds, Command{"route", []string{
			"delete", cfg.ServerIP, "mask", "255.255.255.255"}})
	}
	cmds = append(cmds,
		// 先判存在再删，并让命令自己保证退出码为 0。
		// Remove-NetIPAddress 在目标不存在（网卡或地址已被系统回收）时抛的是
		// Cmdletization 层的终止性错误，-ErrorAction SilentlyContinue 压不住，
		// 退出码照样是 1 —— 上层会据此误报「还原网络配置失败」。
		Command{"powershell", []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
			"-Command", fmt.Sprintf("try { Remove-NetIPAddress -InterfaceAlias '%s' -IPAddress '%s' -Confirm:$false -ErrorAction Stop } catch { }; exit 0",
				cfg.AdapterName, cfg.TunnelIP)}},
		Command{"ipconfig", []string{"/flushdns"}})
	return cmds
}

func setDNSCommands(ifaceName string, servers []string) []Command {
	if len(servers) == 0 {
		return nil
	}
	name := fmt.Sprintf("name=%s", ifaceName)
	out := []Command{{"netsh", []string{"interface", "ipv4", "set", "dnsservers",
		name, "static", servers[0], "primary"}}}
	for i, s := range servers[1:] {
		out = append(out, Command{"netsh", []string{"interface", "ipv4", "add", "dnsservers",
			name, s, fmt.Sprintf("index=%d", i+2)}})
	}
	return out
}

func restoreDNSCommands(iface IfaceDNS) []Command {
	name := fmt.Sprintf("name=%s", iface.Name)
	if len(iface.DNS) == 0 {
		// 原来就没有静态 DNS，还回自动获取，别塞一个编造的地址
		return []Command{{"netsh", []string{"interface", "ipv4", "set", "dnsservers",
			name, "source=dhcp"}}}
	}
	return setDNSCommands(iface.Name, iface.DNS)
}

// ---------- 快照落盘 ----------

const StateFileName = "state.json"

func StatePath(root string) string { return filepath.Join(RuntimeDir(root), StateFileName) }

// SaveSnapshot 落盘。文件存在本身就代表「上次接管过网络且没干净退出」，
// 所以它必须在改写网络之前写入、在还原成功之后才删除。
func SaveSnapshot(path string, snap Snapshot) error {
	raw, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadSnapshot 的第二个返回值表示「文件是否存在」。文件存在但内容坏了时同样返回 true ——
// 调用方据此知道网络可能还被接管着，不能当没事发生。
func LoadSnapshot(path string) (Snapshot, bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Snapshot{}, false, nil
		}
		return Snapshot{}, false, err
	}
	var snap Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return Snapshot{}, true, fmt.Errorf("状态文件 %s 损坏: %w", path, err)
	}
	return snap, true, nil
}

func RemoveSnapshot(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func newSnapshot(gateway string, ifIndex int, serverNextHop string, ifaces []IfaceDNS) Snapshot {
	return Snapshot{
		DefaultGateway: gateway,
		DefaultIfIndex: ifIndex,
		ServerNextHop:  serverNextHop,
		Interfaces:     ifaces,
		CapturedAt:     time.Now().Format(time.RFC3339),
	}
}
