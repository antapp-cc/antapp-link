package proto

import "testing"

// 记过的槽已经断开时必须重选活槽，不能继续把包发到死槽上。
// 发到死槽 = 整批被 flushBatch 丢弃，表现是「显示在线但这条流不动」。
func TestFlowFallbackReselectsWhenSlotDead(t *testing.T) {
	ft := NewFlowTable()
	ft.max = 2

	dst := [4]byte{8, 8, 8, 8}
	src := [4]byte{10, 0, 0, 2}
	// 先把表塞满
	ft.Slot(buildTCP(src, dst, 1111, 443), 4, nil)
	ft.Slot(buildTCP(src, dst, 2222, 443), 4, nil)
	if len(ft.m) != 2 {
		t.Fatalf("表应已满，实际 %d 条", len(ft.m))
	}

	p := buildTCP(src, dst, 3333, 443)
	first := ft.Slot(p, 4, nil)
	// 模拟 first 所在成员闪断：alive 集合不再包含它
	alive := func(k int) bool { return k != first }
	second := ft.Slot(p, 4, alive)
	if second == first {
		t.Fatalf("槽 %d 已断开，不该继续把包发它", first)
	}
	if !alive(second) {
		t.Fatalf("重选结果 %d 必须是活槽", second)
	}
	// 重选后旁路表更新为新槽，成员恢复后沿用新值（不会跳回旧值再乱序一次）
	if got := ft.Slot(p, 4, nil); got != second {
		t.Fatalf("恢复后应沿用重选后的槽 %d，实际 %d", second, got)
	}
}

// 记过的槽号越界（槽数变少）时必须重选，直接把包发到不存在的槽会整批丢弃。
func TestFlowFallbackRejectsOutOfRangeSlot(t *testing.T) {
	ft := NewFlowTable()
	ft.max = 1
	dst := [4]byte{8, 8, 8, 8}
	src := [4]byte{10, 0, 0, 2}
	ft.Slot(buildTCP(src, dst, 1111, 443), 4, nil) // 表满

	p := buildTCP(src, dst, 4444, 443)
	ft.Slot(p, 4, nil) // 先记一条
	key, _, _, ok := flowKeyMeta(p)
	if !ok {
		t.Fatal("构造的包应能解析出 flow key")
	}
	ft.fallback[key] = 9 // 人为写一个越界槽（n=4）

	got := ft.Slot(p, 4, nil)
	if got <= 0 || got >= 4 {
		t.Fatalf("越界槽应被重选，实际返回 %d", got)
	}
}

// 流结束时（FIN）要把旁路表里的条目一起清掉，否则短连接会把 1024 容量吃满。
func TestFlowFallbackClearedOnFIN(t *testing.T) {
	ft := NewFlowTable()
	ft.max = 1
	dst := [4]byte{8, 8, 8, 8}
	src := [4]byte{10, 0, 0, 2}
	ft.Slot(buildTCP(src, dst, 1111, 443), 4, nil)

	p := buildTCP(src, dst, 4444, 443)
	ft.Slot(p, 4, nil)
	key, _, _, ok := flowKeyMeta(p)
	if !ok {
		t.Fatal("构造的包应能解析出 flow key")
	}
	if _, exists := ft.fallback[key]; !exists {
		t.Fatal("表满时的回退流应记进旁路表")
	}

	fin := buildTCPWithFlags(src, dst, 4444, 443, 0x11) // FIN|ACK
	ft.Slot(fin, 4, nil)
	if _, exists := ft.fallback[key]; exists {
		t.Fatal("FIN 之后旁路表里的条目应被清掉")
	}
}

// 表满过又降下来的过渡期：旁路表里的流要沿用原槽（转正），不能当新流重新选槽，
// 否则会换槽一次、内层 TCP 乱序。
func TestFlowFallbackPromotesWhenTableHasRoom(t *testing.T) {
	ft := NewFlowTable()
	ft.max = 2
	dst := [4]byte{8, 8, 8, 8}
	src := [4]byte{10, 0, 0, 2}
	a := buildTCP(src, dst, 1111, 443)
	ft.Slot(a, 4, nil)
	ft.Slot(buildTCP(src, dst, 2222, 443), 4, nil) // 表满

	p := buildTCP(src, dst, 3333, 443)
	first := ft.Slot(p, 4, nil) // 记进旁路表

	// 用 FIN 回收一条，表降到不满
	ft.Slot(buildTCPWithFlags(src, dst, 1111, 443, 0x11), 4, nil)
	if len(ft.m) != 1 {
		t.Fatalf("表应降到 1 条，实际 %d", len(ft.m))
	}

	if got := ft.Slot(p, 4, nil); got != first {
		t.Fatalf("过渡期应沿用旁路表的槽：first=%d got=%d", first, got)
	}
	key, _, _, ok := flowKeyMeta(p)
	if !ok {
		t.Fatal("构造的包应能解析出 flow key")
	}
	if _, exists := ft.fallback[key]; exists {
		t.Fatal("转正后应清掉旁路条目")
	}
	if _, exists := ft.m[key]; !exists {
		t.Fatal("转正后应写进主映射")
	}
}
