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
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if _, err := os.Stat(*cfgPath); errors.Is(err, os.ErrNotExist) {
		if err := writeDefaultConfig(*cfgPath); err != nil {
			fmt.Fprintf(stderr, "写默认配置失败: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "已写入默认配置 %s\n", *cfgPath)
	}

	cfg, err := LoadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	if err := pki.Init(cfg.PKIDir); err != nil {
		fmt.Fprintf(stderr, "初始化 PKI 失败: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "PKI 就绪      : %s\n", cfg.PKIDir)
	fmt.Fprintf(stdout, "隧道端口      : %s\n", cfg.Listen)
	fmt.Fprintf(stdout, "转发端口段    : %d-%d\n", cfg.ForwardPorts.Start, cfg.ForwardPorts.End)
	fmt.Fprintf(stdout, "\n下一步: antapp-linkd invite pi-node-01\n")
	return 0
}

func writeDefaultConfig(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(Default(), "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o600)
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
	path := filepath.Join(*outDir, fmt.Sprintf("antapp-node-%s.json", name))
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
	fmt.Fprintf(stdout, "转发端口      : %d-%d（TCP + UDP）\n", cfg.ForwardPorts.Start, cfg.ForwardPorts.End)
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
	fmt.Fprintf(stdout, "netfilter 规则已就绪：%d-%d (TCP+UDP) -> %s\n",
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
	fmt.Fprintf(stdout, "转发端口    : %d-%d（TCP + UDP）\n", cfg.ForwardPorts.Start, cfg.ForwardPorts.End)
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

	fmt.Fprintf(stdout, "\n端口转发规则（内核 DNAT）:\n")
	for _, r := range DNATRules(cfg) {
		fmt.Fprintf(stdout, "  %s\n", r.String())
	}
	return 0
}
