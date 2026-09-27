//go:build !windows

package setup

// 安装器只在 Windows 上有界面，这里给个空实现让包能编译。
func CenterOnScreen(uintptr) {}
