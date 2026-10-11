//go:build !windows && !linux

package update

import "errors"

var ErrNotSupported = errors.New("update: 自动替换程序只在 Windows 和 Linux 上可用")

func Apply(string) error { return ErrNotSupported }

func CleanupOld() {}
