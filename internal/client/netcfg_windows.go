//go:build windows

package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
)

// CREATE_NO_WINDOW：托盘程序调子进程时不能闪黑框
const createNoWindow = 0x08000000

func hiddenProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}

func runCommand(c Command) error {
	cmd := exec.Command(c.Name, c.Args...)
	cmd.SysProcAttr = hiddenProcAttr()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w: %s", c.String(), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func runPowerShell(script string) (string, error) {
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-Command", script)
	cmd.SysProcAttr = hiddenProcAttr()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("powershell: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// Capture 抓取接管前的现场。
//
// 走 PowerShell 的 cmdlet 而不是解析 netsh 的文本输出：cmdlet 的属性名不随系统语言变化，
// 而中文 Windows 上 netsh 的「静态配置的 DNS 服务器」这类关键字会让关键字匹配全部落空。
func Capture() (Snapshot, error) {
	gateway, ifIndex, err := captureDefaultRoute()
	if err != nil {
		return Snapshot{}, err
	}
	ifaces, err := captureDNS()
	if err != nil {
		return Snapshot{}, err
	}
	return newSnapshot(gateway, ifIndex, ifaces), nil
}

type psRoute struct {
	NextHop        string `json:"NextHop"`
	InterfaceIndex int    `json:"InterfaceIndex"`
}

func captureDefaultRoute() (string, int, error) {
	const script = `Get-NetRoute -AddressFamily IPv4 -DestinationPrefix '0.0.0.0/0' -ErrorAction SilentlyContinue ` +
		`| Where-Object { $_.NextHop -ne '0.0.0.0' } ` +
		`| Sort-Object RouteMetric ` +
		`| Select-Object -First 1 NextHop,InterfaceIndex | ConvertTo-Json -Compress`
	out, err := runPowerShell(script)
	if err != nil {
		return "", 0, fmt.Errorf("查询默认路由: %w", err)
	}
	text := strings.TrimSpace(out)
	if text == "" {
		return "", 0, errors.New("当前没有默认路由（可能没联网），无法继续")
	}
	var r psRoute
	if err := json.Unmarshal([]byte(text), &r); err != nil {
		return "", 0, fmt.Errorf("解析默认路由 %q: %w", text, err)
	}
	if r.NextHop == "" {
		return "", 0, errors.New("默认网关为空，无法继续")
	}
	return r.NextHop, r.InterfaceIndex, nil
}

type psDNS struct {
	InterfaceAlias  string   `json:"InterfaceAlias"`
	InterfaceIndex  int      `json:"InterfaceIndex"`
	ServerAddresses []string `json:"ServerAddresses"`
}

func captureDNS() ([]IfaceDNS, error) {
	const script = `Get-DnsClientServerAddress -AddressFamily IPv4 -ErrorAction SilentlyContinue ` +
		`| Where-Object { $_.ServerAddresses.Count -gt 0 } ` +
		`| Select-Object InterfaceAlias,InterfaceIndex,ServerAddresses | ConvertTo-Json -Compress`
	out, err := runPowerShell(script)
	if err != nil {
		return nil, fmt.Errorf("查询网卡 DNS: %w", err)
	}
	text := strings.TrimSpace(out)
	if text == "" {
		return nil, nil
	}

	// 只有一条结果时 ConvertTo-Json 给的是对象而不是数组
	var raw []psDNS
	if strings.HasPrefix(text, "{") {
		var one psDNS
		if err := json.Unmarshal([]byte(text), &one); err != nil {
			return nil, fmt.Errorf("解析网卡 DNS: %w", err)
		}
		raw = []psDNS{one}
	} else if err := json.Unmarshal([]byte(text), &raw); err != nil {
		return nil, fmt.Errorf("解析网卡 DNS: %w", err)
	}

	var out2 []IfaceDNS
	for _, r := range raw {
		if r.InterfaceAlias == "" || len(r.ServerAddresses) == 0 {
			continue
		}
		out2 = append(out2, IfaceDNS{
			Name:  r.InterfaceAlias,
			Index: r.InterfaceIndex,
			DNS:   r.ServerAddresses,
		})
	}
	return out2, nil
}

func (s Snapshot) Apply(cfg NetConfig) error {
	for _, c := range ApplyCommands(s, cfg) {
		if err := runCommand(c); err != nil {
			return err
		}
	}
	return nil
}

// Restore 尽力还原全部配置：某一条失败（典型是规则本就不存在）不该阻断后面的还原。
func (s Snapshot) Restore(cfg NetConfig) error {
	var firstErr error
	for _, c := range RestoreCommands(s, cfg) {
		if err := runCommand(c); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
