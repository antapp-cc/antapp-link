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
	"github.com/lxn/win"
	"golang.org/x/sys/windows"

	"github.com/antapp-cc/antapp-link/internal/setup"
	"github.com/antapp-cc/antapp-link/internal/update"
)

//go:embed assets/antapp.ico
var appIcon []byte

// 托盘变色版：黑灰=未连接，鲜绿=已连接——用户扫一眼托盘就知道隧道通没通。
// 由 tools/grayico 从 antapp.ico 生成，换 logo 后记得重新生成这两个文件。
//
//go:embed assets/antapp-gray.ico
var appIconGray []byte

//go:embed assets/antapp-green.ico
var appIconGreen []byte

// UI 是主窗口加托盘。托盘用 walk 自带的 NotifyIcon，跟主窗口共用同一个消息循环 ——
// 换成独立的托盘库就得处理两个消息循环抢主线程的问题。
//
// 窗口布局照用户已经在 Pi 节点机上用惯的那个 OpenVPN 客户端来：
// 状态行 → 大日志区 → 分配 IP 与版本 → 三个按钮。
type UI struct {
	app       *App
	logs      *LogBuffer
	icon      *walk.Icon
	iconGray  *walk.Icon
	iconGreen *walk.Icon

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

	quitting      bool
	trayHinted    bool
	trayConnected bool
	logSeq        uint64
	done          chan struct{}
	pending       *update.Manifest

	// busyOp 非空表示一次连接/断开在跑。只允许 UI 线程读写：
	// 点按钮立刻置上并渲染「连接中/断开中」，操作结束在 Synchronize 里清掉。
	// 值为 "connect" / "disconnect" / "reconnect"。
	busyOp string
}

// RunUI 阻塞运行图形界面，直到用户从托盘菜单退出。
func RunUI(app *App, logs *LogBuffer, rootDir string) error {
	iconPath, err := ensureIconFile(rootDir, "antapp.ico", appIcon)
	if err != nil {
		return err
	}
	icon, err := walk.NewIconFromFile(iconPath)
	if err != nil {
		return fmt.Errorf("加载图标 %s: %w", iconPath, err)
	}
	grayPath, err := ensureIconFile(rootDir, "antapp-gray.ico", appIconGray)
	if err != nil {
		return err
	}
	iconGray, err := walk.NewIconFromFile(grayPath)
	if err != nil {
		return fmt.Errorf("加载灰色图标 %s: %w", grayPath, err)
	}
	greenPath, err := ensureIconFile(rootDir, "antapp-green.ico", appIconGreen)
	if err != nil {
		return err
	}
	iconGreen, err := walk.NewIconFromFile(greenPath)
	if err != nil {
		return fmt.Errorf("加载绿色图标 %s: %w", greenPath, err)
	}

	u := &UI{app: app, logs: logs, icon: icon, iconGray: iconGray, iconGreen: iconGreen, done: make(chan struct{})}
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
		u.hintTray()
	})

	// 每次打开都居中。不指定位置的话 Windows 按「层叠」摆放，
	// 多开几次就跑偏到屏幕角落了。
	setup.CenterOnScreen(u.mw.Handle())

	// 日志框退出焦点轮转。
	//
	// 窗口被激活时，Windows 把焦点给第一个带 WS_TABSTOP 的控件 —— 只读日志框
	// 拿到焦点就把整片内容画成蓝底。在 VisibleChanged 里 SetFocus 给按钮试过，
	// 没用（会被随后的 SetForegroundWindow 重置）。索性让它根本不参与轮转。
	disableTabStop(u.txtLog.Handle())

	if err := u.buildTray(); err != nil {
		return err
	}

	// 窗口每次重新显示：滚到最后一行，并把焦点给主按钮。
	//
	// 不在这里清选择 —— 同 refresh 里的道理，碰 EM_SETSEL 反而会引入高亮。
	// 焦点给主按钮是因为 Windows 默认把焦点给第一个可聚焦控件（那个只读日志框），
	// 拿不到焦点就不存在「聚焦态高亮」。
	u.mw.VisibleChanged().Attach(func() {
		if !u.mw.Visible() {
			return
		}
		const (
			wmVScroll = 0x0115
			sbBottom  = 7
		)
		u.txtLog.SendMessage(wmVScroll, sbBottom, 0)
		_ = u.btnPrimary.SetFocus()
	})

	// 没有新版本时这个按钮不该占着位置
	u.btnUpdate.SetVisible(false)

	go u.refreshLoop()
	go u.autoCheckUpdate()
	go u.watchInvite()
	u.refresh()
	return nil
}

// watchInvite 盯着 config\node.antapp，发现被外部改过就重载并重连。
//
// 用户在客户端已经运行时双击一个 .antapp 文件，那个新进程只会把连接码写进文件
// 然后退出（单实例闸门挡着）。真正的切换得由这里完成 —— 否则双击看起来毫无反应。
func (u *UI) watchInvite() {
	path := InviteFilePath(u.app.RootDir())
	last := inviteModTime(path)

	for {
		select {
		case <-u.done:
			return
		case <-time.After(2 * time.Second):
		}

		now := inviteModTime(path)
		if now.IsZero() || now.Equal(last) {
			continue
		}
		last = now

		inv, err := LoadSavedInvite(u.app.RootDir())
		if err != nil {
			u.app.Log().Warn("连接码变了但读不出来", "err", err)
			continue
		}
		if err := u.app.UpdateInvite(inv); err != nil {
			u.app.Log().Warn("换用新连接码失败", "err", err)
			continue
		}
		u.app.Log().Info("检测到新的连接码，重新连接", "server", inv.Server, "name", inv.Name)

		go func() {
			_ = u.app.Connect()
			u.mw.Synchronize(u.refresh)
		}()
	}
}

func inviteModTime(path string) time.Time {
	st, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return st.ModTime()
}

func (u *UI) buildTray() error {
	ni, err := walk.NewNotifyIcon(u.mw)
	if err != nil {
		return fmt.Errorf("创建托盘图标: %w", err)
	}
	u.ni = ni
	// 初始就用对状态的图标，避免启动瞬间闪一下；之后由 refresh() 在切换时换。
	u.trayConnected = u.app.Status().Online
	if u.trayConnected {
		err = ni.SetIcon(u.iconGreen)
	} else {
		err = ni.SetIcon(u.iconGray)
	}
	if err != nil {
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
	_ = mImport.SetText("导入连接码")
	mImport.Triggered().Attach(u.onImport)
	ni.ContextMenu().Actions().Add(mImport)

	mOpenDir := walk.NewAction()
	_ = mOpenDir.SetText("打开配置目录")
	mOpenDir.Triggered().Attach(u.onOpenConfigDir)
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

	// 操作进行中的渲染优先：按钮和状态行立刻给出过渡态，
	// 绝不能让用户对着一个没变化的界面猜程序死活。
	busyOp := u.busyOp
	switch {
	case busyOp == "" && st.Connecting:
		busyOp = "connect"
	case busyOp == "" && st.Disconnecting:
		busyOp = "disconnect"
	}

	var state string
	switch {
	case !configured:
		state = "当前状态: 未配置连接码"
	case busyOp == "disconnect":
		state = "当前状态: 正在断开…"
	case busyOp == "connect" || busyOp == "reconnect":
		state = "当前状态: 正在连接…"
	case !st.Running:
		state = "当前状态: 未连接"
	case st.Online:
		state = "当前状态: 已连接"
	default:
		state = "当前状态: 连接中…"
	}
	if configured && st.Online && busyOp == "" {
		state += fmt.Sprintf("（延迟 %d ms）", st.RTT.Milliseconds())
	}
	if u.pending != nil {
		state += fmt.Sprintf("　·　有新版本 %s", u.pending.Version)
	}
	u.lblState.SetText(state)

	// 上次的错误比「服务端 x」更有用，就摆在状态行下面
	switch {
	case busyOp != "":
		u.lblIP.SetText("正在交换网络配置，请稍候…")
	case configured && !st.Online && st.LastError != "":
		u.lblIP.SetText("上次错误: " + truncateRunes(st.LastError, 46))
	case st.Online:
		u.lblIP.SetText(fmt.Sprintf("分配 IP: %s", st.TunnelIP))
	case configured:
		u.lblIP.SetText("服务端: " + st.Server)
	default:
		u.lblIP.SetText("分配 IP: —")
	}

	// 流量按本次连接累计；断开重连会新开一个隧道实例，计数从零开始
	if st.Running {
		u.lblTraffic.SetText(fmt.Sprintf("发送 %s / 接收 %s",
			formatBytes(st.TxBytes), formatBytes(st.RxBytes)))
	} else {
		u.lblTraffic.SetText("发送 — / 接收 —")
	}

	// 按钮文字跟着状态走；操作进行中一律禁用，防重入。
	switch {
	case busyOp == "disconnect":
		u.btnPrimary.SetText("断开中…")
		u.btnPrimary.SetEnabled(false)
	case busyOp == "connect" || busyOp == "reconnect":
		u.btnPrimary.SetText("连接中…")
		u.btnPrimary.SetEnabled(false)
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
	u.btnReconn.SetEnabled(configured && st.Running && busyOp == "")

	if u.ni != nil {
		// 托盘颜色 = 隧道通没通：已连接鲜绿，其余（未连/连接中/未配置）黑灰。
		// 只在状态切换时 SetIcon，每秒重设会让图标闪烁。
		if color := busyOp == "" && st.Online; color != u.trayConnected {
			u.trayConnected = color
			icon := u.iconGray
			if color {
				icon = u.iconGreen
			}
			_ = u.ni.SetIcon(icon)
		}
		switch {
		case busyOp == "disconnect":
			u.ni.SetToolTip("AntApp Link · 正在断开…")
		case busyOp != "" || (st.Running && !st.Online):
			u.ni.SetToolTip("AntApp Link · 连接中…")
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

	// 日志只做增量追加，绝不整体 SetText。
	//
	// SetText 走的是 WM_SETTEXT，之后这个只读框的选择会变成「全选」，一有焦点就
	// 整片画成蓝底。之后无论用 EM_SETSEL 去清、还是只用滚动消息绕开，都压不住 ——
	// 三种写法都试过，见 git 历史（dc4e6dc / 2fdfafc / f27a79a）。
	//
	// 改成只追加新行：控件从空开始，选择状态就永远停在初始值，不给它变全选的机会。
	if newLines, seq := u.logs.Since(u.logSeq); len(newLines) > 0 {
		u.logSeq = seq
		u.txtLog.AppendText(strings.Join(newLines, "\r\n") + "\r\n")

		const (
			wmVScroll = 0x0115
			sbBottom  = 7
		)
		u.txtLog.SendMessage(wmVScroll, sbBottom, 0)
	}
}

// Connect/Disconnect 会做网络操作（探测、改路由、跑 PowerShell），不能卡住界面线程。
// busyOp 让按钮在点击的一瞬间就给出过渡态并禁用 —— 旧版这里十几秒没有任何变化，
// 用户以为没点上，反复点击还会跟进行中的网络还原撞车。
func (u *UI) onPrimary() {
	if u.busyOp != "" {
		return
	}
	if !u.app.Configured() {
		u.onImport()
		return
	}
	op := "connect"
	if u.app.Status().Running {
		op = "disconnect"
	}
	u.busyOp = op
	u.refresh() // 立刻渲染过渡态，不等下一秒的定时刷新
	go func() {
		if op == "disconnect" {
			_ = u.app.Disconnect()
		} else {
			_ = u.app.Connect()
		}
		u.mw.Synchronize(func() {
			u.busyOp = ""
			u.refresh()
		})
	}()
}

func (u *UI) onReconnect() {
	if u.busyOp != "" {
		return
	}
	u.busyOp = "reconnect"
	u.refresh()
	go func() {
		_ = u.app.Disconnect()
		_ = u.app.Connect()
		u.mw.Synchronize(func() {
			u.busyOp = ""
			u.refresh()
		})
	}()
}

func (u *UI) onHide() {
	u.mw.Hide()
	u.hintTray()
}

// hintTray 第一次收进托盘时说一声，之后不再打扰。
//
// 「隐藏」按钮走的是这里，而点 X 走 Closing 事件 —— 两条路都要提示，否则
// 用户从按钮收起窗口时就完全没反馈（实测就是这么漏掉的）。
//
// 气泡停多久由系统的「通知显示时长」决定（默认 5 秒），程序改不了：Vista 之后
// uTimeout 被忽略，试过自己再发一条空 NIF_INFO 去收掉，结果系统把图标状态也
// 一起改坏、之后干脆不显示了，所以不再干预。
func (u *UI) hintTray() {
	if u.trayHinted {
		return
	}
	u.trayHinted = true
	_ = u.ni.ShowInfo("AntApp Link 仍在运行",
		"窗口已收进托盘，专线保持连接。\n双击托盘图标可以重新打开，右键菜单里可以退出。")
}

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

// onImport 导入连接码：先问来源，再走对应的读取方式。
//
// 两条路都得有 —— 服务端 invite 出来的是文件，用户拷到节点机上得能选；
// 而单行 antapp:// 码通常是从聊天窗口复制的，读剪贴板最省事。
func (u *UI) onImport() {
	fromFile, ok := u.askImportSource()
	if !ok {
		return
	}

	var raw string
	if fromFile {
		dlg := new(walk.FileDialog)
		dlg.Title = "选择连接码文件"
		dlg.Filter = "AntApp Link 连接码 (*.antapp)|*.antapp"
		chosen, err := dlg.ShowOpen(u.mw)
		if err != nil {
			u.alert("打开文件对话框失败", err.Error())
			return
		}
		if !chosen {
			return
		}
		raw = dlg.FilePath
	} else {
		text, err := ReadClipboard()
		if err != nil {
			u.alert("读取剪贴板失败", err.Error())
			return
		}
		raw = text
	}

	inv, err := LoadInvite(raw)
	if err != nil {
		where := "剪贴板里没有可用的连接码"
		if fromFile {
			where = "这个文件里没有可用的连接码"
		}
		u.alert(where, "需要服务端生成的 .antapp 连接码文件，或整行 antapp:// 连接码。\n\n"+err.Error())
		return
	}

	// 保存与切换连接会先断开再重连，别卡住界面线程
	go func() {
		if err := SaveInvite(u.app.RootDir(), inv); err != nil {
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

// askImportSource 问连接码从哪来。ok=false 表示用户取消。
func (u *UI) askImportSource() (fromFile bool, ok bool) {
	var dlg *walk.Dialog

	if err := (Dialog{
		AssignTo: &dlg,
		Title:    "导入连接码",
		Icon:     u.icon,
		Size:     Size{Width: 470, Height: 210},
		MinSize:  Size{Width: 430, Height: 190},
		Layout:   VBox{Margins: Margins{Left: 16, Top: 16, Right: 16, Bottom: 16}, Spacing: 10},
		Children: []Widget{
			Label{Text: "连接码从哪里来？", Font: Font{PointSize: 10}},
			Label{Text: "服务端生成的 .antapp 连接码文件，\n或者聊天窗口里复制好的整行 antapp:// 连接码。"},
			Composite{
				Layout: HBox{MarginsZero: true, Spacing: 8},
				Children: []Widget{
					PushButton{
						Text: "从文件导入…", MinSize: Size{Width: 120}, MaxSize: Size{Width: 120},
						OnClicked: func() { fromFile, ok = true, true; dlg.Accept() },
					},
					PushButton{
						Text: "从剪贴板导入", MinSize: Size{Width: 120}, MaxSize: Size{Width: 120},
						OnClicked: func() { fromFile, ok = false, true; dlg.Accept() },
					},
					HSpacer{},
					PushButton{
						Text: "取消", MinSize: Size{Width: 80}, MaxSize: Size{Width: 80},
						OnClicked: func() { dlg.Cancel() },
					},
				},
			},
		},
	}).Create(u.mw); err != nil {
		u.alert("打开导入窗口失败", err.Error())
		return false, false
	}

	dlg.Run()
	return fromFile, ok
}

// onOpenConfigDir 打开 config\ —— 用户从这里拿连接码、换连接码。
//
// 打开的是配置目录而不是程序目录：程序目录里就是一堆 exe/dll，没什么可看的；
// 用户点这个菜单，想找的基本都是那个 node.antapp。
func (u *UI) onOpenConfigDir() {
	go func() {
		dir := ConfigDir(u.app.RootDir())
		if err := os.MkdirAll(dir, 0o700); err != nil {
			u.alert("打开配置目录失败", err.Error())
			return
		}
		if err := openInExplorer(dir); err != nil {
			u.alert("打开配置目录失败", err.Error())
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

// disableTabStop 摘掉控件的 WS_TABSTOP，让它不再被 Windows 选作默认焦点。
func disableTabStop(w win.HWND) {
	const wsTabStop = 0x00010000
	style := win.GetWindowLong(w, win.GWL_STYLE)
	win.SetWindowLong(w, win.GWL_STYLE, style&^wsTabStop)
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

// ensureIconFile 把内嵌的图标释放出来，供 walk 按路径加载。
// 它是从 exe 提取的缓存，放运行时数据目录，不跟配置混在一起。
func ensureIconFile(root, name string, data []byte) (string, error) {
	if len(data) == 0 {
		return "", errors.New("没有内嵌图标数据")
	}
	path := filepath.Join(RuntimeDir(root), name)
	want := sha256.Sum256(data)
	if existing, err := os.ReadFile(path); err == nil && sha256.Sum256(existing) == want {
		return path, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return path, nil
}
