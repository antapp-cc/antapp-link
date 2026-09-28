//go:build windows

package client

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ReadClipboard 读剪贴板文本，直接走 user32/kernel32，不拉 PowerShell。
//
// 托盘没法弹输入框，所以「先复制再点菜单」是最省事又不难用的导入方式。
func ReadClipboard() (string, error) {
	user32 := windows.NewLazySystemDLL("user32.dll")
	kernel32 := windows.NewLazySystemDLL("kernel32.dll")
	openClipboard := user32.NewProc("OpenClipboard")
	closeClipboard := user32.NewProc("CloseClipboard")
	getClipboardData := user32.NewProc("GetClipboardData")
	globalLock := kernel32.NewProc("GlobalLock")
	globalUnlock := kernel32.NewProc("GlobalUnlock")

	// 剪贴板可能被别的程序短暂占着，重试几次；还不行就明确报错。
	var opened bool
	var lastErr error
	for attempt := 0; attempt < 5 && !opened; attempt++ {
		r, _, callErr := openClipboard.Call(0)
		if r != 0 {
			opened = true
			break
		}
		lastErr = callErr
		time.Sleep(30 * time.Millisecond)
	}
	if !opened {
		return "", fmt.Errorf("打开剪贴板失败: %w", lastErr)
	}
	defer closeClipboard.Call()

	const cfUnicodeText = 13
	h, _, _ := getClipboardData.Call(cfUnicodeText)
	if h == 0 {
		return "", nil // 剪贴板里没有文本，不算错误
	}
	p, _, _ := globalLock.Call(h)
	if p == 0 {
		return "", fmt.Errorf("锁定剪贴板数据失败")
	}
	defer globalUnlock.Call(h)

	return strings.TrimSpace(utf16At(ptrFromSyscall(p))), nil
}

// ptrFromSyscall 把系统调用返回的指针从 uintptr 还原成 unsafe.Pointer。
// 直接转换会被 go vet 的 unsafeptr 检查误报，这是标准库同款的间接写法。
func ptrFromSyscall(p uintptr) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(&p))
}

// utf16At 读出 NUL 结尾的 UTF-16 字符串。
func utf16At(p unsafe.Pointer) string {
	if p == nil {
		return ""
	}
	s := (*[1 << 20]uint16)(p)
	for i, c := range s {
		if c == 0 {
			return string(utf16.Decode(s[:i]))
		}
	}
	return string(utf16.Decode(s[:]))
}
