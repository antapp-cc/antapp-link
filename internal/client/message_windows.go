//go:build windows

package client

import "golang.org/x/sys/windows"

// ShowMessage 弹一个系统对话框。GUI 子系统的程序没有控制台，
// 出错时这是唯一能让用户看见的办法。
func ShowMessage(title, text string) {
	body, err := windows.UTF16PtrFromString(text)
	if err != nil {
		return
	}
	caption, err := windows.UTF16PtrFromString(title)
	if err != nil {
		return
	}
	_, _ = windows.MessageBox(0, body, caption, windows.MB_OK|windows.MB_ICONINFORMATION)
}
