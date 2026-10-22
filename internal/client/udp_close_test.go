package client

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"
)

// newTestUDPChannel 造一个只用于"关闭"语义的 UDP 通道：真实 socket +
// 未启动的收发协程。fail/close 都会关底层连接，所以不能拿 nil 顶替。
func newTestUDPChannel(t *testing.T) *udpChannel {
	t.Helper()

	srv, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("监听回环 UDP: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	conn, err := net.DialUDP("udp", nil, srv.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatalf("拨号回环 UDP: %v", err)
	}
	c := &udpChannel{
		conn: conn, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		closed: make(chan struct{}),
	}
	c.lastRx.Store(time.Now().UnixNano())
	t.Cleanup(c.close)
	return c
}

// UDP 通道被动失效时必须由会话自己收尾，不能等下一个上行包。
//
// keepalive 判定路径已死会关掉通道，但修复前只有 pumpFromDevice 里 send 失败才会
// 结束会话：空闲的隧道 TCP 心跳正常、面板显示「已连接」，业务包却全灌进死路径，
// 直到用户下次上网才暴露。这里连一个上行包都不发，会话仍必须在毫秒级结束。
func TestSessionEndsWhenUDPChannelFails(t *testing.T) {
	ctrl, peer := net.Pipe()
	defer peer.Close()

	sess := newClientSession(testTunnel(t), ctrl, "sid", nil, Ack{})
	sess.udp = newTestUDPChannel(t)
	defer sess.closeAll()

	go func() {
		time.Sleep(50 * time.Millisecond)
		sess.udp.fail(errors.New("UDP 数据通道连续 45s 收不到回包"))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	err := sess.run(ctx)
	if err == nil {
		t.Fatal("UDP 通道被动关闭后会话必须结束（没有上行包也不能一直挂着）")
	}
	if !strings.Contains(err.Error(), "UDP") {
		t.Fatalf("会话错误应说明 UDP 通道失效，实际 %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("UDP 关闭后会话拖了 %v 才结束，没有做到立即收尾", elapsed)
	}
}

// 主动收尾（手动断开）关闭 UDP 通道时不该触发重建：那会让「用户点了断开」
// 变成「断开后自己又连上」。只有 fail(...) 标记过的关闭才算故障。
func TestUDPChannelIntentionalCloseKeepsSession(t *testing.T) {
	ctrl, peer := net.Pipe()
	defer peer.Close()

	sess := newClientSession(testTunnel(t), ctrl, "sid", nil, Ack{})
	sess.udp = newTestUDPChannel(t)
	defer sess.closeAll()

	go func() {
		time.Sleep(50 * time.Millisecond)
		sess.udp.close() // 与 session 收尾时一样：没有失效原因
	}()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sess.run(ctx) }()

	select {
	case err := <-done:
		t.Fatalf("主动关闭 UDP 不该结束会话，实际返回 %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ctx 取消后会话应正常收尾，实际 %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 取消后会话没有退出")
	}
}
