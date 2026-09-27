//go:build windows

package client

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
	"golang.org/x/sys/windows"

	"github.com/antapp-cc/antapp-link/internal/update"
)

//go:embed assets/antapp.ico
var appIcon []byte

// UI 是主窗口加托盘。托盘用 walk 自带的 NotifyIcon，跟主窗口共用同一个消息循环 ——
// 换成独立的托盘库就得处理两个消息循环抢主线程的问题。
//
// 窗口布局照用户已经在 Pi 节点机上用惯的那个 OpenVPN 客户端来：
// 状态行 → 大日志区 → 分配 IP 与版本 → 三个按钮。
type UI struct {
	app  *App
	logs *LogBuffer
	icon *walk.Icon

	mw *walk.MainWindow
	ni *walk.NotifyIcon

	lblState   *walk.Label
	lblIP      *walk.Label
	lblTraffic *walk.Label
	lblVersion *walk.Label
	txtLog     *walk.TextEdit
	btnPrimary *walk.PushButton
	btnReconn  *walk.PushButton
	btnUpdate  *walk.PushButton
	btnHide    *walk.PushButton

	quitting    bool
	trayHinted  bool
	lastLogText string
	done        chan struct{}
	pending     *update.Manifest
}

// RunUI 阻塞运行图形界面，直到用户从托盘菜单退出。
func RunUI(app *App, logs *LogBuffer, dataDir string) error {
	iconPath, err := ensureIconFile(dataDir)
	if err != nil {
		return err
	}
	icon, err := walk.NewIconFromFile(iconPath)
	if err != nil {
		return fmt.Errorf("加载图标 %s: %w", iconPath, err)
	}

	u := &UI{app: app, logs: logs, icon: icon, done: make(chan struct{})}
	if err := u.build(); err != nil {
		return err
	}
	defer close(u.done)
	defer u.ni.Dispose()

	u.mw.Run()
	return nil
}

func (u *UI) build() error {
	if err := (MainWindow{
		AssignTo: &u.mw,
		Title:    "AntApp Link",
		Icon:     u.icon,
		// 默认 540x353，跟用户原来那个 OpenVPN 客户端窗口差不多大。
		// MinSize 必须比它小，否则 Windows 会把默认尺寸顶上去。
		MinSize: Size{Width: 480, Height: 300},
		Size:    Size{Width: 540, Height: 353},
		Layout:  VBox{Margins: Margins{Left: 10, Top: 10, Right: 10, Bottom: 10}, Spacing: 8},
		Children: []Widget{
			Label{
				AssignTo: &u.lblState,
				Text:     "当前状态: 未连接",
				Font:     Font{PointSize: 10},
			},
			TextEdit{
				AssignTo: &u.txtLog,
				ReadOnly: true,
				VScroll:  true,
				Font:     Font{Family: "Consolas", PointSize: 9},
			},
			Composite{
				Layout: HBox{MarginsZero: true},
				Children: []Widget{
					Label{AssignTo: &u.lblIP, Text: "分配 IP: —"},
					HSpacer{},
					Label{AssignTo: &u.lblTraffic, Text: "发送 — / 接收 —"},
					HSpacer{},
					Label{AssignTo: &u.lblVersion, Text: fmt.Sprintf("AntApp Link %s", Version)},
				},
			},
			Composite{
				Layout: HBox{MarginsZero: true, Spacing: 8},
				Children: []Widget{
					PushButton{AssignTo: &u.btnPrimary, Text: "连接", MinSize: Size{Width: 100}, OnClicked: u.onPrimary},
					PushButton{AssignTo: &u.btnReconn, Text: "重新连接", MinSize: Size{Width: 100}, OnClicked: u.onReconnect},
					PushButton{AssignTo: &u.btnUpdate, Text: "立即更新", MinSize: Size{Width: 100}, OnClicked: u.onUpdate},
					HSpacer{},
					PushButton{AssignTo: &u.btnHide, Text: "隐藏", MinSize: Size{Width: 100}, OnClicked: u.onHide},
				},
			},
		},
	}).Create(); err != nil {
		return fmt.Errorf("创建主窗口: %w", err)
	}

	// 关窗口只是收进托盘，不是退出 —— 否则隧道会跟着一起断
	u.mw.Closing().Attach(func(canceled *bool, _ walk.CloseReason) {
		if u.quitting {
			return
		}
		*canceled = true
		u.mw.Hide()
		// 第一次收进托盘时说一声。不说的话用户会以为程序已经关了，
		// 但隧道其实还在后台跑着。
		if !u.trayHinted {
			u.trayHinted = true
			_ = u.ni.ShowInfo("AntApp Link 仍在运行",
				"窗口已收进托盘，专线保持连接。\n双击托盘图标可以重新打开，右键菜单里可以退出。")
		}
	})

	if err := u.buildTray(); err != nil {
		return err
	}

	// 没有新版本时这个按钮不该占着位置
	u.btnUpdate.SetVisible(false)

	go u.refreshLoop()
	go u.autoCheckUpdate()
	u.refresh()
	return nil
}

func (u *UI) buildTray() error {
	ni, err := walk.NewNotifyIcon(u.mw)
	if err != nil {
		return fmt.Errorf("创建托盘图标: %w", err)
	}
	u.ni = ni
	if err := ni.SetIcon(u.icon); err != nil {
		return err
	}

	mShow := walk.NewAction()
	_ = mShow.SetText("显示主窗口")
	mShow.Triggered().Attach(u.showWindow)
	ni.ContextMenu().Actions().Add(mShow)
	ni.ContextMenu().Actions().Add(walk.NewSeparatorAction())

	mToggle := walk.NewAction()
	_ = mToggle.SetText("连接 / 断开")
	mToggle.Triggered().Attach(u.onPrimary)
	ni.ContextMenu().Actions().Add(mToggle)

	mImport := walk.NewAction()
	_ = mImport.SetText("从剪贴板导入连接码")
	mImport.Triggered().Attach(u.onImport)
	ni.ContextMenu().Actions().Add(mImport)

	mOpenDir := walk.NewAction()
	_ = mOpenDir.SetText("打开数据目录")
	mOpenDir.Triggered().Attach(u.onOpenDataDir)
	ni.ContextMenu().Actions().Add(mOpenDir)

	mUpdate := walk.NewAction()
	_ = mUpdate.SetText("检查更新")
	mUpdate.Triggered().Attach(u.onCheckUpdate)
	ni.ContextMenu().Actions().Add(mUpdate)
	ni.ContextMenu().Actions().Add(walk.NewSeparatorAction())

	mAuto := walk.NewAction()
	_ = mAuto.SetText("开机自启")
	_ = mAuto.SetChecked(AutostartEnabled())
	mAuto.Triggered().Attach(func() {
		go func() {
			if mAuto.Checked() {
				_ = DisableAutostart()
				mAuto.SetChecked(false)
			} else {
				_ = EnableAutostart()
				mAuto.SetChecked(true)
			}
		}()
	})
	ni.ContextMenu().Actions().Add(mAuto)
	ni.ContextMenu().Actions().Add(walk.NewSeparatorAction())

	mQuit := walk.NewAction()
	_ = mQuit.SetText("退出")
	mQuit.Triggered().Attach(u.quit)
	ni.ContextMenu().Actions().Add(mQuit)

	// 双击托盘图标回到主窗口
	ni.MouseDown().Attach(func(x, y int, button walk.MouseButton) {
		if button == walk.LeftButton {
			u.showWindow()
		}
	})

	return ni.SetVisible(true)
}

func (u *UI) showWindow() {
	u.mw.Show()
	u.mw.Activate()
}

func (u *UI) refreshLoop() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-u.done:
			return
		case <-ticker.C:
			// walk 要求所有控件操作回到主线程
			u.mw.Synchronize(u.refresh)
		}
	}
}

func (u *UI) refresh() {
	st := u.app.Status()
	configured := u.app.Configured()

	var state string
	switch {
	case !configured:
		state = "当前状态: 未配置连接码"
	case !st.Running:
		state = "当前状态: 未连接"
	case st.Online:
		state = "当前状态: 已连接"
	default:
		state = "当前状态: 连接中…"
	}
	if configured && st.Online {
		state += fmt.Sprintf("（延迟 %d ms）", st.RTT.Milliseconds())
	}
	if u.pending != nil {
		state += fmt.Sprintf("　·　有新版本 %s", u.pending.Version)
	}
	u.lblState.SetText(state)

	// 上次的错误比「服务端 x」更有用，就摆在状态行下面
	if configured && !st.Online && st.LastError != "" {
		u.lblIP.SetText("上次错误: " + truncateRunes(st.LastError, 46))
	} else if st.Online {
		u.lblIP.SetText(fmt.Sprintf("分配 IP: %s", st.TunnelIP))
	} else if configured {
		u.lblIP.SetText("服务端: " + st.Server)
	} else {
		u.lblIP.SetText("分配 IP: —")
	}

	// 流量按本次连接累计；断开重连会新开一个隧道实例，计数从零开始
	if st.Running {
		u.lblTraffic.SetText(fmt.Sprintf("发送 %s / 接收 %s",
			formatBytes(st.TxBytes), formatBytes(st.RxBytes)))
	} else {
		u.lblTraffic.SetText("发送 — / 接收 —")
	}

	// 按钮文字跟着状态走：没连接码时主按钮就是导入入口
	switch {
	case !configured:
		u.btnPrimary.SetText("导入连接码")
		u.btnPrimary.SetEnabled(true)
	case st.Running:
		u.btnPrimary.SetText("断开连接")
		u.btnPrimary.SetEnabled(true)
	default:
		u.btnPrimary.SetText("连接")
		u.btnPrimary.SetEnabled(true)
	}
	u.btnReconn.SetEnabled(configured && st.Running)

	if u.ni != nil {
		switch {
		case !configured:
			u.ni.SetToolTip("AntApp Link · 未配置连接码")
		case !st.Running:
			u.ni.SetToolTip("AntApp Link · 未连接")
		case st.Online:
			u.ni.SetToolTip(fmt.Sprintf("AntApp Link · 已连接 %s · %d ms", st.TunnelIP, st.RTT.Milliseconds()))
		default:
			u.ni.SetToolTip("AntApp Link · 连接中…")
		}
	}

	// 日志只在内容变了才重设，否则每秒都会把滚动位置弹回顶部
	text := strings.Join(u.logs.Tail(500), "\r\n")
	if text != u.lastLogText {
		u.lastLogText = text
		u.txtLog.SetText(text)

		// 滚到最后一行。这里刻意不用 SetTextSelection(n, n) 去「把光标挪到末尾」：
		// EM_SETSEL 之后 TextLength 确实报出空选择（诊断实测 165,165），但光标落在
		// 末尾时这个只读文本框会把整片日志画成蓝底高亮 —— 截图能看到，而选择状态
		// 读出来是正常的，所以只能绕开它。直接用滚动条消息，不碰选择。
		const (
			wmVScroll = 0x0115
			sbBottom  = 7
		)
		u.txtLog.SendMessage(wmVScroll, sbBottom, 0)
	}
}

// Connect/Disconnect 会做网络操作（探测、改路由、跑 PowerShell），不能卡住界面线程。
func (u *UI) onPrimary() {
	if !u.app.Configured() {
		u.onImport()
		return
	}
	go func() {
		if u.app.Status().Running {
			_ = u.app.Disconnect()
		} else {
			_ = u.app.Connect()
		}
		u.mw.Synchronize(u.refresh)
	}()
}

func (u *UI) onReconnect() {
	go func() {
		_ = u.app.Disconnect()
		_ = u.app.Connect()
		u.mw.Synchronize(u.refresh)
	}()
}

func (u *UI) onHide() { u.mw.Hide() }

// ---------- 在线更新 ----------

// autoCheckUpdate 在界面出来之后静默查一次，不打扰用户。
func (u *UI) autoCheckUpdate() {
	select {
	case <-u.done:
		return
	case <-time.After(8 * time.Second):
	}
	u.checkUpdate(false)
}

func (u *UI) onCheckUpdate() { go u.checkUpdate(true) }

func (u *UI) checkUpdate(manual bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	m, err := u.app.CheckUpdate(ctx)

	u.mw.Synchronize(func() {
		switch {
		case m != nil:
			u.pending = m
			u.btnUpdate.SetText("更新到 " + m.Version)
			u.btnUpdate.SetVisible(true)
			u.refresh()
			if manual {
				u.askUpdate()
			}
		case err != nil:
			if manual {
				walk.MsgBox(u.mw, "检查更新失败", err.Error(), walk.MsgBoxIconWarning)
			}
		default:
			if manual {
				walk.MsgBox(u.mw, "已是最新",
					fmt.Sprintf("当前版本 %s 已经是最新的。", Version), walk.MsgBoxIconInformation)
			}
		}
	})
}

func (u *UI) onUpdate() { u.askUpdate() }

func (u *UI) askUpdate() {
	m := u.pending
	if m == nil {
		return
	}
	notes := strings.TrimSpace(m.Notes)
	if notes != "" {
		notes = "\n\n更新说明：\n" + truncateRunes(notes, 300)
	}
	if walk.MsgBox(u.mw, "更新到 "+m.Version,
		fmt.Sprintf("当前版本：%s\n新版本：%s%s\n\n"+
			"下载并更新吗？更新会先断开连接、还原网络，随后自动重启。",
			Version, m.Version, notes),
		walk.MsgBoxYesNo|walk.MsgBoxIconQuestion) != walk.DlgCmdYes {
		return
	}

	u.btnUpdate.SetEnabled(false)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		defer cancel()

		newExe, err := u.app.DownloadUpdate(ctx, m)
		if err != nil {
			u.mw.Synchronize(func() {
				u.btnUpdate.SetEnabled(true)
				walk.MsgBox(u.mw, "下载更新包失败", err.Error(), walk.MsgBoxIconWarning)
			})
			return
		}

		u.mw.Synchronize(func() {
			if walk.MsgBox(u.mw, "下载完成",
				"更新包已通过校验。\n\n点「是」马上重启到新版本；点「否」下次启动时再更新。",
				walk.MsgBoxYesNo|walk.MsgBoxIconQuestion) != walk.DlgCmdYes {
				u.btnUpdate.SetEnabled(true)
				return
			}
			u.applyUpdate(newExe)
		})
	}()
}

// applyUpdate 已经回到主线程：替换文件、拉起新版，然后立刻退出自己 ——
// 此刻磁盘上的文件名已经归新版所有了。
func (u *UI) applyUpdate(newExe string) {
	if err := u.app.ApplyUpdate(newExe); err != nil {
		walk.MsgBox(u.mw, "更新失败", err.Error(), walk.MsgBoxIconWarning)
		u.btnUpdate.SetEnabled(true)
		return
	}
	u.quitting = true
	os.Exit(0)
}

// 卸载入口刻意不放在这个托盘菜单里：它紧挨着「退出」，而卸载是不可逆的，
// 误点代价太大。改由安装目录里的「卸载 AntApp Link」快捷方式承担。

// onImport 从剪贴板取连接码。
//
// 界面上做不了「粘贴一大段文本」的输入体验（那需要多行对话框），而连接码本来就是
// 从聊天窗口复制来的，直接读剪贴板是最顺手也最不容易出错的方式。
func (u *UI) onImport() {
	go func() {
		code, err := ReadClipboard()
		if err != nil {
			u.alert("读取剪贴板失败", err.Error())
			return
		}
		inv, err := LoadInvite(code)
		if err != nil {
			u.alert("剪贴板里没有可用的连接码",
				"请先复制整行 antapp:// 连接码（或整个 json 文件的内容），再点这个按钮。\n\n"+err.Error())
			return
		}
		if err := SaveInvite(u.app.DataDir(), inv); err != nil {
			u.alert("保存连接码失败", err.Error())
			return
		}
		if err := u.app.UpdateInvite(inv); err != nil {
			u.alert("切换连接码失败", err.Error())
			return
		}
		u.mw.Synchronize(func() {
			u.refresh()
			walk.MsgBox(u.mw, "连接码已导入",
				fmt.Sprintf("服务端：%s\n节点名：%s\n\n现在可以点「连接」了。", inv.Server, inv.Name),
				walk.MsgBoxIconInformation)
		})
	}()
}

func (u *UI) onOpenDataDir() {
	go func() {
		if err := openInExplorer(u.app.DataDir()); err != nil {
			u.alert("打开数据目录失败", err.Error())
		}
	}()
}

func (u *UI) quit() {
	if u.quitting {
		return
	}
	u.quitting = true
	go func() {
		// 先把网络还原干净，再关窗口
		_ = u.app.Disconnect()
		u.mw.Synchronize(func() { u.mw.Close() })
	}()
}

// alert 把错误挂在主窗口上弹出来，而不是变成一个找不到归属的孤立对话框。
func (u *UI) alert(title, msg string) {
	u.mw.Synchronize(func() {
		walk.MsgBox(u.mw, title, msg, walk.MsgBoxIconWarning)
	})
}

// openInExplorer 用 ShellExecute 打开目录。
//
// 不走 explorer.exe：它把目录打开之后经常返回非 0 退出码（实测在 C:\Program Files
// 下必定返回 1），拿退出码当判据就会把「其实已经打开了」报成失败。
// ShellExecute 是 Windows 打开文件/目录的规范入口，成败可信。
func openInExplorer(path string) error {
	file, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	return windows.ShellExecute(0, nil, file, nil, nil, windows.SW_SHOWNORMAL)
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// ensureIconFile 把内嵌的图标释放到数据目录，供 walk 按路径加载。
func ensureIconFile(dataDir string) (string, error) {
	if len(appIcon) == 0 {
		return "", errors.New("没有内嵌图标数据")
	}
	path := filepath.Join(dataDir, "antapp.ico")
	want := sha256.Sum256(appIcon)
	if existing, err := os.ReadFile(path); err == nil && sha256.Sum256(existing) == want {
		return path, nil
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, appIcon, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return path, nil
}
