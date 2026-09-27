//go:build windows

// Package setup 实现 AntApp Link 的安装与卸载：释放程序、建快捷方式、
// 登记到「应用和功能」，并且能被同一份代码反过来卸载干净。
package setup

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows/registry"
)

const (
	AppName       = "AntApp Link"
	AppExeName    = "antapp-link.exe"
	AutostartTask = "AntAppLink"

	// UninstallerName 是卸载器。安装时把安装程序自己复制一份留在这里 ——
	// 用户删掉当初的安装包之后，「应用和功能」和客户端里的卸载入口还得能用。
	//
	// 它住在 %ProgramData% 而不是安装目录：卸载器要删掉整个安装目录，而 Windows
	// 不允许删除正在运行的 exe —— 放在安装目录里必定留个尾巴删不掉。
	UninstallerName = "uninstall.exe"

	// UninstallLinkName 是放在安装目录里的卸载快捷方式。刻意不放客户端托盘的
	// 右键菜单里 —— 那里紧挨着「退出」，而卸载是不可逆的，太容易点错。
	UninstallLinkName = "卸载 AntApp Link.lnk"

	uninstallKey = `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\AntApp Link`
)

// Version 由构建脚本用 -ldflags -X 注入。
var Version = "0.1.0"

const createNoWindow = 0x08000000

func hiddenProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}

func runHidden(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = hiddenProcAttr()
	return cmd.Run()
}

func runHiddenOutput(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = hiddenProcAttr()
	out, err := cmd.Output()
	return string(out), err
}

type Options struct {
	InstallDir string
	SourceDir  string
	DataDir    string

	StartMenu bool
	Desktop   bool
	Launch    bool

	// RemoveData 只用于卸载：连 %ProgramData%\AntAppLink 里的连接码和日志一起删。
	RemoveData bool
}

func DefaultOptions() Options {
	self, err := os.Executable()
	srcDir := "."
	if err == nil {
		srcDir = filepath.Dir(self)
	}
	return Options{
		InstallDir: DefaultInstallDir(),
		SourceDir:  srcDir,
		DataDir:    DefaultDataDir(),
		StartMenu:  true,
		Desktop:    true,
		Launch:     true,
	}
}

func DefaultInstallDir() string {
	if pf := os.Getenv("ProgramFiles"); pf != "" {
		return filepath.Join(pf, AppName)
	}
	return `C:\Program Files\` + AppName
}

// DefaultDataDir 与客户端保持一致：数据就放在安装目录下的 data 里，
// 用户翻安装目录就能看到连接码、日志和状态文件。
func DefaultDataDir() string {
	return filepath.Join(DefaultInstallDir(), "data")
}

// fallbackDataDir 是客户端在安装目录写不进去时会退回的位置，卸载时一并清掉。
func fallbackDataDir() string {
	if pd := os.Getenv("ProgramData"); pd != "" {
		return filepath.Join(pd, "AntAppLink")
	}
	return filepath.Join(os.Getenv("LOCALAPPDATA"), "AntAppLink")
}

// UninstallerDir 是卸载器的落脚处。
//
// 刻意不放在安装目录里：卸载器要删掉整个安装目录，而 Windows 不允许删除正在运行的
// exe —— 放在那儿必定剩下一个 uninstall.exe 删不掉，卸载就永远「不干净」。
func UninstallerDir() string {
	if pd := os.Getenv("ProgramData"); pd != "" {
		return filepath.Join(pd, AppName)
	}
	return filepath.Join(os.Getenv("LOCALAPPDATA"), AppName)
}

// UninstallerPath 是卸载器的完整路径。
func UninstallerPath() string {
	return filepath.Join(UninstallerDir(), UninstallerName)
}

// Installed 从注册表读回已安装的信息。
func Installed() (Options, bool) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, uninstallKey, registry.QUERY_VALUE)
	if err != nil {
		return Options{}, false
	}
	defer k.Close()

	loc, _, err := k.GetStringValue("InstallLocation")
	if err != nil || loc == "" {
		return Options{}, false
	}
	return Options{InstallDir: loc, DataDir: filepath.Join(loc, "data")}, true
}

func Install(opts Options, log func(string)) error {
	src := filepath.Join(opts.SourceDir, AppExeName)
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("安装包里找不到 %s（它需要和安装程序放在同一个目录）: %w", AppExeName, err)
	}

	// 先停掉正在跑的客户端，否则程序文件被占用、覆盖会失败
	log("停止正在运行的客户端")
	stopClient()

	log("创建安装目录 " + opts.InstallDir)
	if err := os.MkdirAll(opts.InstallDir, 0o755); err != nil {
		return fmt.Errorf("创建安装目录: %w", err)
	}

	dst := filepath.Join(opts.InstallDir, AppExeName)
	if err := copyFile(src, dst); err != nil {
		return fmt.Errorf("拷贝程序: %w", err)
	}
	log("已释放 " + dst)

	if err := installUninstaller(log); err != nil {
		return err
	}

	// 卸载入口就放在安装目录里，用户想卸载时翻进来点一下
	uninstLink := filepath.Join(opts.InstallDir, UninstallLinkName)
	if err := createShortcut(uninstLink, UninstallerPath(), opts.InstallDir, "--uninstall"); err != nil {
		log("卸载快捷方式创建失败（仍可从「应用和功能」卸载）：" + err.Error())
	} else {
		log("已创建卸载快捷方式 " + uninstLink)
	}

	if opts.StartMenu {
		if err := createShortcut(startMenuLink(), dst, opts.InstallDir); err != nil {
			log("开始菜单快捷方式创建失败（不影响使用）：" + err.Error())
		} else {
			log("已创建开始菜单快捷方式")
		}
	}
	if opts.Desktop {
		if err := createShortcut(desktopLink(), dst, opts.InstallDir); err != nil {
			log("桌面快捷方式创建失败（不影响使用）：" + err.Error())
		} else {
			log("已创建桌面快捷方式")
		}
	}

	if err := writeUninstallEntry(opts, dst); err != nil {
		return fmt.Errorf("登记卸载信息失败: %w", err)
	}
	log("已登记到「应用和功能」，可从那里卸载")

	if opts.Launch {
		log("启动客户端")
		if err := exec.Command(dst).Start(); err != nil {
			log("启动失败（可以自己双击快捷方式）：" + err.Error())
		}
	}
	return nil
}

func Uninstall(opts Options, log func(string)) error {
	log("停止客户端")
	stopClient()

	// 计划任务留着的话，卸载后每次登录还会去启动一个已经不存在的程序
	log("清理开机自启计划任务")
	_ = runHidden("schtasks", "/delete", "/tn", AutostartTask, "/f")

	log("删除快捷方式")
	for _, link := range []string{
		startMenuLink(),
		desktopLink(),
		filepath.Join(opts.InstallDir, UninstallLinkName),
	} {
		if err := os.Remove(link); err == nil {
			log("  已删除 " + link)
		}
	}

	log("删除程序文件")
	dataDir := filepath.Join(opts.InstallDir, "data")
	if opts.RemoveData {
		log("  删除数据目录 " + dataDir)
		_ = os.RemoveAll(dataDir)
	} else {
		log("  保留数据目录 " + dataDir + "（连接码与日志）")
	}
	_ = os.Remove(filepath.Join(opts.InstallDir, AppExeName))
	_ = os.Remove(filepath.Join(opts.InstallDir, "wintun.dll"))
	// 目录非空时会失败，忽略即可 —— 不强行删掉用户自己放进去的东西
	_ = os.Remove(opts.InstallDir)

	log("清理注册表卸载信息")
	if err := registry.DeleteKey(registry.LOCAL_MACHINE, uninstallKey); err != nil {
		log("  删除失败（可能本来就没有）：" + err.Error())
	}

	// 客户端在安装目录写不进去时会把数据退回到这里，顺手一起清掉
	if fb := fallbackDataDir(); fb != "" {
		if _, err := os.Stat(fb); err == nil {
			log("清理回退数据目录 " + fb)
			_ = os.RemoveAll(fb)
		}
	}

	// 卸载器自己住在 %ProgramData%，而此刻跑的是它在临时目录里的副本 ——
	// 所以这块能连卸载器一起删干净。这正是把它搬出安装目录的意义。
	if dir := UninstallerDir(); dir != "" {
		log("清理卸载器目录 " + dir)
		if err := os.RemoveAll(dir); err != nil {
			log("  删除失败（重启后可手动清理）：" + err.Error())
		}
	}
	return nil
}

// installUninstaller 把安装程序自己复制一份到 %ProgramData%，充当卸载器。
func installUninstaller(log func(string)) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("取安装程序路径: %w", err)
	}
	target := UninstallerPath()
	if strings.EqualFold(self, target) {
		return nil // 已经在目标位置，说明是卸载器在跑
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("创建卸载器目录: %w", err)
	}
	if err := copyFile(self, target); err != nil {
		return fmt.Errorf("释放卸载程序: %w", err)
	}
	log("已释放 " + target)
	return nil
}

// RelaunchFromTempIfNeeded 检查自己是不是在临时目录里跑，不是就切过去。
//
// 卸载要删掉 %ProgramData%\AntApp Link 整个目录，而卸载器自己就住在那里 ——
// Windows 不允许删除正在运行的 exe，直接删必定失败。所以先把自己复制到临时目录、
// 用副本重跑一遍，原进程立刻退出，副本就再没有「自己挡自己的路」这回事。
//
// 返回 true 表示已经切换，调用方必须马上退出（活儿交给副本了）。
func RelaunchFromTempIfNeeded(log func(string)) (bool, error) {
	self, err := os.Executable()
	if err != nil {
		return false, fmt.Errorf("取当前程序路径: %w", err)
	}

	tmp := filepath.Join(os.TempDir(), "antapp-uninstall.exe")
	if strings.EqualFold(self, tmp) {
		// 我就是那个副本。先等原进程退出 —— 它正占着 %ProgramData% 里那个 exe，
		// 不等的话接下来删目录会失败。
		time.Sleep(1200 * time.Millisecond)
		return false, nil
	}

	if err := copyFile(self, tmp); err != nil {
		return false, fmt.Errorf("复制卸载程序到临时目录失败: %w", err)
	}
	// 参数原样带过去（--uninstall、--quiet 之类）
	cmd := exec.Command(tmp, os.Args[1:]...)
	if err := cmd.Start(); err != nil {
		return false, fmt.Errorf("启动临时目录里的卸载程序失败: %w", err)
	}
	log("已切换到临时目录里的副本：" + tmp)
	return true, nil
}

func stopClient() {
	_ = runHidden("taskkill", "/IM", AppExeName, "/F")
	// taskkill 返回后文件句柄未必立刻释放，等一小会儿再动文件
	time.Sleep(600 * time.Millisecond)
}

func copyFile(src, dst string) error {
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

func startMenuLink() string {
	base := os.Getenv("ProgramData")
	if base == "" {
		base = `C:\ProgramData`
	}
	return filepath.Join(base, `Microsoft\Windows\Start Menu\Programs`, AppName+".lnk")
}

// 用公共桌面：装在哪个账户下都能看到同一个快捷方式
func desktopLink() string {
	base := os.Getenv("PUBLIC")
	if base == "" {
		base = `C:\Users\Public`
	}
	return filepath.Join(base, "Desktop", AppName+".lnk")
}

func createShortcut(lnkPath, target, workDir string, args ...string) error {
	if err := os.MkdirAll(filepath.Dir(lnkPath), 0o755); err != nil {
		return err
	}
	q := func(s string) string { return strings.ReplaceAll(s, "'", "''") }
	script := fmt.Sprintf(
		`$ws = New-Object -ComObject WScript.Shell; `+
			`$s = $ws.CreateShortcut('%s'); `+
			`$s.TargetPath = '%s'; $s.WorkingDirectory = '%s'; `+
			`$s.Arguments = '%s'; `+
			`$s.IconLocation = '%s,0'; $s.Save()`,
		q(lnkPath), q(target), q(workDir), q(strings.Join(args, " ")), q(target))
	_, err := runHiddenOutput("powershell", "-NoProfile", "-NonInteractive",
		"-ExecutionPolicy", "Bypass", "-Command", script)
	return err
}

func writeUninstallEntry(opts Options, exePath string) error {
	k, _, err := registry.CreateKey(registry.LOCAL_MACHINE, uninstallKey, registry.WRITE)
	if err != nil {
		return err
	}
	defer k.Close()

	// 指向 %ProgramData% 里那份卸载器，而不是当前这个安装包 ——
	// 用户很可能早把安装包删了，那样「应用和功能」里的卸载按钮就废了
	uninst := UninstallerPath()

	values := map[string]string{
		"DisplayName":     AppName,
		"DisplayVersion":  Version,
		"Publisher":       "AntApp",
		"InstallLocation": opts.InstallDir,
		"DisplayIcon":     exePath,
		"UninstallString": fmt.Sprintf(`"%s" --uninstall`, uninst),
	}
	for name, v := range values {
		if err := k.SetStringValue(name, v); err != nil {
			return err
		}
	}
	if err := k.SetDWordValue("NoModify", 1); err != nil {
		return err
	}
	return k.SetDWordValue("NoRepair", 1)
}
