// Command antapp-setup 是 AntApp Link 的安装 / 卸载程序。
//
// 安装：把 antapp-link.exe 释放到安装目录、建快捷方式、登记到「应用和功能」。
// 卸载：同一个程序加 --uninstall（「应用和功能」里的卸载按钮就是这么调它的）。
package main

import (
	_ "embed"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"

	"github.com/antapp-cc/antapp-link/internal/setup"
)

//go:embed assets/antapp.ico
var iconData []byte

func main() {
	uninstall := flag.Bool("uninstall", false, "卸载")
	quiet := flag.Bool("quiet", false, "静默模式，不显示界面")
	dirFlag := flag.String("dir", "", "安装目录")
	flag.Parse()

	if *uninstall {
		// 卸载器先把自己切到临时目录再干活：它住在 %ProgramData%，直接删那个目录
		// 会被「不能删除正在运行的 exe」挡住。切换成功后职责就交给副本了。
		switched, err := setup.RelaunchFromTempIfNeeded(func(string) {})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if switched {
			return
		}
	}

	if *quiet {
		os.Exit(runQuiet(*uninstall, *dirFlag))
	}
	os.Exit(runUI(*uninstall, *dirFlag))
}

func runQuiet(uninstall bool, dirFlag string) int {
	if uninstall {
		opts, ok := setup.Installed()
		if !ok {
			fmt.Fprintln(os.Stderr, "没有找到已安装的 AntApp Link")
			return 1
		}
		// 静默卸载是给脚本和自动化用的，默认连数据一起清干净；
		// 走界面的那条路会让用户自己勾选
		opts.RemoveData = true
		if err := setup.Uninstall(opts, func(string) {}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}

	opts := setup.DefaultOptions()
	if dirFlag != "" {
		opts.InstallDir = dirFlag
	}
	if err := setup.Install(opts, func(s string) { fmt.Println(s) }); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func runUI(uninstall bool, dirFlag string) int {
	icon, err := loadIcon()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if uninstall {
		return runUninstallUI(icon)
	}
	return runInstallUI(icon, dirFlag)
}

func loadIcon() (*walk.Icon, error) {
	path := filepath.Join(os.TempDir(), "antapp-setup.ico")
	if err := os.WriteFile(path, iconData, 0o644); err != nil {
		return nil, err
	}
	return walk.NewIconFromFile(path)
}

func runInstallUI(icon *walk.Icon, dirFlag string) int {
	opts := setup.DefaultOptions()
	if dirFlag != "" {
		opts.InstallDir = dirFlag
	}

	var mw *walk.MainWindow
	var edDir *walk.LineEdit
	var chkDesktop, chkStartMenu, chkLaunch *walk.CheckBox
	var btn *walk.PushButton
	var txtLog *walk.TextEdit

	logLine := func(s string) {
		mw.Synchronize(func() { txtLog.AppendText(s + "\r\n") })
	}

	browse := func() {
		dlg := new(walk.FileDialog)
		dlg.Title = "选择安装目录"
		dlg.InitialDirPath = edDir.Text()
		if ok, err := dlg.ShowBrowseFolder(mw); err != nil || !ok {
			return
		}
		edDir.SetText(dlg.FilePath)
	}

	doInstall := func() {
		btn.SetEnabled(false)
		go func() {
			o := opts
			o.InstallDir = edDir.Text()
			o.Desktop = chkDesktop.Checked()
			o.StartMenu = chkStartMenu.Checked()
			o.Launch = chkLaunch.Checked()

			err := setup.Install(o, logLine)
			mw.Synchronize(func() {
				if err != nil {
					txtLog.AppendText("\r\n安装失败：" + err.Error() + "\r\n")
					btn.SetEnabled(true)
					btn.SetText("重试")
					return
				}
				walk.MsgBox(mw, "安装完成",
					"AntApp Link 已安装到：\n"+o.InstallDir+"\n\n"+
						"首次使用请在界面上导入连接码。",
					walk.MsgBoxIconInformation)
				mw.Close()
			})
		}()
	}

	if err := (MainWindow{
		AssignTo: &mw,
		Title:    "安装 AntApp Link",
		Icon:     icon,
		Size:     Size{Width: 600, Height: 540},
		MinSize:  Size{Width: 540, Height: 480},
		Layout:   VBox{Margins: Margins{Left: 16, Top: 16, Right: 16, Bottom: 16}, Spacing: 12},
		Children: []Widget{
			Label{
				Text: "AntApp Link —— Pi 节点虚拟专线",
				Font: Font{PointSize: 11},
			},
			GroupBox{
				Title:  "安装位置",
				Layout: VBox{Margins: Margins{Left: 10, Top: 6, Right: 10, Bottom: 10}, Spacing: 6},
				Children: []Widget{
					Composite{
						Layout: HBox{MarginsZero: true, Spacing: 8},
						Children: []Widget{
							Label{Text: "安装目录"},
							LineEdit{AssignTo: &edDir, Text: opts.InstallDir},
							// 固定宽度：不给 MaxSize 的话 BoxLayout 会把它拉长
							PushButton{Text: "浏览…", MinSize: Size{Width: 78}, MaxSize: Size{Width: 78}, OnClicked: browse},
						},
					},
					Label{Text: "连接码、日志和状态文件放在该目录下的 data 文件夹里。"},
				},
			},
			GroupBox{
				Title:  "安装选项",
				Layout: VBox{Margins: Margins{Left: 10, Top: 6, Right: 10, Bottom: 8}, Spacing: 5},
				Children: []Widget{
					// 每行套一层 HBox + HSpacer 才会左对齐：
					// CheckBox 本身不参与横向拉伸，直接放进 VBox 会被居中
					Composite{
						Layout: HBox{MarginsZero: true},
						Children: []Widget{
							CheckBox{AssignTo: &chkDesktop, Text: "创建桌面快捷方式", Checked: true},
							HSpacer{},
						},
					},
					Composite{
						Layout: HBox{MarginsZero: true},
						Children: []Widget{
							CheckBox{AssignTo: &chkStartMenu, Text: "创建开始菜单快捷方式", Checked: true},
							HSpacer{},
						},
					},
					Composite{
						Layout: HBox{MarginsZero: true},
						Children: []Widget{
							CheckBox{AssignTo: &chkLaunch, Text: "安装完成后立即启动客户端", Checked: true},
							HSpacer{},
						},
					},
				},
			},
			GroupBox{
				Title:  "安装过程",
				Layout: VBox{Margins: Margins{Left: 10, Top: 6, Right: 10, Bottom: 10}},
				Children: []Widget{
					TextEdit{
						AssignTo: &txtLog,
						ReadOnly: true,
						VScroll:  true,
						MinSize:  Size{Height: 110},
						Font:     Font{Family: "Consolas", PointSize: 8},
					},
				},
			},
			Composite{
				Layout: HBox{MarginsZero: true},
				Children: []Widget{
					HSpacer{},
					PushButton{
						AssignTo:  &btn,
						Text:      "开始安装",
						MinSize:   Size{Width: 110},
						MaxSize:   Size{Width: 110},
						OnClicked: doInstall,
					},
				},
			},
		},
	}).Create(); err != nil {
		fmt.Fprintln(os.Stderr, "创建窗口失败:", err)
		return 1
	}
	mw.Run()
	return 0
}

func runUninstallUI(icon *walk.Icon) int {
	opts, ok := setup.Installed()
	if !ok {
		walk.MsgBox(nil, "卸载 AntApp Link",
			"没有找到已安装的 AntApp Link。\n\n如果确实装过，可能安装信息已被清理，可以直接手动删除安装目录。",
			walk.MsgBoxIconWarning)
		return 1
	}
	var mw *walk.MainWindow
	var chkData *walk.CheckBox
	var btn *walk.PushButton
	var txtLog *walk.TextEdit

	logLine := func(s string) {
		mw.Synchronize(func() { txtLog.AppendText(s + "\r\n") })
	}

	doUninstall := func() {
		btn.SetEnabled(false)
		go func() {
			o := opts
			o.RemoveData = chkData.Checked()

			err := setup.Uninstall(o, logLine)
			mw.Synchronize(func() {
				if err != nil {
					txtLog.AppendText("\r\n卸载失败：" + err.Error() + "\r\n")
					btn.SetEnabled(true)
					return
				}
				walk.MsgBox(mw, "卸载完成", "AntApp Link 已卸载。", walk.MsgBoxIconInformation)
				mw.Close()
			})
		}()
	}

	if err := (MainWindow{
		AssignTo: &mw,
		Title:    "卸载 AntApp Link",
		Icon:     icon,
		Size:     Size{Width: 600, Height: 480},
		MinSize:  Size{Width: 540, Height: 430},
		Layout:   VBox{Margins: Margins{Left: 16, Top: 16, Right: 16, Bottom: 16}, Spacing: 12},
		Children: []Widget{
			Label{
				Text: "将删除下面这些内容",
				Font: Font{PointSize: 11},
			},
			GroupBox{
				Title:  "程序与数据",
				Layout: VBox{Margins: Margins{Left: 10, Top: 6, Right: 10, Bottom: 10}, Spacing: 4},
				Children: []Widget{
					Composite{
						Layout: HBox{MarginsZero: true},
						Children: []Widget{
							Label{Text: "程序目录：" + opts.InstallDir},
							HSpacer{},
						},
					},
					Composite{
						Layout: HBox{MarginsZero: true},
						Children: []Widget{
							Label{Text: "数据目录：" + opts.DataDir},
							HSpacer{},
						},
					},
				},
			},
			GroupBox{
				Title:  "卸载选项",
				Layout: VBox{Margins: Margins{Left: 10, Top: 6, Right: 10, Bottom: 8}, Spacing: 5},
				Children: []Widget{
					// 套 HBox + HSpacer 才会左对齐（CheckBox 不参与横向拉伸）
					Composite{
						Layout: HBox{MarginsZero: true},
						Children: []Widget{
							CheckBox{
								AssignTo: &chkData,
								Text:     "同时删除 data 文件夹（连接码与日志）",
								Checked:  true,
							},
							HSpacer{},
						},
					},
					Composite{
						Layout: HBox{MarginsZero: true},
						Children: []Widget{
							Label{Text: "不勾选则保留连接码，重新安装后不用再次导入。"},
							HSpacer{},
						},
					},
				},
			},
			GroupBox{
				Title:  "卸载过程",
				Layout: VBox{Margins: Margins{Left: 10, Top: 6, Right: 10, Bottom: 10}},
				Children: []Widget{
					TextEdit{
						AssignTo: &txtLog,
						ReadOnly: true,
						VScroll:  true,
						MinSize:  Size{Height: 100},
						Font:     Font{Family: "Consolas", PointSize: 8},
					},
				},
			},
			Composite{
				Layout: HBox{MarginsZero: true},
				Children: []Widget{
					HSpacer{},
					PushButton{
						AssignTo:  &btn,
						Text:      "开始卸载",
						MinSize:   Size{Width: 110},
						MaxSize:   Size{Width: 110},
						OnClicked: doUninstall,
					},
				},
			},
		},
	}).Create(); err != nil {
		fmt.Fprintln(os.Stderr, "创建窗口失败:", err)
		return 1
	}
	mw.Run()
	return 0
}
