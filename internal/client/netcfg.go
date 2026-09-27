package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/antapp-cc/antapp-link/internal/pki"
)

// Command 是一条要执行的外部命令。
type Command struct {
	Name string
	Args []string
}

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
}

// NetConfig 是接管网络需要的参数。
type NetConfig struct {
	AdapterName     string
	ServerIP        string
	TunnelIP        string
	Gateway         string
	Prefix          int
	MTU             int
	DNS             []string
	OriginalGateway string
}

// BuildNetConfig 从连接码拼出网络配置。
//
// 参数直接取自连接码而不等服务端 HELLO_ACK：连接码里已经有同样的值，
// 这样「抓快照 → 落盘 → 改网络」可以一气呵成，不必先把隧道拉起来再改网络。
func BuildNetConfig(inv pki.Invite, originalGateway string) NetConfig {
	serverIP, _, err := net.SplitHostPort(inv.Server)
	if err != nil {
		serverIP = inv.Server
	}
	return NetConfig{
		AdapterName:     AdapterName,
		ServerIP:        serverIP,
		TunnelIP:        inv.TunnelIP,
		Gateway:         inv.Gateway,
		Prefix:          inv.Prefix,
		MTU:             inv.MTU,
		DNS:             inv.DNS,
		OriginalGateway: originalGateway,
	}
}

func MaskFromPrefix(prefix int) string {
	if prefix < 0 || prefix > 32 {
		return "255.255.255.0"
	}
	return net.IP(net.CIDRMask(prefix, 32)).String()
}

// ApplyCommands 生成接管网络的命令，顺序敏感：
//
//  1. 先配好隧道网卡自己的地址、MTU 与接口跃点
//  2. 再加云服 IP 的 /32 绕行路由 —— 必须在改写默认路由之前。顺序反了，
//     承载隧道的 TCP 连接自己会被送进隧道，形成自噬：表现为「连不上，但没有任何报错」
//  3. 才轮到把默认路由指向隧道
//  4. 最后改 DNS，否则解析会先跑到还被污染的本地 DNS 上
func ApplyCommands(snap Snapshot, cfg NetConfig) []Command {
	cmds := []Command{
		// 只配地址不配网关：让默认路由完全由下面那条显式命令控制，
		// 免得 netsh 顺手加的默认路由和我们的混在一起，还原时删不干净。
		{"netsh", []string{"interface", "ipv4", "set", "address",
			fmt.Sprintf("name=%s", cfg.AdapterName),
			"source=static",
			fmt.Sprintf("addr=%s", cfg.TunnelIP),
			fmt.Sprintf("mask=%s", MaskFromPrefix(cfg.Prefix))}},
		{"netsh", []string{"interface", "ipv4", "set", "subinterface",
			cfg.AdapterName, fmt.Sprintf("mtu=%d", cfg.MTU), "store=persistent"}},
		{"netsh", []string{"interface", "ipv4", "set", "interface",
			fmt.Sprintf("name=%s", cfg.AdapterName), "metric=1"}},
	}

	if cfg.OriginalGateway != "" && cfg.ServerIP != "" {
		cmds = append(cmds, Command{"route", []string{
			"add", cfg.ServerIP, "mask", "255.255.255.255", cfg.OriginalGateway, "metric", "1"}})
	}
	cmds = append(cmds, Command{"route", []string{
		"add", "0.0.0.0", "mask", "0.0.0.0", cfg.Gateway, "metric", "1"}})

	for _, iface := range snap.Interfaces {
		cmds = append(cmds, setDNSCommands(iface.Name, cfg.DNS)...)
	}
	return append(cmds, Command{"ipconfig", []string{"/flushdns"}})
}

// RestoreCommands 与 ApplyCommands 逆序：先撤默认路由，再撤 /32 绕行。
// 反过来会留下一个「流量正被送进一条已经不通的隧道」的窗口。
func RestoreCommands(snap Snapshot, cfg NetConfig) []Command {
	var cmds []Command

	for _, iface := range snap.Interfaces {
		cmds = append(cmds, restoreDNSCommands(iface)...)
	}

	cmds = append(cmds,
		Command{"route", []string{"delete", "0.0.0.0", "mask", "0.0.0.0", cfg.Gateway}})
	if cfg.OriginalGateway != "" && cfg.ServerIP != "" {
		cmds = append(cmds, Command{"route", []string{
			"delete", cfg.ServerIP, "mask", "255.255.255.255"}})
	}
	cmds = append(cmds,
		Command{"netsh", []string{"interface", "ipv4", "delete", "address",
			fmt.Sprintf("name=%s", cfg.AdapterName), fmt.Sprintf("addr=%s", cfg.TunnelIP)}},
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

func StatePath(dataDir string) string { return filepath.Join(dataDir, StateFileName) }

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

func newSnapshot(gateway string, ifIndex int, ifaces []IfaceDNS) Snapshot {
	return Snapshot{
		DefaultGateway: gateway,
		DefaultIfIndex: ifIndex,
		Interfaces:     ifaces,
		CapturedAt:     time.Now().Format(time.RFC3339),
	}
}
