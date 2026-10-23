package client

import (
	"context"
	"net"
	"testing"
	"time"
)

// recordConn 只记录收到的字节，用于验证「数据确实发出去了」。
type recordConn struct {
	ch chan []byte
}

func newRecordConn() *recordConn { return &recordConn{ch: make(chan []byte, 8)} }

func (c *recordConn) Write(b []byte) (int, error) {
	cp := make([]byte, len(b))
	copy(cp, b)
	select {
	case c.ch <- cp:
	default:
	}
	return len(b), nil
}

func (c *recordConn) Read([]byte) (int, error)         { return 0, net.ErrClosed }
func (c *recordConn) Close() error                     { return nil }
func (c *recordConn) LocalAddr() net.Addr              { return nil }
func (c *recordConn) RemoteAddr() net.Addr             { return nil }
func (c *recordConn) SetDeadline(time.Time) error      { return nil }
func (c *recordConn) SetReadDeadline(time.Time) error  { return nil }
func (c *recordConn) SetWriteDeadline(time.Time) error { return nil }

// 一条成员连接写阻塞时，另一条健康成员必须还能发出去。
//
// 改造前 pumpFromDevice 是单协程顺序 flushBatch：槽 1 的 Write 一旦卡在重传上
// （单次写超时 20 秒），槽 2 的上行也发不出去，64 个包的 pktCh 随即填满开始丢包。
// 这是「刚连上好、用一段时间就变慢」最可能的机械原因。
func TestSlowMemberDoesNotBlockOtherSlots(t *testing.T) {
	slow := newBlockedConn()
	slow.block()
	fast := newRecordConn()

	sess := newClientSession(testTunnel(t), slow, "sid", nil, Ack{Members: 4})
	sess.slots[1] = slow
	sess.slots[2] = fast

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 4)
	for k := 0; k < sess.members; k++ {
		go sess.slotWriteLoop(ctx, k, errCh)
	}

	// 槽 1 投一批，它会卡在 slow.Write 里不返回
	sess.enqueue(1, []byte{1, 2, 3})
	slow.waitEntered(t)

	// 槽 2 投一批，必须很快送达
	sess.enqueue(2, []byte{4, 5, 6})
	select {
	case got := <-fast.ch:
		if len(got) == 0 {
			t.Fatal("槽 2 收到的批次是空的")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("槽 1 写阻塞时，槽 2 的上行也被堵住了")
	}
	slow.release()
}

// 投递必须拷贝：pumpFromDevice 复用同一个 batch 缓冲，投完立刻 batch[:0] 再用。
// 不拷贝的话，队列里的批会被下一批数据覆盖，发出去的是错帧。
func TestEnqueueCopiesBatch(t *testing.T) {
	sess := newClientSession(testTunnel(t), newRecordConn(), "sid", nil, Ack{Members: 2})

	buf := []byte{1, 2, 3, 4}
	sess.enqueue(1, buf)
	// 模拟调用方复用缓冲
	copy(buf, []byte{9, 9, 9, 9})

	select {
	case queued := <-sess.slotTx[1]:
		if len(queued) != 4 || queued[0] != 1 || queued[3] != 4 {
			t.Fatalf("队列里的批被调用方覆盖了：%v", queued)
		}
	case <-time.After(time.Second):
		t.Fatal("没有排进队列")
	}
}

// 队列满时必须丢并计数，不能阻塞调用方（网卡 reader 停下来的话，整个上行都会停）。
func TestEnqueueDropsWhenFullWithoutBlocking(t *testing.T) {
	sess := newClientSession(testTunnel(t), newRecordConn(), "sid", nil, Ack{Members: 2})

	done := make(chan struct{})
	go func() {
		// 投 slotTxLen+5 批，趁写协程没启动、队列必然填满
		for i := 0; i < slotTxLen+5; i++ {
			sess.enqueue(1, []byte{byte(i)})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("队列满时 enqueue 阻塞了调用方")
	}
	dropped := sess.tunnel.Drops.SlotQueueFull.Load()
	if dropped == 0 {
		t.Fatal("队列满了却没有记录丢弃次数")
	}
}

// TxWritten 只在真正写进外层连接之后累加。
//
// 以前界面上的「发送」读的是 TxBytes，它在入队之前就加了 —— 队列满丢掉的那批
// 也算「发送成功」。Pi 节点上行常年打满，这个差值能有几百 MB，看起来在发、实际
// 对方收不到。现在两者分开记，差值就是「读到了但没发出去」。
func TestTxWrittenOnlyCountsActuallyWritten(t *testing.T) {
	sess := newClientSession(testTunnel(t), newBlockedConn(), "sid", nil, Ack{Members: 2})

	// 队列只有 slotTxLen 深，塞满之后剩下的全会被丢
	before := sess.stats.TxWritten.Load()
	for i := 0; i < slotTxLen*3; i++ {
		sess.enqueue(1, []byte{byte(i)})
	}
	// 没有写协程消费，所以一个字节都不该算「写出」
	if got := sess.stats.TxWritten.Load(); got != before {
		t.Fatalf("没写出任何数据，TxWritten 却涨了 %d -> %d", before, got)
	}
	// 但排队满导致的丢弃必须被记下来
	if sess.tunnel.Drops.SlotQueueFull.Load() == 0 {
		t.Fatal("队列满却没有记录丢弃")
	}
}

// 队列满时不该做那次必然被丢掉的复制。
//
// 拥塞时这条路径每批都要 make 一个 20KB 切片再扔掉：Pi 节点实测 10 分钟丢 8000 批，
// 等于每秒白分配 266KB，白交 GC。这个用例钉住「满队列路径零分配」。
func TestEnqueueFullQueueDoesNotAllocate(t *testing.T) {
	sess := newClientSession(testTunnel(t), newBlockedConn(), "sid", nil, Ack{Members: 2})

	// 先把队列灌满（这一步有分配，不算在内）
	for len(sess.slotTx[1]) < cap(sess.slotTx[1]) {
		sess.enqueue(1, make([]byte, 64))
	}
	payload := make([]byte, 64)

	allocs := testing.AllocsPerRun(100, func() {
		sess.enqueue(1, payload) // 队列始终是满的，应当直接返回
	})
	if allocs > 0 {
		t.Fatalf("队列满时仍在分配：%.1f allocs/op（应当为 0）", allocs)
	}
}
