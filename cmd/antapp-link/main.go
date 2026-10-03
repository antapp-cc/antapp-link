// Command antapp-link 是 AntApp Link 的 Windows 客户端。
//
// 默认启动主窗口 + 托盘图标。带参数运行时可以指定连接码，或走前台 / 联调模式方便排查。
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/antapp-cc/antapp-link/internal/client"
	"github.com/antapp-cc/antapp-link/internal/pki"
	"github.com/antapp-cc/antapp-link/internal/update"
)

func main() {
	os.Exit(run())
}

func run() int {
	var (
		codeArg    = flag.String("c", "", "连接码：单行 antapp:// 或 .antapp 文件路径（双击连接码文件时由系统传入）")
		dataArg    = flag.String("data", defaultRootDir(), "工作根目录（config/、logs/、data/ 都建在它下面）")
		once       = flag.Bool("once", false, "前台连接，不显示界面（Ctrl+C 退出）")
		noNetCfg   = flag.Bool("no-netcfg", false, "只建隧道、只配虚拟网卡，不改路由与 DNS（联调端口转发用）")
		orderTrace = flag.String("order-trace", "",
			"联调用：把每个进出隧道的包（方向/槽位/五元组/seq）记到该文件，用来断言同一内层流不跨外层连接")
		restoreNet = flag.Bool("restore-network", false, "只还原上次残留的网络配置后退出（卸载器调用）")
		showVer = flag.Bool("version", false, "显示版本")
	)
	flag.Parse()

	// 上一次更新会在程序旁边留下 .old，这次启动顺手清掉
	update.CleanupOld()

	if *showVer {
		fmt.Printf("antapp-link %s\n", client.Version)
		return 0
	}

	// 双击 .antapp 连接码文件时，Windows 把文件路径作为位置参数传进来。
	// 它和 -c 是同一件事，只是入口不同。
	inviteArg := *codeArg
	if inviteArg == "" && flag.NArg() > 0 {
		inviteArg = flag.Arg(0)
	}

	rootDir := *dataArg

	// 单实例闸门。桌面快捷方式点几次就起几个进程的话，托盘上会堆一排图标；
	// 更要紧的是几个实例会同时去抢同一块虚拟网卡和同一批路由，把网络搅乱。
	release, first, err := client.SingleInstance()
	if err != nil {
		client.ShowMessage("AntApp Link", "单实例检查失败："+err.Error())
		return 1
	}
	if !first {
		// 已经有实例在跑。如果这次是双击连接码文件进来的，先把码写进 config\，
		// 那个实例会发现文件变了并自动重载 —— 否则用户双击了却什么都没发生。
		if inviteArg != "" {
			importInviteQuietly(rootDir, inviteArg)
		}
		client.ActivateExisting()
		return 0
	}
	defer release()

	dataDir := rootDir
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		client.ShowMessage("AntApp Link", "创建工作目录失败："+err.Error())
		return 1
	}

	logger, logs, closeLog, err := client.NewFileLogger(dataDir)
	if err != nil {
		client.ShowMessage("AntApp Link", "初始化日志失败："+err.Error())
		return 1
	}
	defer closeLog()

	if inviteArg != "" {
		if _, err := client.ImportInviteFile(dataDir, inviteArg); err != nil {
			logger.Error("导入连接码失败", "err", err)
			client.ShowMessage("AntApp Link", "导入连接码失败："+err.Error())
			return 1
		}
		logger.Info("已导入连接码文件", "file", filepath.Base(inviteArg))
	}

	// 连接码不在这里加载：App 以「未配置」状态启动，由界面扫描 config\*.antapp
	// 决定用哪个 —— 一个直接用，多个让用户挑，客户端不记录上次连的是哪个。
	inv := pki.Invite{}

	var opts []client.Option
	if *noNetCfg {
		opts = append(opts, client.WithNoNetCfg())
		// 这是说明不是警告：-no-netcfg 只在命令行显式指定时才有，
		// 正常双击启动走不到这里。
		logger.Info("联调模式：只建隧道、只配网卡，不改路由与 DNS")
	}
	if *orderTrace != "" {
		opts = append(opts, client.WithOrderTrace(*orderTrace))
	}
	// 清掉上次在线更新留下的旧程序备份（更新时正在运行删不掉，新进程里删才删得动）
	update.CleanupOld()

	app := client.NewApp(inv, dataDir, logger, opts...)

	// 上次没干净退出的话，先把网络修回来再谈连接。
	// 联调模式刻意不碰网络，所以连自愈也一并跳过。
	if !*noNetCfg {
		if err := app.HealIfNeeded(); err != nil {
			logger.Warn("自愈未完全成功", "err", err)
		}
	}
	// 卸载器把客户端硬停之后借这个入口还原网络：上面那步自愈已经干完了活，
	// 这里只要在碰隧道之前退出。
	if *restoreNet {
		return 0
	}

	if *once {
		// 命令行一次性模式：config 里必须恰好有一个连接码文件（联调用，不做选择界面）
		cands := client.ListInviteFiles(dataDir)
		if len(cands) != 1 {
			fmt.Fprintf(os.Stderr, "config 里需要恰好一个 .antapp 连接码文件，实际 %d 个\n", len(cands))
			return 1
		}
		inv, err := client.LoadInviteFile(dataDir, cands[0])
		if err != nil {
			fmt.Fprintf(os.Stderr, "读取连接码失败: %v\n", err)
			return 1
		}
		if err := app.UpdateInvite(inv, cands[0]); err != nil {
			fmt.Fprintf(os.Stderr, "连接失败: %v\n", err)
			return 1
		}
		if err := app.Connect(); err != nil {
			fmt.Fprintf(os.Stderr, "连接失败: %v\n", err)
			return 1
		}
		fmt.Println("已连接，Ctrl+C 退出")
		client.WaitForInterrupt()
		_ = app.Disconnect()
		return 0
	}

	// 连不连、连哪个，由界面扫描 config\*.antapp 决定：一个直接连，多个弹窗让用户挑。
	if err := client.RunUI(app, logs, dataDir); err != nil {
		logger.Error("界面启动失败", "err", err)
		client.ShowMessage("AntApp Link", "界面启动失败："+err.Error())
		return 1
	}
	return 0
}

// importInviteQuietly 把连接码文件原样复制进 config\，不做界面反馈。
//
// 用在「客户端已经在跑，用户又双击了一个 .antapp 文件」这条路径上：
// 本进程只负责把文件落到正确位置，正在跑的那个实例会自己发现并弹出让用户确认。
func importInviteQuietly(rootDir, arg string) {
	if _, err := client.ImportInviteFile(rootDir, arg); err != nil {
		client.ShowMessage("AntApp Link", "导入连接码失败："+err.Error())
		return
	}
	client.ShowMessage("AntApp Link", "已导入连接码文件，正在运行的客户端会弹出选择窗口")
}

// defaultRootDir 是客户端的工作根目录：程序自己所在的目录（安装后就是
// C:\Program Files\AntApp Link）。config\、logs\、data\ 三个子目录都挂在它下面。
//
// 目录不可写时（程序被放在只读位置之类的）退回 %ProgramData%\AntAppLink。
func defaultRootDir() string {
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		if err := os.MkdirAll(dir, 0o700); err == nil {
			return dir
		}
	}
	if pd := os.Getenv("ProgramData"); pd != "" {
		return filepath.Join(pd, "AntAppLink")
	}
	return filepath.Join(os.Getenv("LOCALAPPDATA"), "AntAppLink")
}
