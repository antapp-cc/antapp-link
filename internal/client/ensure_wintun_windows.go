//go:build windows

package client

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
)

// EnsureWintunDLL 把内嵌的 wintun.dll 释放到 exe 同目录，返回它的路径。
//
// 为什么必须是 exe 同目录：wintun 的 Go 绑定内部调用的是
//
//	LoadLibraryEx(name, 0, LOAD_LIBRARY_SEARCH_APPLICATION_DIR|LOAD_LIBRARY_SEARCH_SYSTEM32)
//
// 搜索范围只有「应用程序目录」和 System32 —— PATH 和当前工作目录都不在其中。
// 所以放到 %ProgramData% 之类的地方是没用的。
//
// 已经释放过且内容一致时直接跳过，避免每次启动都写一遍磁盘。
func EnsureWintunDLL() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("取程序路径: %w", err)
	}
	dir := filepath.Dir(exe)
	target := filepath.Join(dir, "wintun.dll")

	if len(wintunDLL) == 0 {
		// 架构没内嵌 dll：这时只能指望用户自己放一份，给出可操作的提示
		if _, err := os.Stat(target); err == nil {
			return target, nil
		}
		return "", fmt.Errorf("本架构没有内嵌 wintun.dll，请把 wintun.dll 放到 %s", dir)
	}

	want := sha256.Sum256(wintunDLL)
	if existing, err := os.ReadFile(target); err == nil && sha256.Sum256(existing) == want {
		return target, nil
	}

	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, wintunDLL, 0o644); err != nil {
		return "", fmt.Errorf("写出 wintun.dll 到 %s 失败（该目录需要可写）: %w", dir, err)
	}
	if err := os.Rename(tmp, target); err != nil {
		return "", fmt.Errorf("落盘 %s: %w", target, err)
	}
	return target, nil
}
