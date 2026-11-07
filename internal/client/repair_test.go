package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/antapp-cc/antapp-link/internal/pki"
	"github.com/antapp-cc/antapp-link/internal/proto"
)

func TestSlotQueueCapacityIsSixteen(t *testing.T) {
	if slotTxLen != 16 {
		t.Fatalf("client slot queue depth = %d, want 16", slotTxLen)
	}
}

func TestClientPendingBytesTrackWriteLifecycle(t *testing.T) {
	conn := newBlockedConn()
	conn.block()
	sess := newClientSession(testTunnel(t), conn, "sid", nil, Ack{Members: 2})
	sess.slots[1] = conn
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sess.slotWriteLoop(ctx, 1, make(chan error, 1))

	sess.enqueue(1, []byte{1, 2, 3})
	conn.waitEntered(t)
	if got := sess.slotPendingBytes[1].Load(); got != 3 {
		t.Fatalf("in-flight batch must remain in pending pressure: %d", got)
	}
	sess.enqueue(1, []byte{4, 5})
	if got := sess.slotPendingBytes[1].Load(); got != 5 {
		t.Fatalf("pending bytes behind blocked write = %d, want 5", got)
	}
	conn.release()
	deadline := time.Now().Add(2 * time.Second)
	for sess.slotPendingBytes[1].Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := sess.slotPendingBytes[1].Load(); got != 0 {
		t.Fatalf("pending bytes after writer drains = %d, want 0", got)
	}
}

func repairTCPPacket() []byte {
	p := make([]byte, 24)
	p[0], p[9] = 0x45, 6
	copy(p[12:16], []byte{10, 0, 0, 2})
	copy(p[16:20], []byte{8, 8, 8, 8})
	binary.BigEndian.PutUint16(p[20:22], 45000)
	binary.BigEndian.PutUint16(p[22:24], 443)
	return p
}

func TestCanceledSlotWriterDropsQueuedBatchesAndClearsPending(t *testing.T) {
	conn := newRecordConn()
	sess := newClientSession(testTunnel(t), newRecordConn(), "cancel-writer", nil, Ack{Members: 2})
	defer sess.closeAll()
	sess.slots[1] = conn
	sess.enqueue(1, []byte{1, 2, 3})
	sess.enqueue(1, []byte{4, 5})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		sess.slotWriteLoop(ctx, 1, make(chan error, 1))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("已取消的槽写协程未退出")
	}
	if len(conn.ch) != 0 {
		t.Fatalf("取消后仍写出 %d 个批次", len(conn.ch))
	}
	if got := len(sess.slotTx[1]); got != 0 {
		t.Fatalf("取消后队列残留 %d 个批次", got)
	}
	if got := sess.slotPendingBytes[1].Load(); got != 0 {
		t.Fatalf("取消后 pending 字节未回零: %d", got)
	}
	if got := sess.tunnel.Drops.StaleBatch.Load(); got != 2 {
		t.Fatalf("取消丢弃批次数=%d want=2", got)
	}
	if got := sess.tunnel.Drops.UplinkBytes.Load(); got != 5 {
		t.Fatalf("取消丢弃字节数=%d want=5", got)
	}
}

func TestCanceledSlotWriterDropsBatchAfterWriteLockWait(t *testing.T) {
	conn := newRecordConn()
	sess := newClientSession(testTunnel(t), newRecordConn(), "cancel-write-lock", nil, Ack{Members: 2})
	defer sess.closeAll()
	sess.slots[1] = conn
	sess.enqueue(1, []byte{1, 2, 3})
	sess.writes[1].Lock()
	locked := true
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		sess.slotWriteLoop(ctx, 1, make(chan error, 1))
		close(done)
	}()
	defer func() {
		if locked {
			sess.writes[1].Unlock()
		}
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("槽写协程未退出")
		}
	}()

	deadline := time.Now().Add(time.Second)
	for len(sess.slotTx[1]) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(sess.slotTx[1]) != 0 {
		t.Fatal("writer 未在写锁上等待")
	}
	cancel()
	sess.writes[1].Unlock()
	locked = false
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("已取消的槽写协程未退出")
	}
	if len(conn.ch) != 0 {
		t.Fatalf("等待写锁期间取消后仍写出 %d 个批次", len(conn.ch))
	}
	if got := sess.slotPendingBytes[1].Load(); got != 0 {
		t.Fatalf("写锁等待取消后 pending 字节未回零: %d", got)
	}
	if got := sess.tunnel.Drops.StaleBatch.Load(); got != 1 {
		t.Fatalf("写锁等待取消丢弃批次数=%d want=1", got)
	}
	if got := sess.tunnel.Drops.UplinkBytes.Load(); got != 3 {
		t.Fatalf("写锁等待取消丢弃字节=%d want=3", got)
	}
}

func TestCloseAllDiscardsQueuedBatchesAndPendingBytes(t *testing.T) {
	sess := newClientSession(testTunnel(t), newRecordConn(), "close-queue", nil, Ack{Members: 2})
	sess.slots[1] = newRecordConn()
	sess.enqueue(1, []byte{1, 2, 3})
	sess.enqueue(1, []byte{4, 5})

	sess.closeAll()
	if got := len(sess.slotTx[1]); got != 0 {
		t.Fatalf("会话关闭后队列残留 %d 个批次", got)
	}
	if got := sess.slotPendingBytes[1].Load(); got != 0 {
		t.Fatalf("会话关闭后 pending 字节未回零: %d", got)
	}
	if got := sess.tunnel.Drops.StaleBatch.Load(); got != 2 {
		t.Fatalf("会话关闭丢弃批次数=%d want=2", got)
	}
	if got := sess.tunnel.Drops.UplinkBytes.Load(); got != 5 {
		t.Fatalf("会话关闭丢弃字节=%d want=5", got)
	}
}

func TestCloseAllRacingEnqueueLeavesNoPendingBatch(t *testing.T) {
	for i := 0; i < 50; i++ {
		sess := newClientSession(testTunnel(t), newRecordConn(), "close-race", nil, Ack{Members: 2})
		conn := newRecordConn()
		sess.slots[1] = conn
		sess.queueMu.Lock()
		enqueueDone, closeDone := make(chan struct{}), make(chan struct{})
		go func() {
			sess.enqueueOn(1, conn, []byte{1, 2, 3})
			close(enqueueDone)
		}()
		go func() {
			sess.closeAll()
			close(closeDone)
		}()
		sess.queueMu.Unlock()

		for _, done := range []<-chan struct{}{enqueueDone, closeDone} {
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("入队与会话关闭发生死锁")
			}
		}
		if got := len(sess.slotTx[1]); got != 0 {
			t.Fatalf("第 %d 轮关闭后队列残留 %d 个批次", i, got)
		}
		if got := sess.slotPendingBytes[1].Load(); got != 0 {
			t.Fatalf("第 %d 轮关闭后 pending 字节=%d", i, got)
		}
		if got := sess.tunnel.Drops.StaleBatch.Load(); got != 1 {
			t.Fatalf("第 %d 轮丢弃批次数=%d want=1", i, got)
		}
	}
}

func TestUDPPumpDoesNotSendWhenCanceledDuringTrace(t *testing.T) {
	receiver, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	sender, err := net.DialUDP("udp4", nil, receiver.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	seal, err := proto.NewCipher(make([]byte, 32), 1)
	if err != nil {
		t.Fatal(err)
	}

	tun := testTunnel(t)
	tun.inv.Mode = pki.ModeUDP
	sess := newClientSession(tun, newRecordConn(), "udp-cancel", nil, Ack{Members: 1})
	defer sess.closeAll()
	sess.udp = &udpChannel{conn: sender, sendSeal: seal, stats: &tun.Stats, closed: make(chan struct{})}

	pkt := make([]byte, 40)
	pkt[0], pkt[9] = 0x45, 6
	copy(pkt[12:16], []byte{10, 0, 0, 2})
	copy(pkt[16:20], []byte{8, 8, 8, 8})
	binary.BigEndian.PutUint16(pkt[20:22], 45000)
	binary.BigEndian.PutUint16(pkt[22:24], 443)
	pkt[32], pkt[33] = 0x50, 0x10

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sess.trace = &OrderTracer{base: time.Now(), w: bufio.NewWriterSize(lifecycleFeedWriter{write: cancel}, 1)}
	tun.pktCh <- pkt
	if err := sess.pumpFromDevice(ctx); err != nil {
		t.Fatalf("取消后的 UDP pump 返回错误: %v", err)
	}
	if got := tun.Stats.TxWritten.Load(); got != 0 {
		t.Fatalf("取消后 UDP 实际写出字节=%d want=0", got)
	}
	if got := tun.Drops.StaleBatch.Load(); got != 1 {
		t.Fatalf("取消丢弃计数=%d want=1", got)
	}
	if got := tun.Drops.UplinkBytes.Load(); got != int64(len(pkt)) {
		t.Fatalf("取消丢弃字节=%d want=%d", got, len(pkt))
	}
	if err := receiver.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := receiver.ReadFromUDP(make([]byte, 2048)); err == nil {
		t.Fatal("取消后仍向 UDP socket 发送了数据报")
	} else if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
		t.Fatalf("等待 UDP 数据报时出现非超时错误: %v", err)
	}
}

func TestClientNewFlowAvoidsFullSmallBatchQueue(t *testing.T) {
	sess := newClientSession(testTunnel(t), newRecordConn(), "repair", nil, Ack{Members: 3})
	sess.slots[1], sess.slots[2] = newRecordConn(), newRecordConn()
	defer sess.closeAll()
	for i := 0; i < cap(sess.slotTx[1]); i++ {
		sess.enqueue(1, make([]byte, 64))
	}
	sess.enqueue(2, make([]byte, 4096))
	p := repairTCPPacket()
	k := sess.flowSlot(p)
	if k != 2 {
		t.Fatalf("满队列仍被选中: slot=%d pending=[%d,%d]", k, sess.slotPendingBytes[1].Load(), sess.slotPendingBytes[2].Load())
	}
	sess.enqueue(k, p)
	if sess.tunnel.Drops.SlotQueueFull.Load() != 0 {
		t.Fatal("有空位却丢弃新流")
	}
	// 已有流不能因原槽满队列而迁移。
	for len(sess.slotTx[2]) < cap(sess.slotTx[2]) {
		sess.enqueue(2, make([]byte, 64))
	}
	if got := sess.flowSlot(p); got != k {
		t.Fatalf("队列满导致已有流迁移: %d -> %d", k, got)
	}
}

type repairDevice struct{ writes int }

func (*repairDevice) Read([]byte) (int, error)      { return 0, net.ErrClosed }
func (*repairDevice) TryRead([]byte) (int, error)   { return 0, nil }
func (d *repairDevice) Write(p []byte) (int, error) { d.writes++; return len(p), nil }
func (*repairDevice) Name() string                  { return "repair" }
func (*repairDevice) Close() error                  { return nil }
func TestControlDataFramesRespectClientMode(t *testing.T) {
	for _, mode := range []pki.TunnelMode{pki.ModeTCP, pki.ModeUDP} {
		t.Run(string(mode), func(t *testing.T) {
			ctrl, peer := net.Pipe()
			defer peer.Close()
			tun := testTunnel(t)
			tun.inv.Mode = mode
			dev := &repairDevice{}
			tun.dev = dev
			sess := newClientSession(tun, ctrl, "repair", nil, Ack{Members: 1})
			defer sess.closeAll()
			done := make(chan struct{})
			go func() {
				defer close(done)
				defer peer.Close()
				_ = proto.WriteFrame(peer, proto.TypeIP, repairTCPPacket())
				_ = proto.WriteFrame(peer, proto.TypeBye, []byte("test done"))
			}()
			err := sess.pumpFromTunnel(context.Background())
			_ = peer.Close()
			<-done
			if mode == pki.ModeUDP {
				if dev.writes != 0 || err == nil || !strings.Contains(err.Error(), "UDP") {
					t.Fatalf("UDP 仍接受 TCP 业务帧: writes=%d err=%v", dev.writes, err)
				}
			} else if dev.writes != 1 {
				t.Fatal("TCP 业务收包兼容性被破坏")
			}
		})
	}
}

func TestUDPWithoutChannelCannotEnqueueTCPBusiness(t *testing.T) {
	tun := testTunnel(t)
	tun.inv.Mode = pki.ModeUDP
	sess := newClientSession(tun, newRecordConn(), "repair", nil, Ack{Members: 1})
	defer sess.closeAll()
	tun.pktCh <- repairTCPPacket()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := sess.pumpFromDevice(ctx)
	if err == nil || !strings.Contains(err.Error(), "UDP") || len(sess.slotTx[0]) != 0 {
		t.Fatalf("UDP 未就绪仍进入 TCP 队列: err=%v depth=%d", err, len(sess.slotTx[0]))
	}
}

func TestQueuedFlowStaysOffRetiredMember(t *testing.T) {
	old, replacement, other := newRecordConn(), newRecordConn(), newRecordConn()
	sess := newClientSession(testTunnel(t), newRecordConn(), "strict", nil, Ack{Members: 3})
	defer sess.closeAll()
	sess.slots[1], sess.slots[2] = old, other
	packet := append(repairTCPPacket(), make([]byte, 16)...)
	packet[32], packet[33] = 0x50, 0x10
	binary.BigEndian.PutUint16(packet[2:4], 40)
	binary.BigEndian.PutUint32(packet[24:28], 1000)
	first := sess.flowSlot(packet)
	if first != 1 {
		t.Fatalf("fixture first slot %d", first)
	}
	frame := func(p []byte) []byte { b := proto.AppendHeader(nil, proto.TypeIP, len(p)); return append(b, p...) }
	sess.enqueue(first, frame(packet))
	sess.dropSlot(first, old)
	newer := append([]byte(nil), packet...)
	binary.BigEndian.PutUint32(newer[24:28], 2000)
	migrated := sess.flowSlot(newer)
	if migrated != 2 {
		t.Fatalf("flow not migrated to live slot2: %d", migrated)
	}
	if !sess.attachSlot(first, replacement) {
		t.Fatal("cannot attach replacement")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done2 := make(chan struct{})
	go func() { defer close(done2); sess.slotWriteLoop(ctx, migrated, make(chan error, 1)) }()
	defer func() { cancel(); <-done2 }()
	sess.enqueue(migrated, frame(newer))
	select {
	case got := <-other.ch:
		t.Logf("migrated slot2 sent seq=%d", binary.BigEndian.Uint32(got[proto.HeaderSize+24:]))
	case <-time.After(time.Second):
		t.Fatal("new member did not send newer batch")
	}
	// Resume the old slot's writer only after the migrated flow has sent newer data.
	done1 := make(chan struct{})
	go func() { defer close(done1); sess.slotWriteLoop(ctx, first, make(chan error, 1)) }()
	defer func() { cancel(); <-done1 }()
	select {
	case got := <-replacement.ch:
		t.Fatalf("flow pinned to slot2 but replacement slot1 later sent stale seq=%d", binary.BigEndian.Uint32(got[proto.HeaderSize+24:]))
	case <-time.After(100 * time.Millisecond):
	}
}

type lifecycleFeedWriter struct{ write func() }

func (w lifecycleFeedWriter) Write(p []byte) (int, error) { w.write(); return len(p), nil }
func TestPumpStopsHarvestingMixedFlowsAfterCancel(t *testing.T) {
	tun := testTunnel(t)
	sess := newClientSession(tun, newRecordConn(), "strict-batch", nil, Ack{Members: 3})
	defer sess.closeAll()
	sess.slots[1], sess.slots[2] = newRecordConn(), newRecordConn()
	makePacket := func(port uint16) []byte {
		p := append(repairTCPPacket(), make([]byte, 16)...)
		binary.BigEndian.PutUint16(p[20:22], port)
		p[32], p[33] = 0x50, 0x10
		binary.BigEndian.PutUint16(p[2:4], 40)
		return p
	}
	packets := [][]byte{makePacket(46010), makePacket(46011)}
	for _, p := range packets {
		sess.flowSlot(p)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	count := 0
	const cancelAt = 4
	const feedUntil = 2000
	writer := lifecycleFeedWriter{write: func() {
		count++
		if count == cancelAt {
			cancel()
			sess.closeAll()
		}
		if count < feedUntil {
			tun.pktCh <- packets[count%2]
		}
	}}
	sess.trace = &OrderTracer{w: bufio.NewWriterSize(writer, 1), base: time.Now()}
	tun.pktCh <- packets[0]
	err := sess.pumpFromDevice(ctx)
	t.Logf("pump canceled at packet %d, returned after processing %d packets; err=%v", cancelAt, count, err)
	if count > cancelAt+1 {
		t.Fatalf("canceled pump kept consuming %d mixed-flow packets until producer stopped", count-cancelAt)
	}
}

func waitClientPressureZero(t *testing.T, s *clientSession, k int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for s.slotPendingBytes[k].Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := s.slotPendingBytes[k].Load(); got != 0 {
		t.Fatalf("slot %d pending未回零: %d", k, got)
	}
}
func TestRetiredBatchIsDroppedAfterDequeueAndWriteLockWait(t *testing.T) {
	old, replacement := newRecordConn(), newRecordConn()
	s := newClientSession(testTunnel(t), newRecordConn(), "retired", nil, Ack{Members: 2})
	defer s.closeAll()
	s.slots[1] = old
	s.enqueue(1, []byte{1, 2, 3})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.writes[1].Lock()
	locked := true
	go func() { defer close(done); s.slotWriteLoop(ctx, 1, make(chan error, 1)) }()
	defer func() {
		if locked {
			s.writes[1].Unlock()
		}
		cancel()
		<-done
	}()
	deadline := time.Now().Add(time.Second)
	for len(s.slotTx[1]) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(s.slotTx[1]) != 0 {
		t.Fatal("writer未取出旧批次")
	}
	s.dropSlot(1, old)
	if !s.attachSlot(1, replacement) {
		t.Fatal("替补连接入槽失败")
	}
	s.enqueue(1, []byte{4, 5})
	s.writes[1].Unlock()
	locked = false
	select {
	case got := <-replacement.ch:
		if !bytes.Equal(got, []byte{4, 5}) {
			t.Fatalf("替补连接收到旧批次: %v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("新批次未发送")
	}
	waitClientPressureZero(t, s, 1)
	if len(old.ch) != 0 {
		t.Fatal("旧连接摘除后仍发生写入")
	}
	if got := s.tunnel.Drops.StaleBatch.Load(); got != 1 {
		t.Fatalf("旧批次丢弃次数=%d want=1", got)
	}
	if got := s.tunnel.Drops.UplinkBytes.Load(); got != 3 {
		t.Fatalf("旧批次丢弃字节=%d want=3", got)
	}
	if got := s.stats.TxWritten.Load(); got != 2 {
		t.Fatalf("实际发送字节=%d want=2", got)
	}
}
func TestUnqueuedBatchKeepsSelectedConnectionIdentity(t *testing.T) {
	for _, migrate := range []bool{false, true} {
		name := "same_slot_new_connection"
		if migrate {
			name = "flow_migrates_to_other_slot"
		}
		t.Run(name, func(t *testing.T) {
			tun := testTunnel(t)
			s := newClientSession(tun, newRecordConn(), "local-buffer", nil, Ack{Members: 3})
			defer s.closeAll()
			old, replacement, other := newRecordConn(), newRecordConn(), newRecordConn()
			s.slots[1], s.slots[2] = old, other
			p := append(repairTCPPacket(), make([]byte, 16)...)
			p[32], p[33] = 0x50, 0x10
			binary.BigEndian.PutUint32(p[24:28], 1000)
			next := append([]byte(nil), p...)
			binary.BigEndian.PutUint32(next[24:28], 2000)
			s.flowSlot(p)
			ctx, cancel := context.WithCancel(context.Background())
			writesDone := make(chan struct{})
			pumpDone := make(chan struct{})
			var pumpErr error
			var wg sync.WaitGroup
			wg.Add(2)
			for k := 1; k <= 2; k++ {
				go func(k int) { defer wg.Done(); s.slotWriteLoop(ctx, k, make(chan error, 1)) }(k)
			}
			go func() { wg.Wait(); close(writesDone) }()
			defer func() { cancel(); <-pumpDone; <-writesDone }()
			calls, moved, attached := 0, 0, false
			s.trace = &OrderTracer{base: time.Now(), w: bufio.NewWriterSize(lifecycleFeedWriter{write: func() {
				calls++
				if calls == 1 {
					s.dropSlot(1, old)
					if migrate {
						moved = s.flowSlot(next)
					}
					attached = s.attachSlot(1, replacement)
					tun.pktCh <- next
				}
			}}, 1)}
			tun.pktCh <- p
			go func() { pumpErr = s.pumpFromDevice(ctx); close(pumpDone) }()
			receiver := replacement
			if migrate {
				receiver = other
			}
			select {
			case got := <-receiver.ch:
				if len(got) != proto.HeaderSize+len(next) || binary.BigEndian.Uint32(got[proto.HeaderSize+24:]) != 2000 {
					t.Fatalf("未入队缓冲混入旧报文: %v", got)
				}
			case <-time.After(time.Second):
				t.Fatal("新连接批次未发送")
			}
			waitClientPressureZero(t, s, 1)
			waitClientPressureZero(t, s, 2)
			cancel()
			<-pumpDone
			if pumpErr != nil || !attached || (migrate && moved != 2) {
				t.Fatalf("替换流程异常: %v attached=%v moved=%d", pumpErr, attached, moved)
			}
			if len(old.ch) != 0 || len(replacement.ch) != 0 {
				t.Fatal("旧报文通过旧连接或替补槽发送")
			}
			if got := tun.Drops.StaleBatch.Load(); got != 1 {
				t.Fatalf("旧缓冲丢弃次数=%d want=1", got)
			}
		})
	}
}

func TestRetiredBatchDoesNotWaitForReplacementWriteLock(t *testing.T) {
	s := newClientSession(testTunnel(t), newRecordConn(), "stale-fast-path", nil, Ack{Members: 2})
	defer s.closeAll()
	old, replacement := newRecordConn(), newRecordConn()
	s.slots[1] = old
	s.enqueue(1, []byte{1, 2, 3})
	s.dropSlot(1, old)
	s.attachSlot(1, replacement)
	s.writes[1].Lock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.slotWriteLoop(ctx, 1, make(chan error, 1)) }()
	defer func() { s.writes[1].Unlock(); cancel(); <-done }()
	waitClientPressureZero(t, s, 1)
	if len(replacement.ch) != 0 || s.tunnel.Drops.StaleBatch.Load() != 1 {
		t.Fatal("过期批次未直接丢弃")
	}
}
