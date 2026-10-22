package server

import (
	"net"
	"testing"
	"time"
)

// 读写有字节流过时必须刷新时间戳 —— 否则清扫协程会把正在传数据的连接当空闲杀掉。
func TestRelayConnTouchesOnReadAndWrite(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	rc := newRelayConn(a)
	// 人为把时间戳推旧，模拟「刚建好但还没数据」
	rc.lastActive.Store(time.Now().Add(-time.Hour).UnixNano())
	if got := rc.idleFor(time.Now()); got < 30*time.Minute {
		t.Fatalf("初始时间戳应该被 push 成旧的，实际 idle=%v", got)
	}

	// 读方向：另起协程往对端写
	go func() { _, _ = b.Write([]byte("x")) }()
	buf := make([]byte, 1)
	if _, err := rc.Read(buf); err != nil {
		t.Fatalf("Read 失败: %v", err)
	}
	if got := rc.idleFor(time.Now()); got > time.Second {
		t.Fatalf("Read 之后时间戳应被刷新，实际 idle=%v", got)
	}

	// 写方向
	rc.lastActive.Store(time.Now().Add(-time.Hour).UnixNano())
	go func() { _, _ = b.Read(make([]byte, 1)) }()
	if _, err := rc.Write([]byte("y")); err != nil {
		t.Fatalf("Write 失败: %v", err)
	}
	if got := rc.idleFor(time.Now()); got > time.Second {
		t.Fatalf("Write 之后时间戳应被刷新，实际 idle=%v", got)
	}
}

// 零字节的读写不该刷新时间戳：Read 返回 (0, err) 是关闭信号，不是"有流量"。
func TestRelayConnIgnoresZeroByteTransfer(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()

	rc := newRelayConn(a)
	rc.lastActive.Store(time.Now().Add(-time.Hour).UnixNano())

	// 空 slice 写入不会真的传数据
	go func() { _, _ = b.Write(nil) }()
	_, _ = rc.Read(make([]byte, 0))

	idle := rc.idleFor(time.Now())
	if idle < 30*time.Minute {
		t.Fatalf("空传输不该刷新时间戳，实际 idle=%v", idle)
	}
}

// idleFor 是清扫判定唯一的依据，边界不能错。
func TestRelayConnIdleFor(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	rc := newRelayConn(a)
	rc.lastActive.Store(time.Now().Add(-15 * time.Minute).UnixNano())

	got := rc.idleFor(time.Now())
	if got < 14*time.Minute || got > 16*time.Minute {
		t.Fatalf("期望约 15 分钟，实际 %v", got)
	}
	// 该被清扫
	if got <= relayIdleTimeout {
		t.Fatalf("15 分钟静默应超过默认阈值 %v，实际 idle=%v", relayIdleTimeout, got)
	}
}

// 并发读写时时间戳更新不能丢更新（同一个 atomic，最后写入者胜出）。
func TestRelayConnConcurrentTouch(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()

	rc := newRelayConn(a)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			_ = b.SetDeadline(time.Now().Add(time.Second))
			if _, err := b.Write([]byte("abcd")); err != nil {
				return
			}
		}
	}()

	buf := make([]byte, 4)
	for i := 0; i < 200; i++ {
		if _, err := rc.Read(buf); err != nil {
			break
		}
	}
	<-done

	if got := rc.idleFor(time.Now()); got > 5*time.Second {
		t.Fatalf("并发读之后时间戳应接近现在，实际 idle=%v", got)
	}
}

// 计数器的增减必须配平，否则 status 里会显示一个永远不归零的「在途连接数」。
func TestRelayStatsBalance(t *testing.T) {
	var s relayStats

	for i := 0; i < 5; i++ {
		s.onAccept()
		s.onActive()
	}
	if active, total, _, _ := s.snapshot(); active != 5 || total != 5 {
		t.Fatalf("期望 active=5 total=5，实际 active=%d total=%d", active, total)
	}

	for i := 0; i < 3; i++ {
		s.onClosed()
	}
	if active, total, _, _ := s.snapshot(); active != 2 || total != 5 {
		t.Fatalf("关闭 3 条后期望 active=2 total=5，实际 active=%d total=%d", active, total)
	}

	// 被拒的连接同样计入 total（它确实被 accept 过），只是不进 active。
	// 调用方顺序是：accept → onAccept → 检查是否已满 → 满则 onRejected。
	s.onAccept()
	s.onRejected()
	s.onAccept()
	s.onRejected()
	s.setIdle(1)
	active, total, rejected, idle := s.snapshot()
	if active != 2 || total != 7 || rejected != 2 || idle != 1 {
		t.Fatalf("期望 2/7/2/1，实际 %d/%d/%d/%d", active, total, rejected, idle)
	}
}
