package proto

import (
	"encoding/binary"
	"testing"
	"time"
)

// buildTCPWithFlags 造一个带 TCP 标志位的 40 字节包：前 24 字节与 buildTCP 一致，
// 第 33 字节是 TCP flags（FIN/RST 回收用）。
func buildTCPWithFlags(src, dst [4]byte, sport, dport uint16, flags byte) []byte {
	p := buildTCP(src, dst, sport, dport)
	p = append(p, make([]byte, 16)...)
	p[33] = flags
	return p
}

func buildUDPFlow(src, dst [4]byte, sport, dport uint16) []byte {
	p := make([]byte, 24)
	p[0] = 0x45
	binary.BigEndian.PutUint16(p[2:4], 28)
	p[9] = 17
	copy(p[12:16], src[:])
	copy(p[16:20], dst[:])
	binary.BigEndian.PutUint16(p[20:22], sport)
	binary.BigEndian.PutUint16(p[22:24], dport)
	return p
}

// 短连接正常关闭靠 FIN 回收：本包仍走原槽（不制造这次乱序），随后删掉映射，
// 后续同 key 的包重新分配。不回收的话长会话里表会被已关闭的连接吃满。
func TestFlowTableReclaimsTCPOnFIN(t *testing.T) {
	ft := NewFlowTable()
	p := buildTCPWithFlags([4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}, 40000, 443, 0x10) // ACK
	first := ft.Slot(p, 4, nil)
	if st := ft.Stats(); st.Size != 1 {
		t.Fatalf("插入后 Size 应为 1，实际 %d", st.Size)
	}

	p[33] = 0x11 // FIN|ACK，同一条流
	got := ft.Slot(p, 4, nil)
	if got != first {
		t.Fatalf("FIN 包换了槽：%d -> %d（本包不该乱序）", first, got)
	}
	st := ft.Stats()
	if st.Size != 0 {
		t.Fatalf("FIN 后映射应被回收，Size=%d", st.Size)
	}
	if st.Reclaimed != 1 {
		t.Fatalf("回收计数应为 1，实际 %d", st.Reclaimed)
	}
}

// RST 同样立即回收。
func TestFlowTableReclaimsTCPOnRST(t *testing.T) {
	ft := NewFlowTable()
	p := buildTCPWithFlags([4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}, 40001, 443, 0x10)
	ft.Slot(p, 4, nil)
	p[33] = 0x14 // RST|ACK
	ft.Slot(p, 4, nil)
	if st := ft.Stats(); st.Size != 0 || st.Reclaimed != 1 {
		t.Fatalf("RST 后应回收一条：Size=%d Reclaimed=%d", st.Size, st.Reclaimed)
	}
}

// 表满时先回收超过 TCP 空闲期的条目，腾出空间给新流；能腾出就不算回退。
func TestFlowTableSweepsStaleTCPWhenFull(t *testing.T) {
	base := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	ft := NewFlowTable()
	ft.max = 2
	ft.clock = func() time.Time { return base }
	ft.Slot(buildTCPWithFlags([4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}, 50000, 443, 0x10), 4, nil)
	ft.Slot(buildTCPWithFlags([4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}, 50001, 443, 0x10), 4, nil)

	base = base.Add(tcpFlowIdle + time.Second)
	newFlow := buildTCPWithFlags([4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}, 50002, 443, 0x10)
	got := ft.Slot(newFlow, 4, nil)
	if got == 0 {
		t.Fatal("新流不该落到控制槽")
	}
	st := ft.Stats()
	if st.Size != 1 {
		t.Fatalf("sweep 后应只剩新流，Size=%d", st.Size)
	}
	if st.Fallbacks != 0 {
		t.Fatalf("腾出空间后不该回退，Fallbacks=%d", st.Fallbacks)
	}
	if st.Reclaimed != 2 {
		t.Fatalf("应回收两条过期流，实际 %d", st.Reclaimed)
	}
}

// UDP 没有结束标记，只能按空闲期回收。
func TestFlowTableUDPIdleReclaim(t *testing.T) {
	base := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	ft := NewFlowTable()
	ft.max = 1
	ft.clock = func() time.Time { return base }
	ft.Slot(buildUDPFlow([4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}, 50000, 53), 4, nil)

	base = base.Add(udpFlowIdle + time.Second)
	newFlow := buildTCPWithFlags([4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}, 50002, 443, 0x10)
	ft.Slot(newFlow, 4, nil)
	st := ft.Stats()
	if st.Size != 1 || st.Reclaimed != 1 || st.Fallbacks != 0 {
		t.Fatalf("UDP 空闲条应被回收并腾出位置：Size=%d Reclaimed=%d Fallbacks=%d",
			st.Size, st.Reclaimed, st.Fallbacks)
	}
}

// 回收不能误删活跃流：普通 TCP 流空闲期没到就必须保持原槽，否则同一内层流
// 会因为清扫换槽，把 TCP 乱序掉。
func TestFlowTableSweepKeepsActiveFlow(t *testing.T) {
	base := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	ft := NewFlowTable()
	ft.max = 1
	ft.clock = func() time.Time { return base }
	active := buildTCPWithFlags([4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}, 50000, 443, 0x10)
	first := ft.Slot(active, 4, nil)

	base = base.Add(30 * time.Second) // 远未到空闲期
	ft.Slot(buildTCPWithFlags([4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}, 60000, 443, 0x10), 4, nil)

	if got := ft.Slot(active, 4, nil); got != first {
		t.Fatalf("活跃流被清扫换了槽：%d -> %d", first, got)
	}
	if st := ft.Stats(); st.Size != 1 {
		t.Fatalf("活跃流不该被回收，Size=%d", st.Size)
	}
}

// 回收路径也要维护计数：漏减会让某个槽一直背着虚高的流数，再也没人选它。
func TestFlowTableReclaimKeepsCounts(t *testing.T) {
	ft := NewFlowTable()
	for i := 0; i < 6; i++ {
		p := buildTCPWithFlags([4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}, uint16(40000+i), 443, 0x10)
		ft.Slot(p, 4, nil)
	}
	checkFlowCounts(t, ft)

	for i := 0; i < 3; i++ {
		p := buildTCPWithFlags([4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}, uint16(40000+i), 443, 0x11)
		ft.Slot(p, 4, nil) // FIN 回收
	}
	checkFlowCounts(t, ft)
	if st := ft.Stats(); st.Size != 3 || st.Reclaimed != 3 {
		t.Fatalf("回收后 Size=%d Reclaimed=%d，期望 3/3", st.Size, st.Reclaimed)
	}
}

// 空闲回收不能只在表满时才跑：表没满时也要定期清，否则 flow 会一路涨到上限
// 才回落，看起来就是「回收不理想」。
func TestFlowTableSweepsStaleWhenNotFull(t *testing.T) {
	base := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	ft := NewFlowTable()
	ft.max = 10
	ft.clock = func() time.Time { return base }
	ft.Slot(buildTCPWithFlags([4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}, 50000, 443, 0x10), 4, nil)
	if ft.Stats().Size != 1 {
		t.Fatalf("插入后 Size 应为 1，实际 %d", ft.Stats().Size)
	}

	base = base.Add(tcpFlowIdle + time.Second)
	ft.Slot(buildTCPWithFlags([4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}, 50001, 443, 0x10), 4, nil)

	st := ft.Stats()
	if st.Size != 1 {
		t.Fatalf("表没满时也应清掉空闲流，Size=%d", st.Size)
	}
	if st.Reclaimed != 1 {
		t.Fatalf("应回收一条空闲流，实际 %d", st.Reclaimed)
	}
	if st.Fallbacks != 0 {
		t.Fatalf("腾出空间后不该回退，Fallbacks=%d", st.Fallbacks)
	}
}

// Stats 要能区分 TCP/UDP：回收期该缩哪一个，取决于表里哪类流多。
func TestFlowStatsProtocols(t *testing.T) {
	ft := NewFlowTable()
	ft.Slot(buildTCPWithFlags([4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}, 50000, 443, 0x10), 4, nil)
	ft.Slot(buildUDPFlow([4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}, 50001, 53), 4, nil)

	st := ft.Stats()
	if st.TCPCount != 1 || st.UDPCount != 1 {
		t.Fatalf("TCP/UDP 计数不对：tcp=%d udp=%d", st.TCPCount, st.UDPCount)
	}
	if st.OtherCount != 0 {
		t.Fatalf("两条 TCP/UDP 流不该算进其他协议，OtherCount=%d", st.OtherCount)
	}
}

// 空闲清扫删掉一条流后留 30 秒墓碑：窗口内同 key 的包回来要恢复原槽，
// 否则每清扫一次就可能让一条安静的长连接换槽、内层 TCP 乱序一次。
func TestFlowTableRecentRestoresSlot(t *testing.T) {
	base := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	ft := NewFlowTable()
	ft.max = 10
	ft.clock = func() time.Time { return base }
	p := buildTCPWithFlags([4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}, 50000, 443, 0x10)
	first := ft.Slot(p, 4, nil)

	base = base.Add(tcpFlowIdle + time.Second)
	ft.Slot(buildTCPWithFlags([4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}, 50001, 443, 0x10), 4, nil)

	base = base.Add(10 * time.Second)
	if got := ft.Slot(p, 4, nil); got != first {
		t.Fatalf("墓碑窗口内应恢复原槽 %d，实际 %d", first, got)
	}
}

// 清扫必须节流：每秒最多一次，不能每个包都遍历全表。
func TestFlowTableSweepThrottled(t *testing.T) {
	base := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	ft := NewFlowTable()
	ft.max = 10
	ft.clock = func() time.Time { return base }
	ft.Slot(buildTCPWithFlags([4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}, 50000, 443, 0x10), 4, nil)

	// 手动把已有流改成过期：这样不必推进 5 分钟也能验证节流。
	ft.mu.Lock()
	for k, e := range ft.m {
		e.lastSeen = base.Add(-tcpFlowIdle - time.Second).UnixNano()
		ft.m[k] = e
	}
	ft.mu.Unlock()

	// 距上次清扫不到 1 秒：不该扫，过期的流还在。
	base = base.Add(500 * time.Millisecond)
	ft.Slot(buildTCPWithFlags([4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}, 50001, 443, 0x10), 4, nil)
	if got := ft.Stats().Size; got != 2 {
		t.Fatalf("1 秒内不该清扫，Size=%d", got)
	}

	// 超过 1 秒后再来一条：触发清扫，过期的 50000 被删。
	base = base.Add(600 * time.Millisecond)
	ft.Slot(buildTCPWithFlags([4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}, 50002, 443, 0x10), 4, nil)
	st := ft.Stats()
	if st.Size != 2 || st.Reclaimed != 1 {
		t.Fatalf("超过 1 秒应清扫过期流：Size=%d Reclaimed=%d", st.Size, st.Reclaimed)
	}
}
