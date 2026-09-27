//go:build windows

package client

import (
	"fmt"
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
// 返回 ok=false 表示已经有实例在跑。ok=true 时，调用方在退出前要调用 release。
func SingleInstance() (release func(), ok bool, err error) {
	name, err := windows.UTF16PtrFromString(instanceMutexName)
	if err != nil {
		return nil, false, err
	}

	h, err := windows.CreateMutex(nil, false, name)
	if err != nil {
		// 已存在时 CreateMutex 会把 ERROR_ALREADY_EXISTS 当错误返回
		if err == windows.ERROR_ALREADY_EXISTS {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("创建互斥体: %w", err)
	}
	// 句柄不能提前关：关掉就等于放锁，后来的实例又会以为自己是第一个
	return func() { _ = windows.CloseHandle(h) }, true, nil
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
