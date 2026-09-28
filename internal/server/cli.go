package server

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/antapp-cc/antapp-link/internal/pki"
)

const (
	DefaultConfigPath = "/etc/antapp-link/server.json"
	Version           = "0.1.0"
)

// RunCLI 是 antapp-linkd 的入口，返回进程退出码。
func RunCLI(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "init":
		return cmdInit(stdout, stderr, args[1:])
	case "invite":
		return cmdInvite(stdout, stderr, args[1:])
	case "up":
		return cmdUp(stdout, stderr, args[1:])
	case "down":
		return cmdDown(stdout, stderr, args[1:])
	case "run":
		return cmdRun(stdout, stderr, args[1:])
	case "install":
		return Install(stdout, stderr, args[1:])
	case "status":
		return cmdStatus(stdout, stderr, args[1:])
	case "version", "-v", "--version":
		fmt.Fprintf(stdout, "antapp-linkd %s\n", Version)
		return 0
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "未知子命令 %q\n\n", args[0])
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `AntApp Link 服务端

用法: antapp-linkd <子命令> [选项]

子命令:
  init                  生成 CA 与服务端证书，并在配置缺失时写入默认配置
  invite <客户端名>      签发客户端证书，输出连接码（文件 + 单行）
  install               安装为 systemd 服务（需要 root）
  up                    配置 netfilter 规则（需要 root）
  down                  清理 netfilter 规则（需要 root）
  run                   前台运行隧道（systemd 调用）
  status                显示隧道与端口转发状态
  version               显示版本

通用选项:
  -c <路径>              配置文件，默认 `+DefaultConfigPath+`
  -o <目录>              invite 的连接码输出目录，默认当前目录

典型流程:
  antapp-linkd init
  antapp-linkd invite pi-node-01 -o /root
  antapp-linkd install
  antapp-linkd up
  systemctl start antapp-linkd
`)
}

func newLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

func cmdInit(stdout, stderr io.Writer, args []string) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("c", DefaultConfigPath, "配置文件路径")
	listen := fs.String("listen", "", "隧道监听地址，形如 0.0.0.0:62233（不传则不改）")
	forward := fs.String("forward", "", "转发端口段，形如 31400-31409（不传则不改）")
	network := fs.String("network", "", "隧道网段，形如 10.10.0.0/24（不传则不改）")
	mode := fs.String("mode", "", "数据通道模式：tcp（默认）或 udp（不传则不改）")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	var ov Overrides
	ov.Listen = *listen
	ov.Network = *network
	if *mode != "" {
		if _, err := pki.ParseMode(*mode); err != nil {
			fmt.Fprintf(stderr, "--mode %v\n", err)
			return 2
		}
		ov.Mode = *mode
	}
	if *forward != "" {
		start, end, err := parsePortRange(*forward)
		if err != nil {
			fmt.Fprintf(stderr, "--forward %v\n", err)
			return 2
		}
		ov.ForwardStart, ov.ForwardEnd = start, end
	}

	_, statErr := os.Stat(*cfgPath)
	fresh := errors.Is(statErr, os.ErrNotExist)

	// 配置不存在就按默认值 + 覆盖项生成；已存在则只同步覆盖项，其余字段原样保留。
	// 这样安装脚本重跑是幂等的，也不会把用户手改过的字段冲掉。
	cfg := Default()
	if !fresh {
		loaded, err := LoadConfig(*cfgPath)
		if err != nil {
			fmt.Fprintf(stderr, "%v\n", err)
			return 1
		}
		cfg = loaded
	}
	cfg.ApplyOverrides(ov)

	if err := SaveConfig(*cfgPath, cfg); err != nil {
		fmt.Fprintf(stderr, "写配置失败: %v\n", err)
		return 1
	}
	if fresh {
		fmt.Fprintf(stdout, "已写入默认配置 %s\n", *cfgPath)
	} else {
		fmt.Fprintf(stdout, "已同步配置 %s\n", *cfgPath)
	}

	if err := pki.Init(cfg.PKIDir); err != nil {
		fmt.Fprintf(stderr, "初始化 PKI 失败: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "PKI 就绪      : %s\n", cfg.PKIDir)
	fmt.Fprintf(stdout, "隧道监听      : %s\n", cfg.Listen)
	fmt.Fprintf(stdout, "隧道网段      : %s（服务端 %s / 客户端 %s）\n",
		cfg.Tunnel.Network, cfg.Tunnel.ServerIP, cfg.Tunnel.ClientIP)
	fmt.Fprintf(stdout, "转发端口段    : %d-%d\n", cfg.ForwardPorts.Start, cfg.ForwardPorts.End)
	fmt.Fprintf(stdout, "数据通道      : %s\n", modeLabel(cfg.Mode))
	fmt.Fprintf(stdout, "\n下一步: antapp-linkd invite pi-node-01\n")
	return 0
}

// modeLabel 把配置里的模式变成人读的说明。
func modeLabel(mode string) string {
	if m, err := pki.ParseMode(mode); err == nil && m == pki.ModeUDP {
		return "UDP（控制走 TCP，数据包走 UDP + AES-GCM）"
	}
	return "TCP（TLS 1.3，数据与控制同一条连接）"
}

// parsePortRange 解析 31400-31409 这种形式。
func parsePortRange(s string) (int, int, error) {
	parts := strings.SplitN(s, "-", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("需要形如 31400-31409 的参数，实际 %q", s)
	}
	start, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	end, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil {
		return 0, 0, fmt.Errorf("端口不是数字: %q", s)
	}
	if start < 1 || end > 65535 || start > end {
		return 0, 0, fmt.Errorf("不是合法的端口区间: %d-%d", start, end)
	}
	return start, end, nil
}

func cmdInvite(stdout, stderr io.Writer, args []string) int {
	// 客户端名必须是第一个参数，剩下的才交给 flag 解析。
	//
	// Go 的 flag 包遇到第一个位置参数就停止解析，所以 `invite pi-node-01 -o /tmp -c conf`
	// 这种最自然的写法里，-o/-c/--server 会被整体当成位置参数而**静默失效**。
	// 把名字摘出来单独处理，才是用户期望的行为。
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(stderr, "用法: antapp-linkd invite <客户端名> [-c 配置] [-o 输出目录] [--server host:port]")
		return 2
	}
	name := args[0]

	fs := flag.NewFlagSet("invite", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("c", DefaultConfigPath, "配置文件路径")
	outDir := fs.String("o", ".", "连接码输出目录")
	serverAddr := fs.String("server", "", "服务端地址 host:port（默认自动探测公网 IP）")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}

	cfg, err := LoadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	// 保证 CA 一定存在：直接 invite 而没先 init 的用户不该拿到一个用不了的东西
	if err := pki.Init(cfg.PKIDir); err != nil {
		fmt.Fprintf(stderr, "初始化 PKI 失败: %v\n", err)
		return 1
	}

	addr := *serverAddr
	if addr == "" {
		ip, err := PublicIPv4()
		if err != nil {
			fmt.Fprintf(stderr, "%v\n", err)
			return 1
		}
		_, port, err := net.SplitHostPort(cfg.Listen)
		if err != nil {
			fmt.Fprintf(stderr, "配置里的 listen %q 不合法: %v\n", cfg.Listen, err)
			return 1
		}
		addr = net.JoinHostPort(ip, port)
	}

	inv, err := pki.Issue(cfg.PKIDir, name, addr, pki.TunnelParams{
		TunnelIP: cfg.Tunnel.ClientIP,
		Gateway:  cfg.Tunnel.ServerIP,
		Prefix:   cfg.PrefixLen(),
		MTU:      cfg.Tunnel.MTU,
		DNS:      cfg.DNS,
		Mode:     pki.TunnelMode(cfg.Mode),
	})
	if err != nil {
		fmt.Fprintf(stderr, "签发失败: %v\n", err)
		return 1
	}
	code, err := inv.Encode()
	if err != nil {
		fmt.Fprintf(stderr, "编码连接码失败: %v\n", err)
		return 1
	}
	raw, err := json.MarshalIndent(inv, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "序列化失败: %v\n", err)
		return 1
	}
	if err := os.MkdirAll(*outDir, 0o700); err != nil {
		fmt.Fprintf(stderr, "创建输出目录失败: %v\n", err)
		return 1
	}
	// 文件名固定成 pinode.antapp：它是给节点机双击导入用的，
	// 名字短一点更像「一张配置」，而不是一长串自动生成物。
	// 代价是同一目录下签多个节点会互相覆盖，要并存就 -o 到不同目录。
	path := filepath.Join(*outDir, "pinode.antapp")
	// 私钥含在里面，权限必须收紧
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		fmt.Fprintf(stderr, "写连接码文件失败: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "客户端名      : %s\n", name)
	fmt.Fprintf(stdout, "服务端        : %s\n", addr)
	fmt.Fprintf(stdout, "隧道地址      : %s/%d（网关 %s）\n", cfg.Tunnel.ClientIP, cfg.PrefixLen(), cfg.Tunnel.ServerIP)
	fmt.Fprintf(stdout, "MTU           : %d\n", cfg.Tunnel.MTU)
	fmt.Fprintf(stdout, "DNS           : %s\n", strings.Join(cfg.DNS, ", "))
	fmt.Fprintf(stdout, "转发端口      : %d-%d（只转 TCP）\n", cfg.ForwardPorts.Start, cfg.ForwardPorts.End)
	fmt.Fprintf(stdout, "连接码文件    : %s\n", path)
	fmt.Fprintf(stdout, "\n单行连接码（发给节点机导入；内含私钥，等同密码）:\n%s\n", code)
	return 0
}

func cmdUp(stdout, stderr io.Writer, args []string) int {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("c", DefaultConfigPath, "配置文件路径")
	force := fs.Bool("force", false, "转发端口被占用时仍然继续（切换端口段的场景）")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := LoadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	if err := Up(cfg, newLogger(stderr), *force); err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "netfilter 规则已就绪：%d-%d (TCP) -> %s\n",
		cfg.ForwardPorts.Start, cfg.ForwardPorts.End, cfg.Tunnel.ClientIP)
	return 0
}

func cmdDown(stdout, stderr io.Writer, args []string) int {
	fs := flag.NewFlagSet("down", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("c", DefaultConfigPath, "配置文件路径")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := LoadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	if err := Down(cfg, newLogger(stderr)); err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "netfilter 规则已清理")
	return 0
}

func cmdRun(stdout, stderr io.Writer, args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("c", DefaultConfigPath, "配置文件路径")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := LoadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := Run(ctx, cfg, newLogger(stderr)); err != nil {
		fmt.Fprintf(stderr, "隧道退出: %v\n", err)
		return 1
	}
	return 0
}

func cmdStatus(stdout, stderr io.Writer, args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("c", DefaultConfigPath, "配置文件路径")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := LoadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "配置文件    : %s\n", *cfgPath)
	fmt.Fprintf(stdout, "监听        : %s\n", cfg.Listen)
	fmt.Fprintf(stdout, "隧道网段    : %s（服务端 %s / 客户端 %s）\n",
		cfg.Tunnel.Network, cfg.Tunnel.ServerIP, cfg.Tunnel.ClientIP)
	fmt.Fprintf(stdout, "MTU         : %d\n", cfg.Tunnel.MTU)
	fmt.Fprintf(stdout, "DNS         : %s\n", strings.Join(cfg.DNS, ", "))
	fmt.Fprintf(stdout, "转发端口    : %d-%d（只转 TCP）\n", cfg.ForwardPorts.Start, cfg.ForwardPorts.End)
	fmt.Fprintf(stdout, "PKI 目录    : %s\n", cfg.PKIDir)

	if st, err := ReadStatus(); err == nil {
		fmt.Fprintf(stdout, "\n隧道进程    : 运行中（状态更新于 %s）\n", st.UpdatedAt)
		if st.Client != "" {
			fmt.Fprintf(stdout, "已接入节点  : %s（自 %s）\n", st.Client, st.ConnectedAt)
		} else {
			fmt.Fprintln(stdout, "已接入节点  : 无")
		}
	} else {
		fmt.Fprintln(stdout, "\n隧道进程    : 状态文件不存在，可能没在运行（systemctl status antapp-linkd）")
	}

	fmt.Fprintf(stdout, "\n端口转发（服务端应答，经隧道转给节点机）:\n")
	fmt.Fprintf(stdout, "  %d-%d/TCP → %s（同端口）\n",
		cfg.ForwardPorts.Start, cfg.ForwardPorts.End, cfg.Tunnel.ClientIP)
	return 0
}
