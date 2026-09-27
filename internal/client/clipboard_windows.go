//go:build windows

package client

import (
	"fmt"
	"strings"
)

// ReadClipboard 读剪贴板文本。
//
// 走 PowerShell 的 Get-Clipboard 而不是自己写 OpenClipboard 那一套：
// 少一份 win32 句柄管理，出错面也小。托盘没法弹输入框，所以「先复制再点菜单」
// 是最省事又不难用的导入方式。
func ReadClipboard() (string, error) {
	out, err := runPowerShell("Get-Clipboard -Raw")
	if err != nil {
		return "", fmt.Errorf("读剪贴板: %w", err)
	}
	return strings.TrimSpace(out), nil
}
