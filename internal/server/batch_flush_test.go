package server

import (
	"net"
	"testing"
	"time"

	"github.com/antapp-cc/antapp-link/internal/proto"
)

// pipeSession 造一个只有控制连接（槽 0）的会话，连接用 net.Pipe 以便读回写出的字节。
func pipeSession(t *testing.T) (*session, net.Conn) {
	t.Helper()
	server, peer := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = peer.Close() })
	sess := &session{members: 1, closed: make(chan struct{})}
	sess.slots = []*slotConn{newSlotConn(server)}
	return sess, peer
}

// batchOf 按写合并的拼法造一批：[hdr|pkt][hdr|pkt]…（帧头已含在内）。
func batchOf(pkts ...[]byte) []byte {
	var batch []byte
	for _, p := range pkts {
		batch = proto.AppendHeader(batch, proto.TypeIP, len(p))
		batch = append(batch, p...)
	}
	return batch
}

// waitTx 等槽位 k 的写协程把 tx 计数加到 want。写出是异步的，断言前必须等它。
func waitTx(t *testing.T, sess *session, k int, want uint64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if sl := sess.slotEntry(k); sl != nil && sl.txBytes.Load() == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("等槽 %d 的 tx 计数到 %d 超时", k, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// 批里已经带了每帧的帧头，flushBatch 必须整批原样写出。再包一层帧头会让对端把
// 4 字节帧头当作 IP 包内容写进网卡（内核按 IP 版本号 0 丢弃），下行数据全丢。
func TestFlushBatchWritesFramesVerbatim(t *testing.T) {
	sess, peer := pipeSession(t)
	pkt := []byte{0x45, 0x00, 0x00, 0x14, 10, 0, 0, 2, 8, 8, 8, 8}

	// flushBatch 只入队、不阻塞；真正的写出在该槽的写协程里
	sess.flushBatch(0, batchOf(pkt))

	_ = peer.SetReadDeadline(time.Now().Add(2 * time.Second))
	typ, got, err := proto.ReadFrame(peer)
	if err != nil {
		t.Fatalf("读回帧: %v", err)
	}
	if typ != proto.TypeIP {
		t.Fatalf("帧类型错: %v", typ)
	}
	if string(got) != string(pkt) {
		t.Fatalf("IP 包被改动：期望 %d 字节 % x，实际 %d 字节 % x", len(pkt), pkt, len(got), got)
	}
}

// 批总量允许超过 MaxPayload（写合并的意义所在），对端要能逐帧读回。
func TestFlushBatchWritesMultiFrameBatch(t *testing.T) {
	sess, peer := pipeSession(t)
	var want [][]byte
	for i := 0; i < 3; i++ {
		pkt := make([]byte, 1400)
		pkt[0], pkt[1] = 0x45, byte(i)
		want = append(want, pkt)
	}
	batch := batchOf(want...)
	if len(batch) <= proto.MaxPayload {
		t.Fatalf("测试前提不成立：批只有 %d 字节", len(batch))
	}

	sess.flushBatch(0, batch)

	_ = peer.SetReadDeadline(time.Now().Add(2 * time.Second))
	for i, wantPkt := range want {
		typ, got, err := proto.ReadFrame(peer)
		if err != nil {
			t.Fatalf("第 %d 帧读回失败: %v", i+1, err)
		}
		if typ != proto.TypeIP || string(got) != string(wantPkt) {
			t.Fatalf("第 %d 帧不对：type=%v 载荷 %d 字节", i+1, typ, len(got))
		}
	}
}

// 每槽收发字节要能被 status 看到：写出的算 tx，读入的算 rx，都按链路上的字节（含帧头）。
func TestSlotStatsCountTraffic(t *testing.T) {
	sess, peer := pipeSession(t)
	pkt := []byte{0x45, 0x00, 0x00, 0x14, 10, 0, 0, 2, 8, 8, 8, 8}
	batch := batchOf(pkt)

	sess.flushBatch(0, batch)
	_ = peer.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := proto.ReadFrame(peer); err != nil {
		t.Fatalf("读回帧: %v", err)
	}
	// net.Pipe 的 Write 要等对端读完才返回，所以读回帧不代表写协程已经记完 tx 计数，
	// 这里等一下它，避免偶发失败。
	waitTx(t, sess, 0, uint64(len(batch)))
	if sl := sess.slotEntry(0); sl != nil {
		sl.noteRx(len(pkt))
	}

	stats := sess.slotStats()
	if len(stats) != 1 {
		t.Fatalf("槽统计应有 1 项，实际 %d 项", len(stats))
	}
	if got := stats[0].TxBytes; got != uint64(len(batch)) {
		t.Errorf("槽 0 发送字节 = %d，期望 %d", got, len(batch))
	}
	if got, want := stats[0].RxBytes, uint64(len(pkt)+proto.HeaderSize); got != want {
		t.Errorf("槽 0 接收字节 = %d，期望 %d", got, want)
	}
}

// flushBatch 绝不能阻塞调用方：pumpTun 是单个 goroutine 串行服务所有槽，在这里等
// 一条拥塞的连接，其余槽（含控制连接的心跳）会一起被拖停。这里用「对端永不读取」的
// net.Pipe 模拟拥塞槽 —— 原来的同步写实现会卡在 Write 上直到写超时。
func TestFlushBatchNeverBlocksOnSlowSlot(t *testing.T) {
	sess, _ := pipeSession(t) // peer 故意不读，Write 会一直阻塞
	pkt := []byte{0x45, 0x00, 0x00, 0x14, 10, 0, 0, 2, 8, 8, 8, 8}

	done := make(chan struct{})
	go func() {
		for i := 0; i < slotQueueLen*4; i++ {
			sess.flushBatch(0, batchOf(pkt))
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("flushBatch 被慢槽阻塞：pumpTun 会被一条拥塞连接拖停全部")
	}
	sl := sess.slotEntry(0)
	if sl == nil {
		t.Fatal("槽 0 不该为空")
	}
	if sl.drops.Load() == 0 {
		t.Error("队列早已写满，却没有丢弃计数（要么队列过大，要么在阻塞等待）")
	}
}

// 空槽不该被记进流量（成员连接断开后流量落回控制连接，账要记在实际承载的那条上）。
func TestSlotEntryReturnsNilForEmptySlot(t *testing.T) {
	sess, _ := pipeSession(t)
	sess.mu.Lock()
	sess.slots = append(sess.slots, nil)
	sess.mu.Unlock()

	if sess.slotEntry(1) != nil {
		t.Fatal("空槽应返回 nil，不能落回控制连接")
	}
	if sess.slotEntry(9) != nil {
		t.Fatal("越界槽应返回 nil")
	}
}
