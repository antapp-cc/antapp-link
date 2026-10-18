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
		sess.memberHeartbeat(context.Background(), 1, nil)
		close(done)
	}()
	sess.closeAll()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("会话关闭后心跳协程没退出")
	}
}

// 槽位被摘掉后心跳也必须退出。
//
// 原来 memberHeartbeat 调 pingMember，而 pingMember 在空槽上返回 nil（"没什么可
// 保活的"），循环因此永不退出 —— 每摘一次槽就漏一个空转协程，一晚上能攒一堆。
// 报告 5.2 用缩短心跳间隔的方式复现过这一条。
func TestMemberHeartbeatExitsWhenSlotRemoved(t *testing.T) {
	old := memberPingInterval
	memberPingInterval = 50 * time.Millisecond
	t.Cleanup(func() { memberPingInterval = old })

	server, peer := net.Pipe()
	defer peer.Close()
	defer server.Close()

	sess := newClientSession(testTunnel(t), nil, "sid", nil, Ack{Members: 2})
	sess.slots[1] = server

	// net.Pipe 是同步无缓冲的：对端不读，pingMember 的写会一直阻塞到
	// slotWriteTimeout（20 秒），测试根本等不到槽检查那一步。这里把帧吃掉。
	go func() {
		buf := make([]byte, 64)
		for {
			if _, err := peer.Read(buf); err != nil {
				return
			}
		}
	}()

	done := make(chan struct{})
	go func() {
		sess.memberHeartbeat(context.Background(), 1, server)
		close(done)
	}()

	// 等第一次心跳过去（确认它真的在跑），再模拟读循环摘槽
	time.Sleep(120 * time.Millisecond)
	sess.mu.Lock()
	sess.slots[1] = nil
	sess.mu.Unlock()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("槽位移除后心跳协程没退出（会积累空转协程）")
	}
}
