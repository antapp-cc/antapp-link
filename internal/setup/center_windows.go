//go:build windows

package setup

import (
	"unsafe"

	"github.com/lxn/win"
)

// CenterOnScreen 把窗口挪到屏幕工作区（去掉任务栏那块）的正中。
//
// 不指定位置时 Windows 按「层叠」摆放，每开一次就偏一点 —— 主窗口、安装向导、
// 卸载向导都该固定在中间，用户不用去屏幕上找它在哪。
func CenterOnScreen(hwnd win.HWND) {
	if hwnd == 0 {
		return
	}
	const spiGetWorkArea = 0x0030
	var work win.RECT
	if !win.SystemParametersInfo(spiGetWorkArea, 0, unsafe.Pointer(&work), 0) {
		return
	}
	var wr win.RECT
	if !win.GetWindowRect(hwnd, &wr) {
		return
	}
	x := work.Left + (work.Right-work.Left-(wr.Right-wr.Left))/2
	y := work.Top + (work.Bottom-work.Top-(wr.Bottom-wr.Top))/2
	win.SetWindowPos(hwnd, 0, x, y, 0, 0, win.SWP_NOSIZE|win.SWP_NOZORDER|win.SWP_NOACTIVATE)
}
