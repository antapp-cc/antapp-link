//go:build windows

package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
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
func Capture(serverIP string) (Snapshot, error) {
	gateway, ifIndex, err := captureDefaultRoute()
	if err != nil {
		return Snapshot{}, err
	}
	nextHop, err := resolveServerNextHop(serverIP)
	if err != nil {
		return Snapshot{}, err
	}
	ifaces, err := captureDNS()
	if err != nil {
		return Snapshot{}, err
	}
	return newSnapshot(gateway, ifIndex, nextHop, ifaces), nil
}

// resolveServerNextHop 查到达云服所用的下一跳。
//
// 返回空表示云服就在直连网段内（本地拿 WSL 当服务端时就如此）：这时只靠现成的
// 直连路由就能绕开隧道，不需要、也**不能**加 /32 —— 加了会用默认网关覆盖掉那条
// 更具体的直连路由，把承载隧道的连接自己掐死。
func resolveServerNextHop(serverIP string) (string, error) {
	script := fmt.Sprintf(
		`$r = Find-NetRoute -RemoteIPAddress '%s' -ErrorAction SilentlyContinue `+
			`| Where-Object { $_.NextHop -and $_.NextHop -ne '0.0.0.0' } | Select-Object -First 1; `+
			`if ($r) { $r | Select-Object NextHop | ConvertTo-Json -Compress }`, serverIP)
	out, err := runPowerShell(script)
	if err != nil {
		return "", fmt.Errorf("查询到 %s 的路由: %w", serverIP, err)
	}
	text := strings.TrimSpace(out)
	if text == "" {
		return "", nil
	}
	var r struct {
		NextHop string `json:"NextHop"`
	}
	if err := json.Unmarshal([]byte(text), &r); err != nil {
		return "", fmt.Errorf("解析路由 %q: %w", text, err)
	}
	return r.NextHop, nil
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

// ConfigureAdapter 只把隧道网卡本身配起来（地址、MTU、接口跃点），不碰路由和 DNS。
//
// 单独暴露出来是给联调模式用的：只验端口转发时，也需要网卡上有 10.10.0.2，
// 这样内核收到隧道里过来的包才认得出是给自己的；但路由和 DNS 一概不动，
// 用户正在用的网络完全不受影响。
func ConfigureAdapter(cfg NetConfig) error {
	for _, c := range PrepareCommands(cfg) {
		if err := runCommand(c); err != nil {
			return err
		}
	}
	return nil
}

// Apply 分两阶段接管网络：先把网卡配好，拿到它的接口索引，再挂路由。
// 路由必须显式绑定接口索引，否则 Windows 可能把它挂到别的网卡上去
// （实测会把 10.10.0.1 挂到 WLAN 上，流量于是根本没进隧道）。
func (s Snapshot) Apply(cfg NetConfig) error {
	if err := ConfigureAdapter(cfg); err != nil {
		return err
	}
	ifIndex, err := interfaceIndex(cfg.AdapterName)
	if err != nil {
		return err
	}
	for _, c := range RouteCommands(s, cfg, ifIndex) {
		if err := runCommand(c); err != nil {
			return err
		}
	}

	// 智能分流：国内网段走原网关直连，其余才进隧道。
	//
	// 好处是服务端出口出问题时国内网络照常可用，不会「连上就没有网」。
	if splitRoutesSupported(s) {
		routes := CNRoutes()
		cmds := splitRouteCommands(routes, s.DefaultIfIndex, s.DefaultGateway, true)
		ok := 0
		for _, c := range cmds {
			if err := runCommand(c); err == nil {
				ok++
			}
		}
		// 只报结论。写了多少条是实现细节，用户不需要看，而且会把日志撑出好几行。
		if ok == len(cmds) {
			logf("智能分流已启用：国内流量直连，其余走隧道")
		} else {
			logf("智能分流部分生效：%d 条直连路由没写进去（这些网段会走隧道，不影响可用）",
				len(cmds)-ok)
		}
	}
	return nil
}

func interfaceIndex(name string) (int, error) {
	script := fmt.Sprintf(
		`$a = Get-NetAdapter -Name '%s' -ErrorAction SilentlyContinue | Select-Object -First 1; `+
			`if ($a) { $a.ifIndex }`, name)
	out, err := runPowerShell(script)
	if err != nil {
		return 0, fmt.Errorf("查询网卡 %s 的接口索引: %w", name, err)
	}
	text := strings.TrimSpace(out)
	idx, err := strconv.Atoi(text)
	if err != nil {
		return 0, fmt.Errorf("网卡 %q 的接口索引不是数字（拿到 %q）—— 网卡可能没建起来", name, text)
	}
	return idx, nil
}

// Restore 尽力还原全部配置：某一条失败（典型是规则本就不存在）不该阻断后面的还原。
func (s Snapshot) Restore(cfg NetConfig) error {
	var firstErr error
	for _, c := range RestoreCommands(s, cfg) {
		if err := runCommand(c); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	// 国内直连路由也要撤掉，否则断开后它们还留在表里
	if splitRoutesSupported(s) {
		for _, c := range splitRouteCommands(CNRoutes(), s.DefaultIfIndex, s.DefaultGateway, false) {
			_ = runCommand(c)
		}
	}
	return firstErr
}
