//go:build !windows

package update

import "errors"

var ErrNotWindows = errors.New("update: 自动替换程序只在 Windows 上可用")

func Apply(string) error { return ErrNotWindows }

func CleanupOld() {}
