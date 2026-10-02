package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/antapp-cc/antapp-link/internal/pki"
)

// Config 对应 /etc/antapp-link/server.json。
type Config struct {
	Listen       string       `json:"listen"`
	Tunnel       TunnelConfig `json:"tunnel"`
	DNS          []string     `json:"dns"`
	ForwardPorts PortRange    `json:"forward_ports"`
	PKIDir       string       `json:"pki_dir"`

	// Mode 决定数据通道走法："tcp"（默认，TLS over TCP）或 "udp"
	// （控制仍走 TCP，数据包走 UDP + AES-GCM）。
	// 它会被写进签发的连接码，客户端据此选择。
	Mode string `json:"mode,omitempty"`

	// MaxMembers 是早期版本写在顶层的 max_members，只用于兼容读取：
	// LoadConfig / SaveConfig 会把它并进 Tunnel.MaxMembers（文档里的位置）。
	MaxMembers int `json:"max_members,omitempty"`
}

type TunnelConfig struct {
	Device   string `json:"device"`
	Network  string `json:"network"`
	ServerIP string `json:"server_ip"`
	ClientIP string `json:"client_ip"`
	MTU      int    `json:"mtu"`

	// MaxMembers 是单个逻辑会话允许的最大并行连接数（1-4，默认 1=关闭多连接）。
	// 客户端请求的 members 会被压到这个上限以下。
	MaxMembers int `json:"max_members,omitempty"`
}

type PortRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// Default 的转发端口段就是正式的 31400-31409。
//
// 早先并网验证期用过 31410-31419，为的是避开现网 rinetd 占着的那一段；
// 现在老服务已经下线，直接用正式端口段，不用再切来切去。
func Default() Config {
	return Config{
		Listen: "0.0.0.0:62233",
		Tunnel: TunnelConfig{
			Device:     "antapp0",
			Network:    "10.10.0.0/24",
			ServerIP:   "10.10.0.1",
			ClientIP:   "10.10.0.2",
			MTU:        1400,
			MaxMembers: 1,
		},
		// 客户端的 DNS 指向隧道网关：服务端在网关上跑 dnsmasq（filter-AAAA）。
		// 隧道只接管 IPv4，绝不能把 AAAA 发给客户端 —— 浏览器拿到 v6 地址会
		// 直连（绕开隧道）死路，实测就是 Pi Desktop 内嵌页面全白屏的根因。
		DNS:          []string{"10.10.0.1"},
		ForwardPorts: PortRange{Start: 31400, End: 31409},
		PKIDir:       "/etc/antapp-link/pki",
	}
}

// migrateLegacyMaxMembers 把早期写在顶层的 max_members 并进 tunnel 层。
// 顶层有值时以它为准（那是用户显式写下的旧配置），随后清空，只留 tunnel 一个真值来源。
func (c *Config) migrateLegacyMaxMembers() {
	if c.MaxMembers > 0 {
		c.Tunnel.MaxMembers = c.MaxMembers
	}
	c.MaxMembers = 0
}

// LoadConfig 先铺默认值再让文件覆盖，所以配置文件只需要写想改的字段。
func LoadConfig(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("读取配置 %s: %w", path, err)
	}
	cfg := Default()
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("解析配置 %s: %w", path, err)
	}
	cfg.migrateLegacyMaxMembers()
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("配置 %s 不合法: %w", path, err)
	}
	return cfg, nil
}

// SaveConfig 把配置写回文件。
func SaveConfig(path string, cfg Config) error {
	cfg.migrateLegacyMaxMembers()
	if err := cfg.Validate(); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o644)
}

// Overrides 是安装时可以指定、其余用默认值的几项。
//
// 有了它，安装脚本就不必靠 sed 去改 JSON —— 那种做法一旦字段顺序或缩进变了
// 就会静默失效，而这里改的是结构体字段，改没改过一目了然。
type Overrides struct {
	Listen       string // 形如 0.0.0.0:62233；空表示不改
	ForwardStart int    // 0 表示不改
	ForwardEnd   int
	Network      string // 空表示不改
	Mode         string // tcp / udp；空表示不改
}

// ApplyOverrides 把非空项写进配置。网段变了的话，服务端/客户端地址按新网段重算
// （取 .1 和 .2），否则会跟 Validate 的「地址必须落在网段内」冲突。
func (c *Config) ApplyOverrides(o Overrides) {
	if o.Listen != "" {
		c.Listen = o.Listen
	}
	if o.Mode != "" {
		c.Mode = o.Mode
	}
	if o.ForwardStart > 0 && o.ForwardEnd > 0 {
		c.ForwardPorts = PortRange{Start: o.ForwardStart, End: o.ForwardEnd}
	}
	if o.Network != "" && o.Network != c.Tunnel.Network {
		if ip, ipnet, err := net.ParseCIDR(o.Network); err == nil {
			ones, _ := ipnet.Mask.Size()
			base := ip.Mask(ipnet.Mask)
			server := make(net.IP, len(base))
			copy(server, base)
			server[len(server)-1] = 1
			client := make(net.IP, len(base))
			copy(client, base)
			client[len(client)-1] = 2

			c.Tunnel.Network = fmt.Sprintf("%s/%d", base.String(), ones)
			c.Tunnel.ServerIP = server.String()
			c.Tunnel.ClientIP = client.String()
			// DNS 跟着网关走：中继（dnsmasq filter-AAAA）永远绑在隧道网关上
			c.DNS = []string{c.Tunnel.ServerIP}
		}
	}
}

func (c Config) Validate() error {
	if _, err := pki.ParseMode(c.Mode); err != nil {
		return err
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return fmt.Errorf("listen %q 必须是 host:port 形式: %w", c.Listen, err)
	}
	_, ipnet, err := net.ParseCIDR(c.Tunnel.Network)
	if err != nil {
		return fmt.Errorf("tunnel.network %q 不是合法网段: %w", c.Tunnel.Network, err)
	}
	serverIP := net.ParseIP(c.Tunnel.ServerIP)
	if serverIP == nil {
		return fmt.Errorf("tunnel.server_ip %q 不是合法 IP", c.Tunnel.ServerIP)
	}
	clientIP := net.ParseIP(c.Tunnel.ClientIP)
	if clientIP == nil {
		return fmt.Errorf("tunnel.client_ip %q 不是合法 IP", c.Tunnel.ClientIP)
	}
	if !ipnet.Contains(serverIP) {
		return fmt.Errorf("tunnel.server_ip %s 不在网段 %s 内", c.Tunnel.ServerIP, c.Tunnel.Network)
	}
	if !ipnet.Contains(clientIP) {
		return fmt.Errorf("tunnel.client_ip %s 不在网段 %s 内", c.Tunnel.ClientIP, c.Tunnel.Network)
	}
	if serverIP.Equal(clientIP) {
		return errors.New("tunnel.server_ip 与 tunnel.client_ip 不能相同")
	}
	if strings.TrimSpace(c.Tunnel.Device) == "" {
		return errors.New("tunnel.device 不能为空")
	}
	// 576 是 IPv4 主机必须能重组的最小值；1500 是常规以太网上限。
	if c.Tunnel.MTU < 576 || c.Tunnel.MTU > 1500 {
		return fmt.Errorf("tunnel.mtu %d 越界（576-1500）", c.Tunnel.MTU)
	}
	if len(c.DNS) == 0 {
		return errors.New("dns 不能为空")
	}
	for _, d := range c.DNS {
		if net.ParseIP(d) == nil {
			return fmt.Errorf("dns %q 不是合法 IP", d)
		}
	}
	if c.ForwardPorts.Start < 1 || c.ForwardPorts.End > 65535 || c.ForwardPorts.Start > c.ForwardPorts.End {
		return fmt.Errorf("forward_ports %d-%d 不是合法的端口区间", c.ForwardPorts.Start, c.ForwardPorts.End)
	}
	if strings.TrimSpace(c.PKIDir) == "" {
		return errors.New("pki_dir 不能为空")
	}
	if c.Tunnel.MaxMembers < 1 || c.Tunnel.MaxMembers > 4 {
		return fmt.Errorf("max_members %d 越界（1-4）", c.Tunnel.MaxMembers)
	}
	return nil
}

func (c Config) PrefixLen() int {
	_, ipnet, err := net.ParseCIDR(c.Tunnel.Network)
	if err != nil {
		return 24
	}
	ones, _ := ipnet.Mask.Size()
	return ones
}
