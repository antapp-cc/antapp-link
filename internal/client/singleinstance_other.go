//go:build !windows

package client

// 非 Windows 平台没有客户端界面，这两个函数只是让包能编译过。
func SingleInstance() (func(), bool, error) { return func() {}, true, nil }

func ActivateExisting() bool { return false }
