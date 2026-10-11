package client

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/antapp-cc/antapp-link/internal/proto"
)

// 心跳必须写在成员槽自己的连接上（不是控制连接），载荷是 8 字节时间戳。
func TestPingMemberWritesPingFrame(t *testing.T) {
	server, peer := net.Pipe()
	defer peer.Close()
	sess := newClientSession(testTunnel(t), nil, "sid", nil, Ack{Members: 2})
	sess.slots[1] = server

	done := make(chan error, 1)
	go func() { done <- sess.pingMember(1) }()

	_ = peer.SetReadDeadline(time.Now().Add(2 * time.Second))
	typ, payload, err := proto.ReadFrame(peer)
	if err != nil {
		t.Fatalf("读心跳帧: %v", err)
	}
	if typ != proto.TypePing {
		t.Errorf("期望 PING，收到 %s", typ)
	}
	if len(payload) != 8 {
		t.Errorf("PING 载荷应为 8 字节时间戳，实际 %d", len(payload))
	}
	if err := <-done; err != nil {
		t.Errorf("pingMember: %v", err)
	}
}

// 槽空的时候不能去碰控制连接，也不能报错（那时流量本来就落回控制连接了）。
func TestPingMemberSkipsEmptySlot(t *testing.T) {
	ctrl, ctrlPeer := net.Pipe()
	defer ctrlPeer.Close()
	sess := newClientSession(testTunnel(t), ctrl, "sid", nil, Ack{Members: 2})
	// 槽 1 故意留空

	if err := sess.pingMember(1); err != nil {
		t.Errorf("空槽 pingMember 应静默跳过，实际 %v", err)
	}
	// 控制连接上不该出现任何字节
	_ = ctrlPeer.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	buf := make([]byte, 16)
	if n, _ := ctrlPeer.Read(buf); n != 0 {
		t.Errorf("空槽不该往控制连接写心跳，实际写了 %d 字节", n)
	}
}

// 会话收尾（closed 信号）时心跳协程必须退出，否则每补拨一次就漏一个协程。
func TestMemberHeartbeatExitsOnSessionClose(t *testing.T) {
	sess := newClientSession(testTunnel(t), nil, "sid", nil, Ack{Members: 2})
	done := make(chan struct{})
	go func() {
		sess.memberHeartbeat(context.Background(), 1)
		close(done)
	}()
	sess.closeAll()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("会话关闭后心跳协程没退出")
	}
}
