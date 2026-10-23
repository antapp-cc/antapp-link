//go:build !linux

package server

import "net"

// setNotsentLowat 只在 Linux 上有意义（服务端只跑 Linux），其它平台什么都不做。
func setNotsentLowat(*net.TCPConn, int) error { return nil }
