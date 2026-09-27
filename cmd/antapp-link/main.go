// Command antapp-link 是 AntApp Link 的 Windows 客户端。
//
// 正常使用不需要命令行：导入连接码之后双击即可，之后都在托盘里操作。
// 带参数运行时可以指定连接码，或走前台模式方便排查。
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/antapp-cc/antapp-link/internal/client"
)

func main() {
	os.Exit(run())
}

func run() int {
	var (
		codeArg = flag.String("c", "", "连接码：单行 antapp:// 或 json/txt 文件路径")
		dataArg = flag.String("data", defaultDataDir(), "数据目录")
		once    = flag.Bool("once", false, "前台连接，不显示托盘（Ctrl+C 退出）")
		showVer = flag.Bool("version", false, "显示版本")
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

	logger, closeLog, err := client.NewFileLogger(dataDir)
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

	inv, err := client.LoadSavedInvite(dataDir)
	if err != nil {
		logger.Error("没有可用的连接码", "err", err)
		client.ShowMessage("AntApp Link",
			"还没有导入连接码。\n\n"+
				"做法：复制整行 antapp:// 连接码，然后在托盘菜单里选「从剪贴板导入连接码」。\n"+
				"也可以命令行导入：antapp-link.exe -c \"antapp://...\"")
		return 1
	}

	app := client.NewApp(inv, dataDir, logger)

	// 上次没干净退出的话，先把网络修回来再谈连接
	if err := app.HealIfNeeded(); err != nil {
		logger.Warn("自愈未完全成功", "err", err)
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

	if err := app.Connect(); err != nil {
		// 连不上也要留在托盘里，用户可以改连接码或看日志
		logger.Warn("自动连接失败，托盘仍可用", "err", err)
	}
	if err := app.RunTray(); err != nil {
		logger.Error("托盘启动失败", "err", err)
		client.ShowMessage("AntApp Link", "托盘启动失败："+err.Error())
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
