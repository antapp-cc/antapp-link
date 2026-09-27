package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
)

// Config 对应 /etc/antapp-link/server.json。
type Config struct {
	Listen       string       `json:"listen"`
	Tunnel       TunnelConfig `json:"tunnel"`
	DNS          []string     `json:"dns"`
	ForwardPorts PortRange    `json:"forward_ports"`
	PKIDir       string       `json:"pki_dir"`
}

type TunnelConfig struct {
	Device   string `json:"device"`
	Network  string `json:"network"`
	ServerIP string `json:"server_ip"`
	ClientIP string `json:"client_ip"`
	MTU      int    `json:"mtu"`
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
			Device:   "antapp0",
			Network:  "10.10.0.0/24",
			ServerIP: "10.10.0.1",
			ClientIP: "10.10.0.2",
			MTU:      1400,
		},
		DNS:          []string{"8.8.8.8", "149.112.112.112"},
		ForwardPorts: PortRange{Start: 31400, End: 31409},
		PKIDir:       "/etc/antapp-link/pki",
	}
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
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("配置 %s 不合法: %w", path, err)
	}
	return cfg, nil
}

func (c Config) Validate() error {
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
