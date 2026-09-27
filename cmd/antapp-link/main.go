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
		codeArg  = flag.String("c", "", "连接码：单行 antapp:// 或 json/txt 文件路径")
		dataArg  = flag.String("data", defaultRootDir(), "工作根目录（config/、logs/、data/ 都建在它下面）")
		once     = flag.Bool("once", false, "前台连接，不显示界面（Ctrl+C 退出）")
		noNetCfg = flag.Bool("no-netcfg", false, "只建隧道、只配虚拟网卡，不改路由与 DNS（联调端口转发用）")
		showVer  = flag.Bool("version", false, "显示版本")
	)
	flag.Parse()

	// 上一次更新会在程序旁边留下 .old，这次启动顺手清掉
	update.CleanupOld()

	if *showVer {
		fmt.Printf("antapp-link %s\n", client.Version)
		return 0
	}

	// 单实例闸门。桌面快捷方式点几次就起几个进程的话，托盘上会堆一排图标；
	// 更要紧的是几个实例会同时去抢同一块虚拟网卡和同一批路由，把网络搅乱。
	// 已经有实例在跑就把它叫到前台，本进程安静退出。
	release, first, err := client.SingleInstance()
	if err != nil {
		client.ShowMessage("AntApp Link", "单实例检查失败："+err.Error())
		return 1
	}
	if !first {
		client.ActivateExisting()
		return 0
	}
	defer release()

	dataDir := *dataArg
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

	if *codeArg != "" {
		inv, err := client.LoadInvite(*codeArg)
		if err != nil {
			logger.Error("连接码无法解析", "err", err)
			client.ShowMessage("AntApp Link", "连接码无法解析：\n"+err.Error())
			return 1
		}
		if err := client.SaveInvite(dataDir, inv); err != nil {
			logger.Error("保存连接码失败", "err", err)
			client.ShowMessage("AntApp Link", "保存连接码失败："+err.Error())
			return 1
		}
		logger.Info("已导入连接码", "server", inv.Server, "name", inv.Name)
	}

	// 没有连接码不是错误：界面照样起来，引导用户在界面上导入。
	// 装完之后直接弹个框退出，用户等于看不到这个软件。
	inv, err := client.LoadSavedInvite(dataDir)
	if err != nil {
		logger.Warn("还没有连接码，等用户在界面上导入", "err", err)
		inv = pki.Invite{}
	}

	var opts []client.Option
	if *noNetCfg {
		opts = append(opts, client.WithNoNetCfg())
		logger.Warn("联调模式：只建隧道，不改路由与 DNS")
	}
	app := client.NewApp(inv, dataDir, logger, opts...)

	// 上次没干净退出的话，先把网络修回来再谈连接。
	// 联调模式刻意不碰网络，所以连自愈也一并跳过。
	if !*noNetCfg {
		if err := app.HealIfNeeded(); err != nil {
			logger.Warn("自愈未完全成功", "err", err)
		}
	}

	if *once {
		if err := app.Connect(); err != nil {
			fmt.Fprintf(os.Stderr, "连接失败: %v\n", err)
			return 1
		}
		fmt.Println("已连接，Ctrl+C 退出")
		client.WaitForInterrupt()
		_ = app.Disconnect()
		return 0
	}

	if app.Configured() {
		if err := app.Connect(); err != nil {
			// 连不上也要把界面显示出来，用户可以改连接码或看日志
			logger.Warn("自动连接失败，界面仍可用", "err", err)
		}
	}
	if err := client.RunUI(app, logs, dataDir); err != nil {
		logger.Error("界面启动失败", "err", err)
		client.ShowMessage("AntApp Link", "界面启动失败："+err.Error())
		return 1
	}
	return 0
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
