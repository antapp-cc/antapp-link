//go:build linux

package server

import (
	"net"

	"golang.org/x/sys/unix"
)

// setNotsentLowat 给这条连接设 TCP_NOTSENT_LOWAT（Linux 3.12+）。
func setNotsentLowat(c *net.TCPConn, n int) error {
	raw, err := c.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	if err := raw.Control(func(fd uintptr) {
		serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_NOTSENT_LOWAT, n)
	}); err != nil {
		return err
	}
	return serr
}
