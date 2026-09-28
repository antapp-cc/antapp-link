package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/antapp-cc/antapp-link/internal/pki"
)

// Command 是一条要执行的外部命令。如今只剩低频路径还在用
// （schtasks 开机自启、旧版防火墙规则扫尾），网络接管已全部原生化。
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

// splitRouteMetric 是国内直连路由的 route metric。还原时按
// 「下一跳 = 原默认网关 且 metric = 此值」整批扫除，网段表更新过也能清干净。
const splitRouteMetric = 5

// tunnelOverheadBytes 是承载内层 IP 包的外层开销估算：
// 外层 IPv4 头 20 + TCP 头 20 + TLS 记录头与认证标签约 16~22。
const tunnelOverheadBytes = 56

// effectiveMTU 是隧道网卡当前实际生效的 MTU。配置值（连接码里的）是固定数，
// 实际值会跟随出口链路自动收缩/回升 —— 日志要显示的是这个，不是配置值。
// 非 Windows 平台无人写入，恒为 0（调用方按 0 = 未接管处理）。
var effectiveMTU atomic.Int64

// targetTunnelMTU 跟随出口计算隧道 MTU（WireGuard monitorMTU 同思路）：
// 取连接码 MTU 与「出口 MTU - 隧道开销」的较小值，下限 576（IPv4 主机必须
// 能重组的最小值）。出口变小（PPPoE/4G/套了层 VPN）时自动收缩避免大包黑洞，
// 出口恢复后自动回升。egressMTU 为 0（读不到）时退回连接码值。
func targetTunnelMTU(egressMTU, inviteMTU uint32) uint32 {
	target := inviteMTU
	if egressMTU > tunnelOverheadBytes && egressMTU-tunnelOverheadBytes < target {
		target = egressMTU - tunnelOverheadBytes
	}
	if target < 576 {
		target = 576
	}
	return target
}

// dnsLockPrefix 是旧版 DNS 锁定防火墙规则的显示名前缀。新版本不再创建这类规则，
// 只在启动自愈时按这个前缀清理旧版残留。
const dnsLockPrefix = "AntApp Link 锁定 DNS"

// nrptRuleComment 是 NRPT 规则的识别标记，增删都按它筛，不碰别人的规则。
const nrptRuleComment = "AntApp Link DNS"

// nrptNamespace 是被污染、必须走隧道解析的域名作用域。
//
// api.minepi.com 在国内所有递归上都是被污染的假答案（实测路由器 DNS、AliDNS、
// DNSPod 给的全是 Twitter/Facebook 地址段的 IP），唯一干净的来源是隧道出口的
// 解析。而路由器 RA 下发的 IPv6 DNS 应答快、抢得赢隧道，它还是系统托管的
// 删不掉，所以用 NRPT（名称解析策略表）按域名作用域直接规定解析去向 ——
// 它的优先级高于一切网卡 DNS 配置，且不加防火墙规则。
//
// 只圈 Pi 自己的域名，刻意不扩大干预面：其它域照常走系统默认（快），规则万一
// 成为孤儿（崩溃后又卸载）也只影响 Pi 域名，不影响整机上网。
const nrptNamespace = ".minepi.com"

// nrptKeyPath / nrptRuleGUID 是 NRPT 规则在注册表的落脚点。
// GUID 固定不变：重写即覆盖（幂等），删除按标记扫时也认得出自己。
const nrptKeyPath = `SYSTEM\CurrentControlSet\Services\Dnscache\Parameters\DnsPolicyConfig`
const nrptRuleGUID = "{8F2A7B41-9C5D-4E3A-B6D8-21F0A9C7E4D2}"

// routeOp 是接管时的一条路由。onTunnel 区分挂隧道网卡还是原默认网卡：
// Windows 按接口选路，挂错网卡流量就进不了隧道（旧版 netsh 时代的硬伤，
// 现在在数据结构上杜绝）。
type routeOp struct {
	dest     netip.Prefix
	nextHop  netip.Addr
	metric   uint32
	onTunnel bool
}

// routePlan 按生效顺序产出全部接管路由。顺序有两条硬约束：
//
//  1. 绕行路由必须在 /1 接管路由之前。反了的话承载隧道的 TCP 连接会被
//     自己送进隧道，表现是「连不上，但没有任何报错」。
//  2. 云服在直连网段内时不加绕行路由：现成的直连路由更具体，加一条指向
//     默认网关的 /32 会覆盖它，隧道自己掐死自己。
func routePlan(snap Snapshot, cfg NetConfig) []routeOp {
	gw, err := netip.ParseAddr(cfg.Gateway)
	if err != nil {
		// 没有隧道网关就谈不上接管，宁可一条不加也不产半个计划
		return nil
	}
	var ops []routeOp
	if snap.ServerNextHop != "" && cfg.ServerIP != "" {
		if hop, err := netip.ParseAddr(snap.ServerNextHop); err == nil {
			if srv, err := netip.ParseAddr(cfg.ServerIP); err == nil {
				ops = append(ops, routeOp{dest: netip.PrefixFrom(srv, 32), nextHop: hop, metric: 1})
			}
		}
	}
	for _, dest := range [2]netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/1"),
		netip.MustParsePrefix("128.0.0.0/1"),
	} {
		ops = append(ops, routeOp{dest: dest, nextHop: gw, metric: 1, onTunnel: true})
	}
	return ops
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
