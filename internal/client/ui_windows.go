//go:build windows

package client

import (
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
)

//go:embed assets/antapp.ico
var appIcon []byte

// UI 是主窗口加托盘。托盘用 walk 自带的 NotifyIcon，跟主窗口共用同一个消息循环 ——
// 换成独立的托盘库就得处理两个消息循环抢主线程的问题。
type UI struct {
	app  *App
	logs *LogBuffer
	icon *walk.Icon

	mw *walk.MainWindow
	ni *walk.NotifyIcon

	lblState  *walk.Label
	lblDetail *walk.Label
	btnToggle *walk.PushButton
	txtLog    *walk.TextEdit
	chkAuto   *walk.CheckBox

	quitting    bool
	lastLogText string
	done        chan struct{}
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
		MinSize:  Size{Width: 520, Height: 400},
		Size:     Size{Width: 620, Height: 500},
		Layout:   VBox{Margins: Margins{Left: 12, Top: 12, Right: 12, Bottom: 12}, Spacing: 10},
		Children: []Widget{
			Composite{
				Layout: Grid{Columns: 2, Spacing: 8},
				Children: []Widget{
					Label{Text: "状态"},
					Label{AssignTo: &u.lblState, Text: "未配置"},
					Label{Text: "明细"},
					Label{AssignTo: &u.lblDetail, Text: "—"},
				},
			},
			Composite{
				Layout: HBox{Spacing: 8},
				Children: []Widget{
					PushButton{AssignTo: &u.btnToggle, Text: "连接", MinSize: Size{Width: 96}, OnClicked: u.onToggle},
					PushButton{Text: "从剪贴板导入连接码", OnClicked: u.onImport},
					PushButton{Text: "打开数据目录", OnClicked: u.onOpenDataDir},
					HSpacer{},
					CheckBox{AssignTo: &u.chkAuto, Text: "开机自启", OnCheckedChanged: u.onAutostart},
				},
			},
			Label{Text: "运行日志"},
			TextEdit{
				AssignTo: &u.txtLog,
				ReadOnly: true,
				VScroll:  true,
				Font:     Font{Family: "Consolas", PointSize: 8},
			},
		},
	}).Create(); err != nil {
		return fmt.Errorf("创建主窗口: %w", err)
	}

	// 关窗口只是收进托盘，不是退出 —— 否则隧道会跟着一起断
	u.mw.Closing().Attach(func(canceled *bool, _ walk.CloseReason) {
		if !u.quitting {
			*canceled = true
			u.mw.Hide()
		}
	})

	u.chkAuto.SetChecked(AutostartEnabled())
	if err := u.buildTray(); err != nil {
		return err
	}

	go u.refreshLoop()
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

	onShow := func() {
		u.mw.Show()
		u.mw.Activate()
	}

	mShow := walk.NewAction()
	_ = mShow.SetText("显示主窗口")
	mShow.Triggered().Attach(onShow)
	ni.ContextMenu().Actions().Add(mShow)
	ni.ContextMenu().Actions().Add(walk.NewSeparatorAction())

	mToggle := walk.NewAction()
	_ = mToggle.SetText("连接 / 断开")
	mToggle.Triggered().Attach(u.onToggle)
	ni.ContextMenu().Actions().Add(mToggle)
	ni.ContextMenu().Actions().Add(walk.NewSeparatorAction())

	mQuit := walk.NewAction()
	_ = mQuit.SetText("退出")
	mQuit.Triggered().Attach(u.quit)
	ni.ContextMenu().Actions().Add(mQuit)

	// 双击托盘图标回到主窗口
	ni.MouseDown().Attach(func(x, y int, button walk.MouseButton) {
		if button == walk.LeftButton {
			onShow()
		}
	})

	return ni.SetVisible(true)
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
	u.btnToggle.SetEnabled(configured)

	switch {
	case !configured:
		u.lblState.SetText("未配置连接码")
		u.lblState.SetTextColor(walk.RGB(0x88, 0x44, 0x00))
		u.lblDetail.SetText("复制整行 antapp:// 连接码，再点「从剪贴板导入连接码」")
		u.btnToggle.SetText("连接")
	case !st.Running:
		u.lblState.SetText("未连接")
		u.lblState.SetTextColor(walk.RGB(0x88, 0x44, 0x00))
		u.lblDetail.SetText("服务端 " + st.Server)
		u.btnToggle.SetText("连接")
	case st.Online:
		u.lblState.SetText("已连接")
		u.lblState.SetTextColor(walk.RGB(0x0A, 0x7D, 0x1E))
		u.lblDetail.SetText(fmt.Sprintf("隧道 %s · %s · 延迟 %d ms · ↑%.0f KB ↓%.0f KB",
			st.TunnelIP, st.Server, st.RTT.Milliseconds(),
			float64(st.TxBytes)/1024, float64(st.RxBytes)/1024))
		u.btnToggle.SetText("断开")
	default:
		u.lblState.SetText("连接中…")
		u.lblState.SetTextColor(walk.RGB(0x88, 0x44, 0x00))
		u.lblDetail.SetText("服务端 " + st.Server)
		u.btnToggle.SetText("断开")
	}

	// 上次的错误比「服务端 x」更有用，摆在明细行上
	if configured && st.LastError != "" && !st.Online {
		u.lblDetail.SetText(st.LastError)
	}

	if u.ni != nil {
		switch {
		case !configured:
			u.ni.SetToolTip("AntApp Link · 未配置")
		case !st.Running:
			u.ni.SetToolTip("AntApp Link · 未连接")
		case st.Online:
			u.ni.SetToolTip(fmt.Sprintf("AntApp Link · 已连接 %s · %d ms", st.TunnelIP, st.RTT.Milliseconds()))
		default:
			u.ni.SetToolTip("AntApp Link · 连接中…")
		}
	}

	// 日志只在内容变了才重设，否则每秒都会把滚动位置弹回顶部
	text := strings.Join(u.logs.Tail(200), "\r\n")
	if text != u.lastLogText {
		u.lastLogText = text
		u.txtLog.SetText(text)
		// 光标挪到末尾再滚过去，让最新一行始终可见
		u.txtLog.SetTextSelection(len(text), len(text))
		u.txtLog.ScrollToCaret()
	}
}

// Connect/Disconnect 会做网络操作（探测、改路由、跑 PowerShell），不能卡住界面线程。
func (u *UI) onToggle() {
	go func() {
		if u.app.Status().Running {
			_ = u.app.Disconnect()
		} else {
			_ = u.app.Connect()
		}
		u.mw.Synchronize(u.refresh)
	}()
}

// onImport 从剪贴板取连接码。
//
// 界面上没法做「粘贴一大段文本」的输入体验（那需要多行对话框），而连接码本来就是
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

func (u *UI) onAutostart() {
	go func() {
		if u.chkAuto.Checked() {
			_ = EnableAutostart()
		} else {
			_ = DisableAutostart()
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

func openInExplorer(path string) error {
	return runCommand(Command{"explorer", []string{path}})
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
