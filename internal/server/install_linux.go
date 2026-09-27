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
	binaryPath = "/usr/local/bin/antapp-linkd"
	daemonUnit = "/etc/systemd/system/antapp-linkd.service"
	upUnit     = "/etc/systemd/system/antapp-link-up.service"
	sysctlFile = "/etc/sysctl.d/99-antapp-link.conf"
)

// BBR + fq：Pi 节点上传流量占比高，换成 BBR 对丢包链路改善明显。
var sysctlContent = strings.Join([]string{
	"net.ipv4.ip_forward=1",
	"net.core.default_qdisc=fq",
	"net.ipv4.tcp_congestion_control=bbr",
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

	steps := []struct {
		desc string
		fn   func() error
	}{
		{"安装二进制到 " + binaryPath, installBinary},
		{"写 " + sysctlFile, func() error { return os.WriteFile(sysctlFile, []byte(sysctlContent), 0o644) }},
		{"写 " + daemonUnit, func() error { return os.WriteFile(daemonUnit, []byte(daemonUnitContent(*cfgPath)), 0o644) }},
		{"写 " + upUnit, func() error { return os.WriteFile(upUnit, []byte(upUnitContent(*cfgPath)), 0o644) }},
		{"systemctl daemon-reload", func() error { return runSystemctl("daemon-reload") }},
		{"systemctl enable", func() error {
			return runSystemctl("enable", "antapp-link-up.service", "antapp-linkd.service")
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
	return 0
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

func runSystemctl(args ...string) error {
	out, err := exec.Command("systemctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
