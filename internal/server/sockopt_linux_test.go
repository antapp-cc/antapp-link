//go:build linux

package server

import (
	"net"
	"testing"

	"golang.org/x/sys/unix"
)

// tuneTCPBuffers 必须真的把 TCP_NOTSENT_LOWAT 设到连接上，而不只是算出一个数。
func TestTuneTCPBuffersSetsNotsentLowat(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			defer c.Close()
			buf := make([]byte, 1)
			_, _ = c.Read(buf)
		}
	}()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	tuneTCPBuffers(conn)

	raw, err := conn.(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Fatalf("SyscallConn: %v", err)
	}
	var got int
	var gerr error
	if err := raw.Control(func(fd uintptr) {
		got, gerr = unix.GetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_NOTSENT_LOWAT)
	}); err != nil {
		t.Fatalf("Control: %v", err)
	}
	if gerr != nil {
		t.Fatalf("getsockopt TCP_NOTSENT_LOWAT: %v", gerr)
	}
	if got != notsentLowat {
		t.Fatalf("TCP_NOTSENT_LOWAT = %d, want %d", got, notsentLowat)
	}
}
