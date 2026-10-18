//go:build windows

package client

import (
	"fmt"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	// Global\ 前缀让它跨会话唯一。客户端本来就要求管理员，创建全局对象有权限。
	instanceMutexName = `Global\AntAppLink-Client`

	// mainWindowTitle 要和 MainWindow 的 Title 保持一致 —— ActivateExisting
	// 靠它找窗口。
	mainWindowTitle = "AntApp Link"
)

// SingleInstance 用命名互斥体保证同一时间只有一个客户端在跑。
//
// 少了这道闸，桌面快捷方式点几次就起几个实例：托盘上堆一排图标，更要紧的是
// 几个实例会同时去抢同一块虚拟网卡和同一批路由，把网络搅乱。
//
// 拿不到锁时会短暂重试：自动更新是「先拉起新进程、再退出旧进程」（见
// update/apply_windows.go 的 swapExe），新进程启动时旧进程往往还在 —— 不等这一下，
// 新进程会被自己的闸门挡回去，而旧进程紧接着退出，结果更新完一个实例都不剩。
//
// 返回 ok=false 表示确实已有实例在跑（不是刚退出）。ok=true 时调用方退出前要 release。
func SingleInstance() (release func(), ok bool, err error) {
	return acquireInstance(instanceMutexName)
}

// acquireInstance 是 SingleInstance 的可测部分：名字由调用方给，测试才能用临时
// 名字验证交接行为，不去碰真实实例的锁。
func acquireInstance(name string) (release func(), ok bool, err error) {
	utf16Name, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, false, err
	}

	// 4 × 300ms：够等正在退出的旧进程释放互斥体，又不会让「真的已有实例」这种
	// 最常见的情况等太久。
	const attempts = 4
	for i := 0; i < attempts; i++ {
		h, cerr := windows.CreateMutex(nil, false, utf16Name)
		if cerr == nil {
			// 句柄不能提前关：关掉就等于放锁，后来的实例又会以为自己是第一个
			return func() { _ = windows.CloseHandle(h) }, true, nil
		}
		if cerr != windows.ERROR_ALREADY_EXISTS {
			return nil, false, fmt.Errorf("创建互斥体: %w", cerr)
		}

		// ERROR_ALREADY_EXISTS 时 h 仍是本次打开的有效句柄，必须关掉。
		//
		// 不关的后果是自动更新后整个客户端消失：新进程第一次检查时旧进程还在，
		// 拿到已存在对象的句柄却没关 —— 旧进程随后退出，可对象仍被这个句柄撑着。
		// 后面每次重试都还是 ERROR_ALREADY_EXISTS（自己卡的自己），4 次全失败后
		// 走 ActivateExisting + return 0，而旧进程已经死了：一个实例都不剩。
		//
		// 这个窗口只有几十毫秒，所以只在部分电脑上出现（取决于新旧进程的启动速度差）。
		_ = windows.CloseHandle(h)

		if i < attempts-1 {
			time.Sleep(300 * time.Millisecond)
		}
	}
	return nil, false, nil
}

// ActivateExisting 把已经在跑的那个实例叫到前台来。
//
// 按窗口标题找 —— 标题是我们自己定的常量，不会跟别的程序撞。
// x/sys/windows 没封装这几个 user32 函数，所以自己取。
func ActivateExisting() bool {
	title, err := windows.UTF16PtrFromString(mainWindowTitle)
	if err != nil {
		return false
	}

	user32 := windows.NewLazySystemDLL("user32.dll")
	hwnd, _, _ := user32.NewProc("FindWindowW").Call(0, uintptr(unsafe.Pointer(title)))
	if hwnd == 0 {
		return false
	}

	// 可能被最小化或藏在托盘后面，先还原再抢焦点
	const swRestore = 9
	user32.NewProc("ShowWindow").Call(hwnd, swRestore)
	user32.NewProc("SetForegroundWindow").Call(hwnd)
	return true
}
