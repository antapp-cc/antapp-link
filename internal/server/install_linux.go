//go:build linux

package server

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	binaryPath  = "/usr/local/bin/antapp-linkd"
	daemonUnit  = "/etc/systemd/system/antapp-linkd.service"
	upUnit      = "/etc/systemd/system/antapp-link-up.service"
	updateUnit  = "/etc/systemd/system/antapp-link-update.service"
	updateTimer = "/etc/systemd/system/antapp-link-update.timer"
	sysctlFile  = "/etc/sysctl.d/99-antapp-link.conf"
	dnsConf     = "/etc/dnsmasq.d/antapp.conf"
)

// BBR + fq：Pi 节点上传流量占比高，换成 BBR 对丢包链路改善明显。
//
// 后面几项是多连接数据面的调优。deploy/install.sh 也会用 sysctl -w 逐项直写一遍
// （为了当场生效并回读校验），但那份重启就没了，持久化靠这份文件——两处必须一致。
var sysctlContent = strings.Join([]string{
	"net.ipv4.ip_forward=1",
	"net.core.default_qdisc=fq",
	"net.ipv4.tcp_congestion_control=bbr",
	"",
	"# 多连接并发：空闲后不重新起步、放宽单连接缓冲上限、开 MTU 探测",
	"net.ipv4.tcp_slow_start_after_idle=0",
	"net.core.rmem_max=16777216",
	"net.core.wmem_max=16777216",
	"net.ipv4.tcp_rmem=4096 87380 16777216",
	"net.ipv4.tcp_wmem=4096 65536 16777216",
	"net.ipv4.tcp_mtu_probing=1",
	"# tcp_fastopen 是占位：Go 标准库不发起 TFO，配了也不生效",
	"net.ipv4.tcp_fastopen=3",
	"",
}, "\n")

// Install 装成 systemd 服务。拆成两条 unit：up 管 netfilter（oneshot），
// daemon 管隧道（常驻），重启时互不拖累。
func Install(stdout, stderr io.Writer, args []string) int {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("c", DefaultConfigPath, "配置文件路径")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(stderr, "install 需要 root")
		return 1
	}

	// 先读配置并校验：DNS 中继配置要从这里取网关地址，配置不合法就别往下走
	cfg, err := LoadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}

	steps := []struct {
		desc string
		fn   func() error
	}{
		{"安装二进制到 " + binaryPath, installBinary},
		{"写 " + sysctlFile, func() error { return os.WriteFile(sysctlFile, []byte(sysctlContent), 0o644) }},
		{"写 " + dnsConf, func() error { return os.WriteFile(dnsConf, []byte(dnsRelayConfContent(cfg)), 0o644) }},
		{"启用 dnsmasq DNS 中继", enableDNSRelay},
		{"写 " + daemonUnit, func() error { return os.WriteFile(daemonUnit, []byte(daemonUnitContent(*cfgPath)), 0o644) }},
		{"写 " + upUnit, func() error { return os.WriteFile(upUnit, []byte(upUnitContent(*cfgPath)), 0o644) }},
		{"写 " + updateUnit, func() error { return os.WriteFile(updateUnit, []byte(updateUnitContent(*cfgPath)), 0o644) }},
		{"写 " + updateTimer, func() error { return os.WriteFile(updateTimer, []byte(updateTimerContent()), 0o644) }},
		{"systemctl daemon-reload", func() error { return runSystemctl("daemon-reload") }},
		{"systemctl enable", func() error {
			return runSystemctl("enable", "antapp-link-up.service", "antapp-linkd.service", "antapp-link-update.timer")
		}},
		{"systemctl start antapp-link-update.timer", func() error {
			return runSystemctl("start", "antapp-link-update.timer")
		}},
	}
	for _, s := range steps {
		if err := s.fn(); err != nil {
			fmt.Fprintf(stderr, "%s 失败: %v\n", s.desc, err)
			return 1
		}
		fmt.Fprintf(stdout, "OK  %s\n", s.desc)
	}

	fmt.Fprintf(stdout, "\n安装完成。启动顺序：\n")
	fmt.Fprintf(stdout, "  %s up -c %s      # 先配 netfilter\n", binaryPath, *cfgPath)
	fmt.Fprintf(stdout, "  systemctl start antapp-linkd     # 再起隧道\n")
	reportBBRState(stdout)
	return 0
}

// bbrReport 按内核当前的拥塞控制算法生成提示；bbr 时返回空串。
//
// 本入口只写 sysctl 持久化文件、不立即 apply（deploy/install.sh 才会当场 -w 并回读）。
// 不说清楚的话，「装完 BBR 没生效」会被当成安装失败。
func bbrReport(cc string) string {
	cc = strings.TrimSpace(cc)
	if cc == "bbr" {
		return ""
	}
	return fmt.Sprintf("提示：当前内核拥塞控制是 %s，不是 bbr；配置已写入 %s，重启后生效。\n"+
		"（若用 deploy/install.sh 安装，它会当场启用并回读，可以忽略本提示。）\n", cc, sysctlFile)
}

func reportBBRState(w io.Writer) {
	if b, err := os.ReadFile("/proc/sys/net/ipv4/tcp_congestion_control"); err == nil {
		fmt.Fprint(w, bbrReport(string(b)))
	}
}

func installBinary() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if exe == binaryPath {
		return nil
	}
	raw, err := os.ReadFile(exe)
	if err != nil {
		return err
	}
	// 先写临时文件再改名：正在运行的旧进程不会被写坏
	tmp := binaryPath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, binaryPath)
}

func daemonUnitContent(cfgPath string) string {
	return fmt.Sprintf(`[Unit]
Description=AntApp Link tunnel daemon
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s run -c %s
Restart=always
RestartSec=3
LimitNPROC=infinity

[Install]
WantedBy=multi-user.target
`, binaryPath, cfgPath)
}

func upUnitContent(cfgPath string) string {
	return fmt.Sprintf(`[Unit]
Description=AntApp Link netfilter rules
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=%s up -c %s
ExecStop=%s down -c %s

[Install]
WantedBy=multi-user.target
`, binaryPath, cfgPath, binaryPath, cfgPath)
}

// updateUnitContent 是自动更新的一次性任务：跑一次 update，查到新版本就换掉自己并
// 由 systemd 拉起新版。它由 timer 触发，不是常驻服务。
func updateUnitContent(cfgPath string) string {
	return fmt.Sprintf(`[Unit]
Description=AntApp Link self-update (one-shot)
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=%s update -c %s
`, binaryPath, cfgPath)
}

// updateTimerContent 每小时检查一次更新。
//
// 刻意不做「只在凌晨动手」的时间窗口：发版频率本来就低，早一小时晚一小时没差别，
// 而窗口反而会让「今天没更上」变成常态。Persistent=true 让错过的运行（机器关着、
// 时间没对上）在开机后补跑。
func updateTimerContent() string {
	return `[Unit]
Description=AntApp Link hourly update check

[Timer]
OnCalendar=hourly
Persistent=true
RandomizedDelaySec=300

[Install]
WantedBy=timers.target
`
}

func runSystemctl(args ...string) error {
	out, err := exec.Command("systemctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// dnsRelayConfContent 生成隧道 DNS 中继（dnsmasq）的配置。
//
// 关键是 filter-AAAA：隧道只接管 IPv4，把 AAAA 发给客户端，Chromium 等
// 浏览器内核会拿 v6 地址直连（完全绕开隧道）—— 全部死路，实测就是
// Pi Desktop 内嵌页面整批白屏的根因。bind-dynamic 让 dnsmasq 在开机时
// （antapp0 尚未建起）也能正常启动，隧道就绪后自动绑定网关地址。
func dnsRelayConfContent(cfg Config) string {
	return fmt.Sprintf(`# AntApp Link 隧道 DNS（由 antapp-linkd install 生成，可手改但会被覆盖）
# v4-only 隧道：必须 filter-AAAA，客户端拿到 AAAA 会绕开隧道直连 v6 死路
listen-address=%s
bind-dynamic
no-resolv
server=8.8.8.8
server=1.1.1.1
filter-AAAA
no-hosts
cache-size=1000
`, cfg.Tunnel.ServerIP)
}

// enableDNSRelay 启动并自启 dnsmasq，然后无条件 restart。
//
// restart 不能省：Debian/Ubuntu 上 apt install dnsmasq 会立即自启，那时 antapp.conf
// 还没写进去；而 start 对已 active 的 unit 是 no-op、不会重载配置。于是新写的
// listen-address + filter-AAAA 不生效，install 却报成功 —— 浏览器随后会拿到 AAAA
// 绕开隧道直连（实测内嵌页面白屏）。deploy/install.sh 早就补了这一步，Go 侧漏了。
//
// 失败只警告不返回错误：这一步排在写 unit 之前，让它中断安装会留下「二进制换了、
// dnsmasq 配置改了、但 unit 还没写」的半成品机器（deploy/install.sh 同样只是警告）。
func enableDNSRelay() error {
	if _, err := exec.LookPath("dnsmasq"); err != nil {
		fmt.Println("提示: 未安装 dnsmasq，隧道 DNS 中继未启用（deploy/install.sh 会自动安装）")
		return nil
	}
	if err := runSystemctl("enable", "dnsmasq"); err != nil {
		fmt.Printf("警告: 启用 dnsmasq 失败: %v\n", err)
		return nil
	}
	if err := runSystemctl("restart", "dnsmasq"); err != nil {
		fmt.Printf("警告: 重启 dnsmasq 失败: %v\n"+
			"      filter-AAAA 可能没生效 —— 浏览器会拿到 AAAA 地址绕开隧道直连\n", err)
	}
	return nil
}
