//go:build windows

package client

import (
	_ "embed"
	"fmt"
	"time"

	"fyne.io/systray"
)

//go:embed assets/antapp.ico
var trayIcon []byte

// RunTray 阻塞运行托盘界面，直到用户选择退出。
func (a *App) RunTray() error {
	systray.Run(a.onReady, func() {})
	return nil
}

func (a *App) onReady() {
	systray.SetIcon(trayIcon)
	systray.SetTitle("AntApp Link")
	systray.SetTooltip("AntApp Link 虚拟专线")

	mStatus := systray.AddMenuItem("未连接", "当前隧道状态")
	mStatus.Disable()
	systray.AddSeparator()
	mToggle := systray.AddMenuItem("连接", "连接或断开隧道")
	mImport := systray.AddMenuItem("从剪贴板导入连接码", "先复制整行 antapp:// 连接码，再点这里")
	mOpenDir := systray.AddMenuItem("打开数据目录", "连接码、日志与状态文件都在这里")
	mAutostart := systray.AddMenuItemCheckbox("开机自启", "登录后自动连接", AutostartEnabled())
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("退出", "断开并退出")

	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for range ticker.C {
			a.refreshTray(mStatus, mToggle)
		}
	}()
	a.refreshTray(mStatus, mToggle)

	for {
		select {
		case <-mToggle.ClickedCh:
			if a.Status().Running {
				if err := a.Disconnect(); err != nil {
					a.log.Error("断开失败", "err", err)
				}
			} else if err := a.Connect(); err != nil {
				a.log.Error("连接失败", "err", err)
			}
			a.refreshTray(mStatus, mToggle)

		case <-mImport.ClickedCh:
			a.importFromClipboard()

		case <-mOpenDir.ClickedCh:
			if err := runCommand(Command{"explorer", []string{a.dataDir}}); err != nil {
				a.log.Error("打开数据目录失败", "err", err)
			}

		case <-mAutostart.ClickedCh:
			if mAutostart.Checked() {
				mAutostart.Uncheck()
				if err := DisableAutostart(); err != nil {
					a.log.Error("关闭开机自启失败", "err", err)
				}
			} else {
				mAutostart.Check()
				if err := EnableAutostart(); err != nil {
					a.log.Error("开启开机自启失败", "err", err)
				}
			}

		case <-mQuit.ClickedCh:
			if err := a.Disconnect(); err != nil {
				a.log.Error("退出时还原网络失败，状态文件已保留，下次启动会重试", "err", err)
			}
			systray.Quit()
			return
		}
	}
}

func (a *App) importFromClipboard() {
	code, err := ReadClipboard()
	if err != nil {
		a.log.Error("读剪贴板失败", "err", err)
		return
	}
	inv, err := LoadInvite(code)
	if err != nil {
		a.log.Error("剪贴板里没有可用的连接码", "err", err)
		return
	}
	if err := SaveInvite(a.dataDir, inv); err != nil {
		a.log.Error("保存连接码失败", "err", err)
		return
	}
	if err := a.UpdateInvite(inv); err != nil {
		a.log.Error("切换连接码失败", "err", err)
		return
	}
	a.log.Info("已导入连接码", "server", inv.Server, "name", inv.Name)
}

func (a *App) refreshTray(status, toggle *systray.MenuItem) {
	st := a.Status()
	switch {
	case !st.Running:
		if st.LastError != "" {
			status.SetTitle("未连接 · " + truncateRunes(st.LastError, 32))
		} else {
			status.SetTitle("未连接")
		}
		toggle.SetTitle("连接")
	case st.Online:
		status.SetTitle(fmt.Sprintf("已连接 %s · %d ms", st.TunnelIP, st.RTT.Milliseconds()))
		toggle.SetTitle("断开")
	default:
		status.SetTitle("连接中…")
		toggle.SetTitle("断开")
	}
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
