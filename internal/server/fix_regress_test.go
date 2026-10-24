package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"
)

// 回 HELLO_ACK 失败时会话还没 attach，必须自己关掉：newSlotConn 已经起了写协程，
// 原来这里直接 return，每次失败漏一个 goroutine。
func TestHandleControlAckFailureReleasesWriter(t *testing.T) {
	s, _ := newTestServer(t, newHandshakeLimiter(1))
	before := runtime.NumGoroutine()
	for i := 0; i < 20; i++ {
		srv, cli := net.Pipe()
		_ = cli.Close() // 对端已关：回 HELLO_ACK 必然失败
		s.handleControl(context.Background(), tls.Server(srv, s.tlsCfg), "c", helloPayload{Version: protoVersion})
		_ = srv.Close()
	}
	waitFor(t, 2*time.Second, func() bool { return runtime.NumGoroutine() <= before+3 },
		"回 HELLO_ACK 失败后写协程没有退出（goroutine 泄漏）")
}

// 同一会话重复上报 UDP 端口必须忽略：同一 TLS 会话导出的密钥固定，再建 Cipher
// 序号从 0 重来，AES-GCM nonce 就重复了。
func TestRegisterUDPIgnoresDuplicateReport(t *testing.T) {
	s, cliCfg := newTestServer(t, newHandshakeLimiter(1))
	m, err := newUDPMux("127.0.0.1:0", nil, discardLogger())
	if err != nil {
		t.Fatalf("newUDPMux: %v", err)
	}
	t.Cleanup(m.close)
	s.udp = m

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		c, err := tls.Dial("tcp", ln.Addr().String(), cliCfg)
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = io.Copy(io.Discard, c)
	}()
	raw, err := ln.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	tc := tls.Server(raw, s.tlsCfg)
	_ = tc.SetDeadline(time.Now().Add(10 * time.Second))
	if err := tc.Handshake(); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	_ = tc.SetDeadline(time.Time{})

	sess := &session{name: "c", closed: make(chan struct{}), tlsConn: tc}
	sess.slots = []*slotConn{newSlotConn(tc)}
	t.Cleanup(sess.close)
	// registerUDP 只处理当前会话（真实路径：控制连接读循环里调用）。
	s.attach(sess)

	s.registerUDP(sess, []byte{0x30, 0x39})
	first := sess.udpSession()
	if first == nil {
		t.Fatal("首次上报应登记 UDP 会话")
	}
	s.registerUDP(sess, []byte{0x30, 0x3a})
	if sess.udpSession() != first {
		t.Fatal("重复上报替换了 UDP 会话（新 Cipher 序号归零，nonce 复用）")
	}
	if got := m.lookup(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}); got != first {
		t.Fatal("重复上报改写了 UDP 多路复用表")
	}
}

// 隧道端口对公网开放，扫描器的握手失败不能逐条 Warn。
func TestHandshakeFailureLogSampled(t *testing.T) {
	s, _ := newTestServer(t, newHandshakeLimiter(4))
	var buf bytes.Buffer
	s.log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	for i := 0; i < 5; i++ {
		srv, cli := net.Pipe()
		_ = cli.Close()
		if _, _, _, ok := s.handshake(context.Background(), srv); ok {
			t.Fatal("对端已关，握手不应成功")
		}
		_ = srv.Close()
	}
	if n := strings.Count(buf.String(), "TLS 握手失败"); n != 1 {
		t.Fatalf("5 次握手失败应只出 1 条 Warn，实际 %d 条", n)
	}
	if got := s.hsFails.Load(); got != 5 {
		t.Fatalf("失败计数 = %d, want 5", got)
	}
}

// 会话已经被新连接取代时（不再是 current），registerUDP 必须直接拒绝：
// detach 已经跑过且当时 sess.udp 还是 nil，这条登记写进 mux 就再没人回收。
func TestRegisterUDPRejectsStaleSession(t *testing.T) {
	s, _ := newTestServer(t, newHandshakeLimiter(1))
	m, err := newUDPMux("127.0.0.1:0", nil, discardLogger())
	if err != nil {
		t.Fatalf("newUDPMux: %v", err)
	}
	t.Cleanup(m.close)
	s.udp = m

	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	stale := &session{name: "stale", closed: make(chan struct{})}
	stale.slots = []*slotConn{newSlotConn(a)}
	t.Cleanup(stale.close)

	current := &session{name: "current", closed: make(chan struct{})}
	t.Cleanup(current.close)
	s.attach(current)

	s.registerUDP(stale, []byte{0x30, 0x39})
	if stale.udpSession() != nil {
		t.Fatal("被取代的会话不该登记 UDP 通道")
	}
	if got := m.lookup(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}); got != nil {
		t.Fatal("被取代的会话不该写进 mux 的 map")
	}
}
