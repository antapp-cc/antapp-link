package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultConfigIsValid(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("默认配置必须合法: %v", err)
	}
	if got := cfg.PrefixLen(); got != 24 {
		t.Errorf("PrefixLen = %d, want 24", got)
	}
	if cfg.ForwardPorts.Start != 31400 || cfg.ForwardPorts.End != 31409 {
		t.Errorf("默认端口段应该是正式的 31400-31409，实际 %d-%d",
			cfg.ForwardPorts.Start, cfg.ForwardPorts.End)
	}
	if cfg.Listen != "0.0.0.0:62233" {
		t.Errorf("默认监听端口应避开现网 OpenVPN 的 62231，实际 %s", cfg.Listen)
	}
}

func TestLoadConfigFillsDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.json")
	// 只写想改的字段，其余应保留默认值
	if err := os.WriteFile(path, []byte(`{"tunnel":{"mtu":1300}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Tunnel.MTU != 1300 {
		t.Errorf("MTU = %d, want 1300", cfg.Tunnel.MTU)
	}
	if cfg.Tunnel.Device != "antapp0" {
		t.Errorf("未覆盖的字段应保留默认值，Device = %q", cfg.Tunnel.Device)
	}
	if len(cfg.DNS) != 2 {
		t.Errorf("未覆盖的 DNS 应保留默认值，实际 %v", cfg.DNS)
	}
	if cfg.Listen != "0.0.0.0:62233" {
		t.Errorf("未覆盖的 Listen 应保留默认值，实际 %q", cfg.Listen)
	}
}

func TestLoadConfigRejectsMissingFile(t *testing.T) {
	if _, err := LoadConfig(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Error("配置文件不存在时必须报错")
	}
}

func TestValidateRejectsBadConfig(t *testing.T) {
	base := Default()
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"listen 缺端口", func(c *Config) { c.Listen = "0.0.0.0" }, "listen"},
		{"网段非法", func(c *Config) { c.Tunnel.Network = "10.10.0.0" }, "network"},
		{"server_ip 非法", func(c *Config) { c.Tunnel.ServerIP = "nope" }, "server_ip"},
		{"server_ip 不在网段内", func(c *Config) { c.Tunnel.ServerIP = "192.168.1.1" }, "server_ip"},
		{"client_ip 不在网段内", func(c *Config) { c.Tunnel.ClientIP = "172.16.0.2" }, "client_ip"},
		{"server 与 client 相同", func(c *Config) { c.Tunnel.ClientIP = c.Tunnel.ServerIP }, "不能相同"},
		{"device 为空", func(c *Config) { c.Tunnel.Device = "" }, "device"},
		{"mtu 太小", func(c *Config) { c.Tunnel.MTU = 100 }, "mtu"},
		{"mtu 太大", func(c *Config) { c.Tunnel.MTU = 9000 }, "mtu"},
		{"dns 为空", func(c *Config) { c.DNS = nil }, "dns"},
		{"dns 里混进域名", func(c *Config) { c.DNS = []string{"8.8.8.8", "dns.google"} }, "dns"},
		{"端口区间颠倒", func(c *Config) { c.ForwardPorts = PortRange{Start: 31419, End: 31400} }, "forward_ports"},
		{"端口越界", func(c *Config) { c.ForwardPorts = PortRange{Start: 0, End: 31409} }, "forward_ports"},
		{"pki_dir 为空", func(c *Config) { c.PKIDir = "  " }, "pki_dir"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := base
			c.mutate(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatal("应该报错")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("错误信息 %q 应该提到 %q", err.Error(), c.want)
			}
		})
	}
}
