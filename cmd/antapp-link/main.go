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
)

func main() {
	os.Exit(run())
}

func run() int {
	var (
		codeArg  = flag.String("c", "", "连接码：单行 antapp:// 或 json/txt 文件路径")
		dataArg  = flag.String("data", defaultDataDir(), "数据目录")
		once     = flag.Bool("once", false, "前台连接，不显示界面（Ctrl+C 退出）")
		noNetCfg = flag.Bool("no-netcfg", false, "只建隧道、只配虚拟网卡，不改路由与 DNS（联调端口转发用）")
		showVer  = flag.Bool("version", false, "显示版本")
	)
	flag.Parse()

	if *showVer {
		fmt.Printf("antapp-link %s\n", client.Version)
		return 0
	}

	dataDir := *dataArg
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		client.ShowMessage("AntApp Link", "创建数据目录失败："+err.Error())
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

func defaultDataDir() string {
	if pd := os.Getenv("ProgramData"); pd != "" {
		return filepath.Join(pd, "AntAppLink")
	}
	return filepath.Join(os.Getenv("LOCALAPPDATA"), "AntAppLink")
}
