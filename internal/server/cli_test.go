package server

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antapp-cc/antapp-link/internal/pki"
)

func writeTestConfig(t *testing.T, dir string, mutate func(*Config)) string {
	t.Helper()
	cfg := Default()
	cfg.PKIDir = filepath.Join(dir, "pki")
	if mutate != nil {
		mutate(&cfg)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "server.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunCLIUsageAndUnknownCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := RunCLI(nil, &out, &errOut); code != 2 {
		t.Errorf("无参数应返回 2，实际 %d", code)
	}
	if !strings.Contains(errOut.String(), "用法") {
		t.Error("应该打印用法")
	}

	out.Reset()
	errOut.Reset()
	if code := RunCLI([]string{"nonsense"}, &out, &errOut); code != 2 {
		t.Errorf("未知子命令应返回 2，实际 %d", code)
	}

	out.Reset()
	errOut.Reset()
	if code := RunCLI([]string{"version"}, &out, &errOut); code != 0 {
		t.Errorf("version 应返回 0，实际 %d", code)
	}
	if !strings.Contains(out.String(), "antapp-linkd") {
		t.Errorf("version 输出异常: %q", out.String())
	}
}

// 这条覆盖了服务端交付的主链路：init 生成 CA → invite 签发客户端证书并产出连接码。
func TestCLIInitAndInviteEndToEnd(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeTestConfig(t, dir, nil)

	var out, errOut bytes.Buffer
	if code := RunCLI([]string{"init", "-c", cfgPath}, &out, &errOut); code != 0 {
		t.Fatalf("init 失败(%d): %s", code, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "pki", "ca.crt")); err != nil {
		t.Fatalf("init 之后应该有 CA: %v", err)
	}

	out.Reset()
	errOut.Reset()
	code := RunCLI([]string{
		"invite", "pinode-01", "-c", cfgPath, "-o", dir,
		"--server", "103.143.11.34:62233",
	}, &out, &errOut)
	if code != 0 {
		t.Fatalf("invite 失败(%d): %s", code, errOut.String())
	}

	raw, err := os.ReadFile(filepath.Join(dir, "pinode.antapp"))
	if err != nil {
		t.Fatalf("读连接码文件: %v", err)
	}
	var inv pki.Invite
	if err := json.Unmarshal(raw, &inv); err != nil {
		t.Fatalf("连接码文件不是合法 JSON: %v", err)
	}
	if inv.Server != "103.143.11.34:62233" {
		t.Errorf("Server = %q", inv.Server)
	}
	if inv.TunnelIP != "10.10.0.2" || inv.Gateway != "10.10.0.1" {
		t.Errorf("隧道地址不对: %s / %s", inv.TunnelIP, inv.Gateway)
	}

	// 节点机靠 stdout 里那行单行连接码导入
	var single string
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), pki.Scheme) {
			single = strings.TrimSpace(line)
		}
	}
	if single == "" {
		t.Fatalf("stdout 里没有单行连接码:\n%s", out.String())
	}
	decoded, err := pki.Decode(single)
	if err != nil {
		t.Fatalf("单行连接码解不开: %v", err)
	}
	if decoded.Name != "pinode-01" {
		t.Errorf("Name = %q, want pinode-01", decoded.Name)
	}
}

func TestCLIInitIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeTestConfig(t, dir, nil)
	var out, errOut bytes.Buffer
	for i := 1; i <= 2; i++ {
		out.Reset()
		errOut.Reset()
		if code := RunCLI([]string{"init", "-c", cfgPath}, &out, &errOut); code != 0 {
			t.Fatalf("第 %d 次 init 失败: %s", i, errOut.String())
		}
	}
}

func TestCLIInviteNeedsClientName(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeTestConfig(t, dir, nil)
	var out, errOut bytes.Buffer
	if code := RunCLI([]string{"invite", "-c", cfgPath}, &out, &errOut); code != 2 {
		t.Errorf("缺少客户端名应返回 2，实际 %d", code)
	}
}

// status 必须在隧道没跑的时候也能用，否则排查问题时反而先被它绊住。
func TestCLIStatusWithoutTunnel(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeTestConfig(t, dir, func(c *Config) {
		c.ForwardPorts = PortRange{Start: 31400, End: 31409}
	})
	var out, errOut bytes.Buffer
	if code := RunCLI([]string{"status", "-c", cfgPath}, &out, &errOut); code != 0 {
		t.Fatalf("status 失败(%d): %s", code, errOut.String())
	}
	s := out.String()
	if !strings.Contains(s, "31400-31409") {
		t.Errorf("status 应显示转发端口段:\n%s", s)
	}
	if !strings.Contains(s, "DNAT") {
		t.Errorf("status 应显示 DNAT 规则:\n%s", s)
	}
}

func TestCLIUsesConfigForInvite(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeTestConfig(t, dir, func(c *Config) {
		c.Tunnel.ClientIP = "10.10.0.77"
		c.ForwardPorts = PortRange{Start: 31400, End: 31409}
	})
	var out, errOut bytes.Buffer
	if code := RunCLI([]string{"init", "-c", cfgPath}, &out, &errOut); code != 0 {
		t.Fatalf("init 失败: %s", errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := RunCLI([]string{
		"invite", "node-x", "-c", cfgPath, "-o", dir, "--server", "1.2.3.4:62233",
	}, &out, &errOut); code != 0 {
		t.Fatalf("invite 失败: %s", errOut.String())
	}
	if !strings.Contains(out.String(), "10.10.0.77") {
		t.Errorf("invite 应该用配置里的 client_ip:\n%s", out.String())
	}
}
