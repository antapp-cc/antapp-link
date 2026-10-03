//go:build windows

package client

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"errors"
	"fmt"
	"net"
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

//go:embed assets/antapp-fill.ico
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

	// 托盘菜单里的动态配置区（OpenVPN 式：每个 .antapp 一项，点谁连谁）
	mImport    *walk.Action
	cfgActions []*walk.Action

	lblState    *walk.Label
	lblCfg      *walk.Label
	lblIP       *walk.Label
	lblTraffic  *walk.Label
	lblVersion  *walk.Label
	txtLog      *walk.TextEdit
	btnPrimary  *walk.PushButton
	btnReconn   *walk.PushButton
	btnPortTest *walk.PushButton
	btnUpdate   *walk.PushButton
	btnHide     *walk.PushButton

	quitting      bool
	trayHinted    bool
	trayConnected bool
	logSeq        uint64
	done          chan struct{}
	pending       *update.Manifest

	// adoptBusy 防止切换配置的弹窗重入。
	adoptBusy bool

	// lastLoggedErr 去重：同一条连接错误只在日志里记一次，不每秒刷屏。
	lastLoggedErr string

	// busyOp 非空表示一次连接/断开在跑。只允许 UI 线程读写：
	// 点按钮立刻置上并渲染「连接中/断开中」，操作结束在 Synchronize 里清掉。
	// 值为 "connect" / "disconnect" / "reconnect"。
	busyOp string
}

// RunUI 阻塞运行图形界面，直到用户从托盘菜单退出。
func RunUI(app *App, logs *LogBuffer, rootDir string) error {
	iconPath, err := ensureIconFile(rootDir, "antapp-fill.ico", appIcon)
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
			Composite{
				Layout: HBox{MarginsZero: true},
				Children: []Widget{
					Label{
						AssignTo: &u.lblState,
						Text:     "当前状态: 未连接",
						Font:     Font{PointSize: 10},
					},
					HSpacer{},
					Label{
						AssignTo: &u.lblCfg,
						Text:     "",
						Font:     Font{PointSize: 9},
					},
				},
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
					PushButton{AssignTo: &u.btnPortTest, Text: "Node端口测试", MinSize: Size{Width: 110}, OnClicked: u.onPortTest},
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

	// OpenVPN 式：config 里一个文件就自动连；多个不自动连，
	// 从托盘菜单里点哪个连哪个（客户端不记录上次连的是哪个文件）。
	// 开机自启时网络栈常常还没就绪（DHCP/路由刚起来），首次连接会探测失败——
	// 失败后自动退避重试，而不是把「开机自动连」变成「开机必然失败」。
	if files := ListInviteFiles(u.app.RootDir()); len(files) == 1 {
		name := files[0]
		if u.adoptByName(name) {
			u.app.Log().Info(fmt.Sprintf("已自动连接配置 %s", name))
		} else {
			go func() {
				for attempt := 0; attempt < 6; attempt++ {
					select {
					case <-u.done:
						return
					case <-time.After(time.Duration(attempt+1) * 10 * time.Second):
					}
					if u.app.Status().Online {
						return
					}
					u.app.Log().Info(fmt.Sprintf("开机网络未就绪，自动重试连接 %s（第 %d 次）……", name, attempt+1))
					if u.adoptByName(name) {
						return
					}
				}
			}()
		}
	} else if len(files) > 1 {
		u.app.Log().Info(fmt.Sprintf("config 里有 %d 个连接码文件：连接哪个请从托盘菜单选择", len(files)))
	}

	go u.refreshLoop()
	go u.autoCheckUpdate()
	go u.watchInvite()
	u.refresh()
	return nil
}

// watchInvite 盯着 config\*.antapp 的内容变化：运行期间用户放进新文件、或改了
// 现有文件的内容，都会在这里被发现。从「没有配置」变成「恰好一个」时自动选用
// 并连接（与启动时一致）；其余情况只提示，让用户从托盘菜单选。
//
// 用户在客户端已经运行时双击一个 .antapp 文件，那个新进程只会把文件复制进 config\
// 然后退出（单实例闸门挡着）。真正的切换得由这条链完成 —— 否则双击看起来毫无反应。
func (u *UI) watchInvite() {
	snapshot := func() map[string]time.Time {
		now := map[string]time.Time{}
		for _, name := range ListInviteFiles(u.app.RootDir()) {
			path := filepath.Join(ConfigDir(u.app.RootDir()), name)
			if st, err := os.Stat(path); err == nil {
				now[name] = st.ModTime()
			}
		}
		return now
	}
	last := snapshot()

	for {
		select {
		case <-u.done:
			return
		case <-time.After(2 * time.Second):
		}

		now := snapshot()
		changed := len(now) != len(last)
		if !changed {
			for name, mod := range now {
				if last[name] != mod {
					changed = true
					break
				}
			}
		}
		if !changed {
			continue
		}
		prev := last
		last = now

		// 从「没有配置」变成「恰好一个」时自动选用并连接，与启动时的行为一致。
		// 原来这里只记一条日志、主按钮始终停在「导入连接码」，用户手动把文件放进
		// config\ 之后会以为没生效，只能绕到托盘菜单去连。
		if len(now) == 1 && len(prev) == 0 {
			for name := range now {
				if u.adoptByName(name) {
					u.app.Log().Info(fmt.Sprintf("已自动连接配置 %s", name))
				}
				break
			}
			continue
		}

		u.app.Log().Info("config 文件夹的连接码有变化；如需切换，请使用托盘菜单的「切换配置文件」")
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

	// 菜单每次右键弹出前全量重建（固定项 + config 文件动态区），
	// 文件增删改名立即反映到菜单里
	u.rebuildTrayMenu()

	// 双击托盘图标回到主窗口；右键弹出前重建菜单（刷新配置文件列表）
	ni.MouseDown().Attach(func(x, y int, button walk.MouseButton) {
		if button == walk.LeftButton {
			u.showWindow()
			return
		}
		if button == walk.RightButton {
			u.rebuildTrayMenu()
		}
	})

	return ni.SetVisible(true)
}

// rebuildTrayMenu 全量重建托盘右键菜单：固定项 + config\*.antapp 动态配置区
// （OpenVPN 式——两个及以上文件才列出，正在用的显示「断开」，其余显示「连接」）。
func (u *UI) rebuildTrayMenu() {
	menu := u.ni.ContextMenu()
	_ = menu.Actions().Clear()

	add := func(text string, onClick func()) {
		a := walk.NewAction()
		a.SetText(text)
		a.Triggered().Attach(onClick)
		menu.Actions().Add(a)
	}

	add("显示主窗口", u.showWindow)
	menu.Actions().Add(walk.NewSeparatorAction())
	add("连接 / 断开", u.onPrimary)

	files := ListInviteFiles(u.app.RootDir())
	if len(files) >= 2 {
		running := u.app.Status().Running
		current := u.app.CurrentSource()
		if !running {
			current = "" // 没连着就全部显示「连接」
		}
		menu.Actions().Add(walk.NewSeparatorAction())
		for _, name := range files {
			name := name
			if name == current {
				add("断开 "+name, func() {
					go func() {
						_ = u.app.Disconnect()
						u.mw.Synchronize(u.refresh)
					}()
				})
			} else {
				add("连接 "+name, func() {
					if u.adoptBusy {
						return
					}
					u.adoptBusy = true
					go func() {
						if u.adoptByName(name) {
							u.app.Log().Info(fmt.Sprintf("已切换到配置 %s", name))
						}
						u.adoptBusy = false
						u.mw.Synchronize(u.refresh)
					}()
				})
			}
		}
	}

	menu.Actions().Add(walk.NewSeparatorAction())
	add("导入连接码", u.onImport)
	add("打开配置目录", u.onOpenConfigDir)
	add("检查更新", u.onCheckUpdate)
	menu.Actions().Add(walk.NewSeparatorAction())

	autostart := walk.NewAction()
	autostart.SetText("开机自启")
	autostart.SetChecked(AutostartEnabled())
	autostart.Triggered().Attach(func() {
		go func() {
			if autostart.Checked() {
				_ = DisableAutostart()
				autostart.SetChecked(false)
			} else {
				_ = EnableAutostart()
				autostart.SetChecked(true)
			}
		}()
	})
	menu.Actions().Add(autostart)
	menu.Actions().Add(walk.NewSeparatorAction())
	add("退出", u.quit)
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

// adoptCandidate 按文件名采用配置并重连。用户的文件保持原名原内容不动 ——
// 客户端只在连接时读它，不复制不改名。返回是否成功。
func (u *UI) adoptByName(name string) bool {
	inv, err := LoadInviteFile(u.app.RootDir(), name)
	if err != nil {
		u.alert("读取连接码文件失败", err.Error())
		return false
	}
	if err := u.app.UpdateInvite(inv, name); err != nil {
		u.alert("切换配置失败", err.Error())
		return false
	}
	u.refresh()
	go func() {
		_ = u.app.Connect()
		u.mw.Synchronize(u.refresh)
	}()
	return true
}

// pickConfig 弹出配置选择列表：只显示文件名。列表顺序固定按文件名排序，
// 当前使用的行内标注「（当前使用）」——顺序不能随当前配置变（当前项挪首位会让用户按记忆点错行）。
// currentSource 非空时列表首位放当前正在用的文件并标注；返回选中的文件名。
func (u *UI) pickConfig(currentSource string, names []string) (string, bool) {
	// 列表顺序固定按文件名排序，当前使用的行内标注「（当前使用）」。
	items := make([]string, len(names))
	for i, n := range names {
		items[i] = n
		if n == currentSource {
			items[i] += "　（当前使用）"
		}
	}

	var dlg *walk.Dialog
	var lb *walk.ListBox
	var btnOK *walk.PushButton
	chosen := false
	pick := func() {
		if lb.CurrentIndex() >= 0 {
			chosen = true
			dlg.Accept()
		}
	}
	if err := (Dialog{
		AssignTo: &dlg,
		Title:    "选择要使用的配置",
		MinSize:  Size{Width: 460, Height: 300},
		Layout:   VBox{Margins: Margins{Left: 16, Top: 16, Right: 16, Bottom: 16}, Spacing: 10},
		Children: []Widget{
			Label{Text: "config 文件夹里有多个连接码，单击选中一行，再点下面的按钮；双击行直接使用："},
			ListBox{
				AssignTo: &lb,
				Model:    items,
				OnCurrentIndexChanged: func() {
					btnOK.SetEnabled(lb.CurrentIndex() >= 0)
				},
				OnItemActivated: pick, // 双击行直接采用
			},
			Composite{
				Layout: HBox{MarginsZero: true, Spacing: 8},
				Children: []Widget{
					HSpacer{},
					PushButton{
						AssignTo:  &btnOK,
						Text:      "使用选中的",
						MinSize:   Size{Width: 110},
						Enabled:   false, // 没选中行之前禁用，防止误提交默认行
						OnClicked: pick,
					},
					PushButton{Text: "取消", MinSize: Size{Width: 80}, OnClicked: func() { dlg.Cancel() }},
				},
			},
		},
	}).Create(u.mw); err != nil {
		u.alert("无法显示选择窗口", err.Error())
		return "", false
	}
	dlg.Run()
	if !chosen {
		return "", false
	}
	idx := lb.CurrentIndex()
	if idx < 0 || idx >= len(names) {
		return "", false
	}
	u.app.Log().Info(fmt.Sprintf("配置选择结果：列表共 %d 项，用户选定第 %d 项 = %s", len(names), idx+1, names[idx]))
	return names[idx], true
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
	case busyOp == "porttest":
		state = "当前状态: 正在测试节点端口…"
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
	if src := u.app.CurrentSource(); src != "" {
		u.lblCfg.SetText("配置文件: " + src)
	} else {
		u.lblCfg.SetText("")
	}

	// 上次的错误比「服务端 x」更有用，就写进日志区（每条错误只记一次，不刷屏），
	// 状态行保持干净的「服务端 x」。
	if configured && !st.Online && st.LastError != "" && st.LastError != u.lastLoggedErr {
		u.lastLoggedErr = st.LastError
		u.app.Log().Warn("连接失败", "err", st.LastError)
	}
	switch {
	case busyOp != "":
		u.lblIP.SetText("正在交换网络配置，请稍候…")
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
	case busyOp == "porttest":
		u.btnPrimary.SetText("断开连接")
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
	if busyOp == "porttest" {
		u.btnPortTest.SetText("测试中…")
		u.btnPortTest.SetEnabled(false)
	} else {
		u.btnPortTest.SetText("Node端口测试")
		u.btnPortTest.SetEnabled(configured && st.Running)
	}

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
		if files := ListInviteFiles(u.app.RootDir()); len(files) == 1 {
			if u.adoptByName(files[0]) {
				u.app.Log().Info(fmt.Sprintf("已选用配置 %s", files[0]))
			}
			return
		}
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

// onPortTest 借 Pi 官方 checker 容器把 10 个转发端口完整测一遍。
// 隧道必须在线（测试要穿过云服转发），期间所有操作按钮锁死防重入。
func (u *UI) onPortTest() {
	if u.busyOp != "" {
		return
	}
	st := u.app.Status()
	if !st.Running {
		u.app.Log().Warn("隧道未连接，先连接后再测试端口")
		return
	}
	host, _, err := net.SplitHostPort(st.Server)
	if err != nil {
		host = st.Server
	}
	u.busyOp = "porttest"
	u.refresh()
	go func() {
		log := u.app.Log()
		results, err := RunPortTest(host, log)
		u.mw.Synchronize(func() {
			if err != nil {
				log.Warn(fmt.Sprintf("节点端口测试未完成: %v", err))
			} else {
				ok := 0
				for _, r := range results {
					if r.OK {
						ok++
					}
				}
				log.Info(fmt.Sprintf("节点端口测试完成：%d 个端口中 %d 个通，节点已恢复运行", len(results), ok))
			}
			u.busyOp = ""
			u.refresh()
		})
	}()
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
	// 先等隧道连上、再稳 30 秒才查：没连上时海外流量没有隧道路由，访问 GitHub
	// 必定超时，白跑一次还会在日志里留一条「更新源不可用」；刚连上那几秒
	// 路由与 DNS 接管也未必落定，所以额外再等一段。
	// 等不到就先跳过这一轮，交给下面每小时的循环重试。
	if u.waitTunnelReady(5 * time.Minute) {
		u.checkUpdate(false)
	}

	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-u.done:
			return
		case <-ticker.C:
			if u.app.Status().Online {
				u.checkUpdate(false)
			}
		}
	}
}

// waitTunnelReady 等隧道就绪后再稳 30 秒；超时或界面已关闭返回 false。
func (u *UI) waitTunnelReady(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for !u.app.Status().Online {
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-u.done:
			return false
		case <-time.After(time.Second):
		}
	}
	select {
	case <-u.done:
		return false
	case <-time.After(30 * time.Second):
	}
	return true
}

func (u *UI) onCheckUpdate() { go u.checkUpdate(true) }

func (u *UI) checkUpdate(manual bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	m, err := u.app.CheckUpdate(ctx, !manual)

	u.mw.Synchronize(func() {
		switch {
		case m != nil:
			u.pending = m
			u.btnUpdate.SetText("更新到 " + m.Version)
			u.btnUpdate.SetVisible(true)
			u.refresh()
			if manual {
				u.askUpdate()
			} else {
				// 自动更新：检测到新版本直接静默下载并应用，无需手动点击。
				// 失败只记日志，按钮保留供手动重试。
				go u.autoApplyUpdate(m)
			}
		case err != nil:
			if manual {
				hint := ""
				if !u.app.Status().Running {
					hint = "\n\n当前未连接隧道——更新服务器在国外，直连可能不稳定，连接后再试通常就能成功。"
				}
				walk.MsgBox(u.mw, "检查更新失败", err.Error()+hint, walk.MsgBoxIconWarning)
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

// autoApplyUpdate 静默自动更新：下载 → 校验 → 应用重启，全程无交互。
// 失败只记日志，界面按钮保留供手动重试。
func (u *UI) autoApplyUpdate(m *update.Manifest) {
	u.app.Log().Info(fmt.Sprintf("检测到新版本 %s，开始自动更新", m.Version))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	newExe, err := u.app.DownloadUpdate(ctx, m)
	if err != nil {
		u.app.Log().Warn(fmt.Sprintf("自动更新下载失败: %v（可手动点「更新到 %s」重试）", err, m.Version))
		return
	}
	u.app.Log().Info("更新包校验通过，正在应用并重启……")

	done := make(chan struct{})
	u.mw.Synchronize(func() {
		defer close(done)
		if err := u.app.ApplyUpdate(newExe); err != nil {
			u.app.Log().Warn(fmt.Sprintf("自动更新应用失败: %v（可手动点「更新到 %s」重试）", err, m.Version))
			u.btnUpdate.SetEnabled(true)
			return
		}
		u.quitting = true
		os.Exit(0)
	})
	<-done
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

// config 里一个文件直接连，多个弹列表让用户挑，零个引导导入。
func (u *UI) resolveFromConfigDir() {
	files := ListInviteFiles(u.app.RootDir())
	if len(files) == 0 {
		u.onImport()
		return
	}
	var name string
	if len(files) == 1 {
		name = files[0]
	} else {
		if dups := DuplicateFileNames(files); len(dups) > 0 {
			walk.MsgBox(u.mw, "检测到同名配置",
				fmt.Sprintf("config 文件夹里有多个同名的文件：%s。\n\n名字一样分不清谁是谁，请在下面的列表里选择要使用的；多余的建议删掉。",
					strings.Join(dups[0].Files, "、")),
				walk.MsgBoxIconWarning)
		}
		picked, ok := u.pickConfig("", files)
		if !ok {
			return
		}
		// 提交前把将要使用的文件名亮出来让用户确认——选择列表里的高亮行
		// 不一定就是用户心里想的那行，这一步兜底
		name = picked
		if walk.MsgBox(u.mw, "确认使用这个配置",
			fmt.Sprintf("将使用配置文件：%s\n\n确认并连接？", name),
			walk.MsgBoxYesNo|walk.MsgBoxIconQuestion) != walk.DlgCmdYes {
			return
		}
	}
	if u.adoptByName(name) {
		u.app.Log().Info(fmt.Sprintf("已选用配置 %s", name))
	}
}

// rebuildConfigMenu 重建托盘菜单里的动态配置区：config\ 里每个 .antapp 一项。
// 正在用的显示「断开 xxx」，其余显示「连接 xxx」—— 点谁连谁，和 OpenVPN GUI 一样。
func (u *UI) rebuildConfigMenu() {
	actions := u.ni.ContextMenu().Actions()
	for _, a := range u.cfgActions {
		actions.Remove(a)
	}
	u.cfgActions = nil

	// 只有一个配置文件时「连接 / 断开」主项就够了，不列出文件项（对齐 OpenVPN）
	files := ListInviteFiles(u.app.RootDir())
	if len(files) < 2 {
		return
	}
	insertAt := actions.Index(u.mImport)
	running := u.app.Status().Running
	current := u.app.CurrentSource()
	if !running {
		current = ""
	}

	for _, name := range files {
		a := walk.NewAction()
		name := name
		if name == current {
			_ = a.SetText("断开 " + name)
			a.Triggered().Attach(func() {
				go func() {
					_ = u.app.Disconnect()
					u.mw.Synchronize(u.refresh)
				}()
			})
		} else {
			_ = a.SetText("连接 " + name)
			a.Triggered().Attach(func() {
				if u.adoptBusy {
					return
				}
				u.adoptBusy = true
				go func() {
					if u.adoptByName(name) {
						u.app.Log().Info(fmt.Sprintf("已切换到配置 %s", name))
					}
					u.adoptBusy = false
					u.mw.Synchronize(u.refresh)
				}()
			})
		}
		if err := actions.Insert(insertAt, a); err != nil {
			return
		}
		insertAt++
		u.cfgActions = append(u.cfgActions, a)
	}
}

// 菜单项只在 config 里存在不同配置时可点；adoptBusy 与自动识别互斥，防两个框并发。
func (u *UI) onSwitchConfig() {
	if u.adoptBusy || !HasMultipleInvites(u.app.RootDir()) {
		return
	}
	u.adoptBusy = true
	defer func() { u.adoptBusy = false }()
	name, ok := u.pickConfig(u.app.CurrentSource(), ListInviteFiles(u.app.RootDir()))
	if !ok || name == u.app.CurrentSource() {
		return
	}
	if u.adoptByName(name) {
		u.app.Log().Info(fmt.Sprintf("已切换到配置 %s", name))
	}
}

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

	// 导入 = 把连接码文件原样复制进 config\（保留你的名字），再切换连接。别卡住界面线程
	go func() {
		var source string
		if fromFile {
			if _, err := ImportInviteFile(u.app.RootDir(), raw); err != nil {
				u.alert("保存连接码失败", err.Error())
				return
			}
			source = filepath.Base(raw)
		} else {
			name, err := ImportInviteBytes(u.app.RootDir(), "导入的连接码.antapp", []byte(strings.TrimSpace(raw)))
			if err != nil {
				u.alert("保存连接码失败", err.Error())
				return
			}
			source = name
		}
		if err := u.app.UpdateInvite(inv, source); err != nil {
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
