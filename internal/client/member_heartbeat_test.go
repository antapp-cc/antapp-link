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

// 下行单向死掉的成员连接必须被摘掉：上行写还成功，但 PONG/数据一个都回不来。
// 不摘的话槽位一直"活着"继续接流，分到它的流全部黑洞，界面却仍显示已连接。
func TestMemberHeartbeatDropsSlotWhenDownlinkSilent(t *testing.T) {
	oldPing, oldDead := memberPingInterval, memberDeadAfter
	memberPingInterval = 20 * time.Millisecond
	memberDeadAfter = 30 * time.Millisecond
	t.Cleanup(func() { memberPingInterval, memberDeadAfter = oldPing, oldDead })

	server, peer := net.Pipe()
	defer peer.Close()
	defer server.Close()

	sess := newClientSession(testTunnel(t), nil, "sid", nil, Ack{Members: 2})
	sess.slots[1] = server
	// 这条连接收到过帧（PONG），但最后一次是 1 秒前 —— 已经超过判死阈值
	sess.slotSeen[1].Store(time.Now().Add(-time.Second).UnixNano())

	done := make(chan struct{})
	go func() {
		sess.memberHeartbeat(context.Background(), 1, server)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("下行静默超过阈值的成员连接没有被摘除")
	}
	if conn := sess.slotConn(1); conn != nil {
		t.Error("下行静默的槽位应被摘掉，等维护协程补拨")
	}
}

// 还没收到过任何帧的连接不判死：老服务端不回 PONG，不能因为「没见过回包」就摘槽。
func TestMemberHeartbeatKeepsSlotBeforeFirstFrame(t *testing.T) {
	oldPing, oldDead := memberPingInterval, memberDeadAfter
	memberPingInterval = 20 * time.Millisecond
	memberDeadAfter = 10 * time.Millisecond
	t.Cleanup(func() { memberPingInterval, memberDeadAfter = oldPing, oldDead })

	server, peer := net.Pipe()
	defer peer.Close()
	defer server.Close()

	sess := newClientSession(testTunnel(t), nil, "sid", nil, Ack{Members: 2})
	sess.slots[1] = server
	// slotSeen 保持 0：这条连接还没收到过任何帧

	// net.Pipe 同步无缓冲，心跳写必须有人读，否则会卡在 pingMember 里
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

	select {
	case <-done:
		t.Fatal("还没收到过帧的连接不该被判死")
	case <-time.After(150 * time.Millisecond):
	}
	if conn := sess.slotConn(1); conn == nil {
		t.Fatal("未收到过帧的槽位不该被摘掉")
	}
	sess.closeAll()
}

// 运行状态日志里的 slot_silence：槽 0 必须反映控制连接的心跳余量（ctrlSeen），
// 而不是永远 -1；成员槽用各自的收帧时间。
func TestSlotSilenceReportUsesCtrlSeenForSlot0(t *testing.T) {
	tun := testTunnel(t)
	_, conn := net.Pipe()
	defer conn.Close()

	sess := newClientSession(tun, conn, "sid", nil, Ack{Members: 2})
	sess.ctrlSeen.Store(time.Now().Add(-5 * time.Second).UnixNano())
	sess.slotSeen[1].Store(time.Now().Add(-7 * time.Second).UnixNano())

	got := tun.SlotSilenceReport()
	if len(got) != 2 {
		t.Fatalf("每槽一项，长度应为 2，实际 %v", got)
	}
	if got[0] < 4 || got[0] > 8 {
		t.Errorf("槽 0 应报告约 5 秒，实际 %d", got[0])
	}
	if got[1] < 6 || got[1] > 10 {
		t.Errorf("槽 1 应报告约 7 秒，实际 %d", got[1])
	}
}

// 槽位复用：新连接入槽必须把判活时间重置为「现在」。
//
// 不重置的话，上一条连接留下的旧时间会被当成新连接的静默时长；而判活排在
// 发 PING 之前，第一个心跳周期就会直接把刚补上来的连接摘掉（连 PONG 的机会
// 都没有），形成「拨通→15 秒→掉」的循环。
func TestAttachSlotResetsSilence(t *testing.T) {
	server, peer := net.Pipe()
	defer peer.Close()
	defer server.Close()

	sess := newClientSession(testTunnel(t), nil, "sid", nil, Ack{Members: 2})
	sess.slotSeen[1].Store(time.Now().Add(-2 * time.Minute).UnixNano())

	if !sess.attachSlot(1, server) {
		t.Fatal("空槽应当可以入槽")
	}
	if idle := time.Since(time.Unix(0, sess.slotSeen[1].Load())); idle > time.Second {
		t.Fatalf("新连接入槽应重置判活时间，实际仍停在 %v 前", idle)
	}
	if got := sess.slotConn(1); got != server {
		t.Fatal("槽位应指向新连接")
	}
}

// 槽位已被占用时 attachSlot 必须拒绝，且不能改动槽位与判活时间。
func TestAttachSlotRejectsOccupied(t *testing.T) {
	first, firstPeer := net.Pipe()
	defer firstPeer.Close()
	defer first.Close()
	second, secondPeer := net.Pipe()
	defer secondPeer.Close()
	defer second.Close()

	sess := newClientSession(testTunnel(t), nil, "sid", nil, Ack{Members: 2})
	sess.slots[1] = first
	old := time.Now().Add(-time.Minute).UnixNano()
	sess.slotSeen[1].Store(old)

	if sess.attachSlot(1, second) {
		t.Fatal("已占用的槽位不该允许第二个连接入槽")
	}
	if sess.slotConn(1) != first {
		t.Fatal("槽位不该被替换")
	}
	if got := sess.slotSeen[1].Load(); got != old {
		t.Fatal("被拒绝的入槽不该改判活时间")
	}
}

// 入槽重置后，旧时间不能立刻摘槽：第一个心跳周期要把 PING 真的发出去。
// 对端不回 PONG 时，宽限期过后仍必须判死（判活功能不能被重置关掉）。
func TestMemberHeartbeatGraceAfterAttach(t *testing.T) {
	oldPing, oldDead := memberPingInterval, memberDeadAfter
	memberPingInterval = 20 * time.Millisecond
	memberDeadAfter = 60 * time.Millisecond
	t.Cleanup(func() { memberPingInterval, memberDeadAfter = oldPing, oldDead })

	server, peer := net.Pipe()
	defer peer.Close()
	defer server.Close()

	sess := newClientSession(testTunnel(t), nil, "sid", nil, Ack{Members: 2})
	// 模拟上一条连接 1 分钟前留下旧时间，然后新连接入槽
	sess.slotSeen[1].Store(time.Now().Add(-time.Minute).UnixNano())
	if !sess.attachSlot(1, server) {
		t.Fatal("空槽应当可以入槽")
	}

	gotPing := make(chan struct{}, 1)
	go func() {
		buf := make([]byte, 64)
		for {
			if _, err := peer.Read(buf); err != nil {
				return
			}
			select {
			case gotPing <- struct{}{}:
			default:
			}
		}
	}()

	done := make(chan struct{})
	go func() {
		sess.memberHeartbeat(context.Background(), 1, server)
		close(done)
	}()

	select {
	case <-gotPing:
	case <-time.After(time.Second):
		t.Fatal("入槽后第一个周期就该发出 PING，旧时间不该直接判死")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("宽限期过后仍没判死：判活被重置关掉了")
	}
}

// 旧连接迟到的帧不能刷新新连接的判活时间：读循环必须校验槽位归属。
//
// 真实时序：旧连接的 ReadFrame 已经返回一帧、读循环被调度器挂起，心跳把旧槽
// 摘掉、新连接补拨入槽，旧读循环恢复后才执行 touchSlot。不校验身份就会把旧帧
// 的时间写到新槽上，让新连接的下行判死白推迟一个周期。
func TestTouchSlotRejectsStaleConnection(t *testing.T) {
	oldConn, oldPeer := net.Pipe()
	defer oldPeer.Close()
	defer oldConn.Close()
	newConn, newPeer := net.Pipe()
	defer newPeer.Close()
	defer newConn.Close()

	sess := newClientSession(testTunnel(t), nil, "sid", nil, Ack{Members: 2})
	stale := time.Now().Add(-time.Minute).UnixNano()
	sess.slotSeen[1].Store(stale)
	sess.slots[1] = newConn

	if sess.touchSlot(1, oldConn) {
		t.Fatal("旧连接不该被当成槽位主人")
	}
	if got := sess.slotSeen[1].Load(); got != stale {
		t.Fatal("旧连接不能改写新连接的判活时间")
	}
	if !sess.touchSlot(1, newConn) {
		t.Fatal("新连接应能刷新自己的判活时间")
	}
	if got := sess.slotSeen[1].Load(); got == stale {
		t.Fatal("新连接自己的帧没有刷新时间")
	}
}
