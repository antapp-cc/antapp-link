//go:build linux

package server

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Up 配置 netfilter。幂等：重复执行不会产生重复规则。
// force 为 true 时跳过端口冲突检查（切换端口段时会用到）。
func Up(cfg Config, logger *slog.Logger, force bool) error {
	if logger == nil {
		logger = slog.Default()
	}
	if err := CheckForwardPortsFree(cfg); err != nil {
		if !force {
			return err
		}
		logger.Warn("转发端口已被别的进程占用，按 --force 继续", connLogAttrs(err, nil)...)
	}
	if err := enableForwarding(); err != nil {
		return err
	}
	wan, err := defaultInterface()
	if err != nil {
		return err
	}
	logger.Info("默认出口网卡", "iface", wan)

	// 自定义链已存在时这条会报错，忽略即可
	_ = iptables("-t", "nat", "-N", ChainName)
	// 整链清空重铺：这条链完全归本项目管理，升级机器上遗留的历史规则
	// 也一并清掉。
	_ = iptables("-t", "nat", "-F", ChainName)

	for _, r := range Rules(cfg, wan) {
		if err := ensureRule(r); err != nil {
			return err
		}
	}
	logger.Info("netfilter 规则就绪",
		"forward_ports", fmt.Sprintf("%d-%d", cfg.ForwardPorts.Start, cfg.ForwardPorts.End),
		"client", cfg.Tunnel.ClientIP)
	return nil
}

// Down 清理规则。逐条删除并容忍「本就不存在」，这样没跑过 up 也能安全收场；
// 同时把跳到自定义链的跳转规则一并摘掉，避免端口段改过之后留下悬空跳转。
func Down(cfg Config, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	removeJumpsTo("nat", "PREROUTING", ChainName)
	_ = iptables("-t", "nat", "-F", ChainName)
	_ = iptables("-t", "nat", "-F", ChainName)
	_ = iptables("-t", "nat", "-X", ChainName)

	if wan, err := defaultInterface(); err == nil {
		for _, r := range Rules(cfg, wan) {
			if r.Chain == ChainName {
				continue // 已随自定义链一起删掉
			}
			args := []string{"-t", TableOrDefault(r.Table), "-D", r.Chain}
			args = append(args, r.Args...)
			_ = iptables(args...)
		}
	}
	logger.Info("netfilter 规则已清理")
	return nil
}

func ensureRule(r Rule) error {
	check := []string{"-t", TableOrDefault(r.Table), "-C", r.Chain}
	check = append(check, r.Args...)
	if err := iptables(check...); err == nil {
		return nil
	}
	add := []string{"-t", TableOrDefault(r.Table), "-A", r.Chain}
	add = append(add, r.Args...)
	return iptables(add...)
}

func removeJumpsTo(table, chain, target string) {
	out, err := exec.Command("iptables", "-w", "5", "-t", table, "-S", chain).Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		prefix := "-A " + chain + " "
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		args := strings.Fields(strings.TrimPrefix(line, prefix))
		if len(args) == 0 || args[len(args)-1] != target {
			continue
		}
		full := append([]string{"-w", "5", "-t", table, "-D", chain}, args...)
		_ = exec.Command("iptables", full...).Run()
	}
}

func iptables(args ...string) error {
	full := append([]string{"-w", "5"}, args...)
	out, err := exec.Command("iptables", full...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("iptables %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func enableForwarding() error {
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0o644); err != nil {
		return fmt.Errorf("打开 IPv4 转发: %w", err)
	}
	return nil
}

func defaultInterface() (string, error) {
	out, err := exec.Command("ip", "-4", "route", "show", "default").Output()
	if err != nil {
		return "", fmt.Errorf("查询默认路由: %w", err)
	}
	fields := strings.Fields(string(out))
	for i, f := range fields {
		if f == "dev" && i+1 < len(fields) {
			return fields[i+1], nil
		}
	}
	return "", errors.New("找不到默认出口网卡，云服可能没配默认路由")
}

// CheckForwardPortsFree 在接管端口段之前确认没有外部程序在监听。
//
// 本守护进程自己的转发器监听不算占用（升级场景下旧进程还持着端口）；
// 外部程序（典型是现网还在跑的 rinetd）占用会导致外部流量被它抢答、
// 节点机收不到 —— 老节点看起来「突然连不上」却没有任何报错。所以这里必须显式拦一下。
func CheckForwardPortsFree(cfg Config) error {
	ports, err := listeningPorts()
	if err != nil {
		// 拿不到监听列表不该阻止启动，只在日志层说明
		return nil
	}
	var busy []int
	for p := cfg.ForwardPorts.Start; p <= cfg.ForwardPorts.End; p++ {
		proc := ports[p]
		if proc != "" && proc != daemonProcessName {
			busy = append(busy, p)
		}
	}
	if len(busy) > 0 {
		return fmt.Errorf("转发端口 %v 已被外部程序占用（通常是现网还在跑的 rinetd）。"+
			"转发器将无法监听这些端口，外部流量进不了隧道；"+
			"迁移时请先停掉占用方，或先用另一段端口验证", busy)
	}
	return nil
}

// listeningPorts 返回当前 LISTEN 的 TCP 端口 → 监听进程名（需要 root；
// 拿不到进程名的行忽略）。
func listeningPorts() (map[int]string, error) {
	out, err := exec.Command("ss", "-lntpH").Output()
	if err != nil {
		return nil, err
	}
	ports := make(map[int]string)
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[0] != "LISTEN" {
			continue
		}
		_, portStr, err := net.SplitHostPort(fields[3])
		if err != nil {
			continue
		}
		p, err := strconv.Atoi(portStr)
		if err != nil {
			continue
		}
		ports[p] = listenerProcess(line)
	}
	return ports, nil
}

// listenerProcess 从 ss -p 的 users 字段里取监听进程名（可能为空）。
func listenerProcess(line string) string {
	i := strings.Index(line, `users:(("`)
	if i < 0 {
		return ""
	}
	rest := line[i+len(`users:(("`):]
	if j := strings.Index(rest, `"`); j >= 0 {
		return rest[:j]
	}
	return ""
}
