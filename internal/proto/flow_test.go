package proto

import (
	"encoding/binary"
	"sync"
	"testing"
)

// buildTCP 造一个 IPv4+TCP 包。flowKeyOf 只读前 24 字节，其余留零。
func buildTCP(src, dst [4]byte, sport, dport uint16) []byte {
	p := make([]byte, 24)
	p[0] = 0x45
	binary.BigEndian.PutUint16(p[2:4], 40)
	p[9] = 6
	copy(p[12:16], src[:])
	copy(p[16:20], dst[:])
	binary.BigEndian.PutUint16(p[20:22], sport)
	binary.BigEndian.PutUint16(p[22:24], dport)
	return p
}

// 同一条流必须永远同一槽 —— 内层 TCP 绝不能乱序，这是整个方案的基石。
func TestFlowTableStablePerFlow(t *testing.T) {
	ft := NewFlowTable()
	p := buildTCP([4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}, 40000, 443)
	first := ft.Slot(p, 5, nil)
	for i := 0; i < 100; i++ {
		if got := ft.Slot(p, 5, nil); got != first {
			t.Fatalf("第 %d 次得到槽 %d，首次是 %d：同一条流换槽会让内层 TCP 乱序", i, got, first)
		}
	}
}

// 方向无关：请求与回包算同一条流。
func TestFlowTableDirectionAgnostic(t *testing.T) {
	ft := NewFlowTable()
	a := buildTCP([4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}, 40000, 443)
	b := buildTCP([4]byte{8, 8, 8, 8}, [4]byte{10, 0, 0, 2}, 443, 40000)
	if x, y := ft.Slot(a, 4, nil), ft.Slot(b, 4, nil); x != y {
		t.Errorf("同一条流的两个方向落在不同槽：%d vs %d", x, y)
	}
}

// 新流要分散到各槽 —— 这正是纯哈希做不到的事（实测 4 条下出现过 1.4MB/1.4MB/58KB/0）。
func TestFlowTableSpreadsNewFlows(t *testing.T) {
	ft := NewFlowTable()
	seen := map[int]int{}
	for i := 0; i < 30; i++ {
		p := buildTCP([4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}, uint16(40000+i), 443)
		seen[ft.Slot(p, 4, nil)]++
	}
	for slot := 1; slot <= 3; slot++ {
		if seen[slot] != 10 {
			t.Errorf("槽 %d 分到 %d 条流，期望 10 条（%v）", slot, seen[slot], seen)
		}
	}
	if seen[0] != 0 {
		t.Errorf("新流不该落到控制连接槽 0：%v", seen)
	}
}

// 成员数变化时绝不能整表清空：那会让还在有序传输的流换槽，内层 TCP 直接乱序。
// n 增大时旧槽全部有效，一条映射都不该动；n 减小时只重分落到被移除槽上的条目。
func TestFlowTableKeepsMappingsWhenMemberCountChanges(t *testing.T) {
	ft := NewFlowTable()
	const total = 20
	keys := make([][]byte, total)
	slotOf := make([]int, total)
	for i := 0; i < total; i++ {
		keys[i] = buildTCP([4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}, uint16(40000+i), 443)
		slotOf[i] = ft.Slot(keys[i], 5, nil)
		if s := slotOf[i]; s < 1 || s > 4 {
			t.Fatalf("n=5 时槽位 %d 越界", s)
		}
	}

	// n 增大：旧槽全都还在，任何一条流换槽都是错的
	for i := 0; i < total; i++ {
		if got := ft.Slot(keys[i], 8, nil); got != slotOf[i] {
			t.Errorf("n 增大后第 %d 条流换了槽：%d → %d（内层 TCP 会乱序）", i, slotOf[i], got)
		}
	}

	// n 减到 2：只能落在槽 1，且不得越界
	for i := 0; i < total; i++ {
		if got := ft.Slot(keys[i], 2, nil); got != 1 {
			t.Errorf("n=2 时唯一可用的槽是 1，实际 %d", got)
		}
	}

	// 单连接模式仍只走控制连接
	if s := ft.Slot(keys[0], 1, nil); s != 0 {
		t.Errorf("单连接模式应返回 0，实际 %d", s)
	}
}

// 畸形/过短的包绝不能 panic。
func TestFlowTableMalformed(t *testing.T) {
	ft := NewFlowTable()
	full := buildTCP([4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}, 1, 2)
	cases := map[string][]byte{
		"空":       nil,
		"太短":      {0x45, 0x00},
		"非 IPv4":  append([]byte{0x65}, make([]byte, 23)...),
		"TCP 头截断": full[:20],
	}
	for name, p := range cases {
		if s := ft.Slot(p, 4, nil); s < 0 || s > 3 {
			t.Errorf("%s: 槽位 %d 越界", name, s)
		}
	}
}

// 表满时新流退回哈希，老流映射绝不能动（动一个就是乱序）。
func TestFlowTableFullFallsBackToHash(t *testing.T) {
	ft := NewFlowTable()
	ft.max = 8
	base := buildTCP([4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}, 40000, 443)
	first := ft.Slot(base, 4, nil)
	for i := 0; i < 20; i++ {
		p := buildTCP([4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}, uint16(50000+i), 443)
		ft.Slot(p, 4, nil)
	}
	if got := ft.Slot(base, 4, nil); got != first {
		t.Errorf("表满后老流换了槽：%d → %d", first, got)
	}
	if len(ft.m) > ft.max {
		t.Errorf("流表超过上限：%d > %d", len(ft.m), ft.max)
	}
}

// 两端都有多个 goroutine 发包，流表必须并发安全。
func TestFlowTableConcurrent(t *testing.T) {
	ft := NewFlowTable()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				p := buildTCP([4]byte{10, 0, 0, byte(g)}, [4]byte{8, 8, 8, 8}, uint16(40000+i), 443)
				if s := ft.Slot(p, 4, nil); s < 1 || s > 3 {
					t.Errorf("槽位越界: %d", s)
					return
				}
			}
		}(g)
	}
	wg.Wait()
}

// 新流绝不能分给已断开的空槽：那种流的下行会被 flushBatch 整批丢掉，那条流就再也
// 不通了，而且流表不会自我纠正 —— 只要流还在，它就一直往死槽发。
func TestFlowTableAvoidsDeadSlots(t *testing.T) {
	ft := NewFlowTable()
	// n=4，但槽 3 的成员连接断了
	alive := func(k int) bool { return k != 3 }
	seen := map[int]int{}
	for i := 0; i < 30; i++ {
		p := buildTCP([4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}, uint16(40000+i), 443)
		s := ft.Slot(p, 4, alive)
		if s == 3 {
			t.Fatalf("第 %d 条新流被分给了断开的槽 3", i)
		}
		seen[s]++
	}
	// 只剩槽 1、2 可用，30 条应各 15 条
	if seen[1] != 15 || seen[2] != 15 {
		t.Errorf("活槽之间分配不均：%v（期望槽 1、2 各 15）", seen)
	}

	// 一个活槽都没有时退回哈希，不 panic、也不把流钉死在某个槽
	none := func(int) bool { return false }
	p := buildTCP([4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}, 9, 9)
	if s := ft.Slot(p, 4, none); s < 1 || s > 3 {
		t.Errorf("无活槽时应退回纯哈希（1..3），实际 %d", s)
	}
}
