//go:build !windows && !linux

package update

import "errors"

var ErrNotSupported = errors.New("update: 自动替换程序只在 Windows 和 Linux 上可用")

func Apply(string) error { return ErrNotSupported }

// CleanupOld / CleanupOldAfterHealthy 服务于 Windows 的 .old 回滚副本机制。
// 两个符号在所有平台都提供空实现，调用点才不用写平台分支。
func CleanupOld() {}

func CleanupOldAfterHealthy() {}
