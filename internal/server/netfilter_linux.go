//go:build linux

package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
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
	// 整链清空重铺（这条链归本项目管理）。
	_ = iptables("-t", "nat", "-F", ChainName)

	// 再按形态清一遍历史规则：Rules() 全在内置链上，上面那次 -F 清的自定义链早就
	// 没人用了，不补这一步的话跨版本升级时旧规则永远残留（实测入向那条无状态全放行
	// 会让本次的 --ctstate 收紧失效）。不存在就忽略。
	for _, r := range legacyRules(cfg) {
		args := []string{"-t", TableOrDefault(r.Table), "-D", r.Chain}
		args = append(args, r.Args...)
		_ = iptables(args...)
	}

	// 换段时先把上一段留下的 INPUT 放行清掉：Up 只 ensureRule 新段，旧段会永久
	// 对公网开着。记录由上一次 Up 写进 /run。
	currentRange := fmt.Sprintf("%d:%d", cfg.ForwardPorts.Start, cfg.ForwardPorts.End)
	if prev := readForwardRange(); prev != "" && prev != currentRange {
		if err := removeInputRange(prev); err != nil {
			return fmt.Errorf("清理旧转发端口段的放行规则: %w", err)
		}
		logger.Info("已清理旧转发端口段的放行规则", "range", prev)
	}
	rules := Rules(cfg, wan)
	// 按当前配置的**精确形态**清一遍旧版无标记规则：账本损坏或跨版本升级时，
	// 旧规则既不带 marker、也不在账本里，只能靠形状认。只删与本次完全相同的形状
	// （不忽略 -o），不会波及别的网卡或别的配置。删除失败忽略（规则可能本来就不在）。
	for _, r := range rules {
		args := append([]string{"-t", TableOrDefault(r.Table), "-D", r.Chain}, r.Args...)
		_ = iptables(args...)
	}
	for i := range rules {
		rules[i].Args = append(rules[i].Args, "-m", "comment", "--comment", "AntAppLink")
	}
	ledger, err := loadNetfilterLedger()
	if err != nil {
		// 账本损坏时重建：旧内容已经不可用，继续用空账本能让 Up 恢复服务，
		// 否则会出现「规则删不掉也起不来」。旧配置留下的无标记规则会因此
		// 失去账本归属，只能靠模板形状或 marker 认领，这里明确提示。
		logger.Warn("防火墙恢复账本损坏，按空账本重建；旧配置留下的无标记规则可能无法自动清理", "err", err)
		ledger = nil
	}
	for _, r := range rules {
		found := false
		for _, old := range ledger {
			if sameRule(old, r, false) {
				found = true
				break
			}
		}
		if !found {
			ledger = append(ledger, r)
		}
	}
	if err := saveNetfilterLedger(ledger); err != nil {
		// 账本只是 Down 的辅助线索：新规则都带 marker，账本写不了也能靠标记清理。
		// 在只读 /run 的容器里，为此让 up 完全起不来代价更大。
		logger.Warn("预写防火墙恢复账本失败，继续按注释标记铺规则", "err", err)
	}
	for _, r := range rules {
		if err := ensureRule(r); err != nil {
			return err
		}
	}
	if err := writeForwardRange(currentRange); err != nil {
		logger.Warn("记录转发端口段失败（换段时旧规则可能残留）", "err", err)
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
	ledger, err := loadNetfilterLedger()
	if err != nil {
		// 账本坏了不能挡住清理：按标记和模板继续，清完再删账本。
		logger.Warn("防火墙恢复账本损坏，按标记和模板清理", "err", err)
		ledger = nil
	}
	templates := Rules(cfg, "")
	if prev := readForwardRange(); prev != "" {
		templates = append(templates, Rule{Chain: "INPUT", Args: []string{"-p", "tcp", "--dport", prev, "-j", "ACCEPT"}})
	}
	var errs []error
	for _, tc := range [][2]string{{"nat", "PREROUTING"}, {"nat", "POSTROUTING"}, {"filter", "INPUT"}, {"filter", "FORWARD"}, {"mangle", "FORWARD"}} {
		actual, err := listNetfilterRules(tc[0], tc[1])
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, r := range actual {
			// 账本丢失时靠注释标记认自己的规则；模板只对旧版无标记形态生效。
			owned := ruleHasMarker(r) ||
				(tc[0] == "nat" && tc[1] == "PREROUTING" && len(r.Args) == 2 && r.Args[0] == "-j" && r.Args[1] == ChainName)
			for _, saved := range ledger {
				if sameRule(r, saved, false) {
					owned = true
					break
				}
			}
			for _, old := range templates {
				if sameRule(r, old, true) {
					owned = true
					break
				}
			}
			if !owned {
				continue
			}
			args := append([]string{"-t", tc[0], "-D", r.Chain}, r.Args...)
			if err := iptables(args...); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if _, err := listNetfilterRules("nat", ChainName); err == nil {
		if err := iptables("-t", "nat", "-F", ChainName); err != nil {
			errs = append(errs, err)
		}
		if err := iptables("-t", "nat", "-X", ChainName); err != nil {
			errs = append(errs, err)
		}
	} else if !isMissingChainError(err.Error()) {
		errs = append(errs, err)
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("netfilter 清理未完成，保留恢复账本: %w", err)
	}
	for _, file := range []string{forwardRangePath, forwardRangePath + ".rules.json"} {
		if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	logger.Info("netfilter 规则已清理")
	return nil
}

func loadNetfilterLedger() ([]Rule, error) {
	raw, err := os.ReadFile(forwardRangePath + ".rules.json")
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rules []Rule
	if err := json.Unmarshal(raw, &rules); err != nil {
		return nil, fmt.Errorf("防火墙恢复账本损坏: %w", err)
	}
	return rules, nil
}

func saveNetfilterLedger(rules []Rule) error {
	raw, err := json.Marshal(rules)
	if err != nil {
		return err
	}
	file := forwardRangePath + ".rules.json"
	if err := os.MkdirAll(filepath.Dir(file), 0755); err != nil {
		return err
	}
	if err := os.WriteFile(file+".tmp", raw, 0600); err != nil {
		return err
	}
	return os.Rename(file+".tmp", file)
}

func listNetfilterRules(table, chain string) ([]Rule, error) {
	raw, err := exec.Command("iptables", "-w", "5", "-t", table, "-S", chain).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("读取 iptables %s/%s: %w: %s", table, chain, err, strings.TrimSpace(string(raw)))
	}
	return parseNetfilterRules(string(raw), table, chain), nil
}

// removeInputRange 按实际参数撤销新旧版本的放行规则，保留其他程序的注释规则。
func removeInputRange(portRange string) error {
	rules, err := listNetfilterRules("filter", "INPUT")
	if err != nil {
		return err
	}
	old := Rule{Chain: "INPUT", Args: []string{"-p", "tcp", "--dport", portRange, "-j", "ACCEPT"}}
	marked := Rule{Chain: old.Chain, Args: append(append([]string(nil), old.Args...), "-m", "comment", "--comment", "AntAppLink")}
	var errs []error
	for _, r := range rules {
		if sameRule(r, old, false) || sameRule(r, marked, false) {
			args := append([]string{"-t", "filter", "-D", r.Chain}, r.Args...)
			if err := iptables(args...); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
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
