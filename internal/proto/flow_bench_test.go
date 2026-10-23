package proto

import (
	"fmt"
	"testing"
	"time"
)

// 流表的新流分配原本要遍历整张映射统计每槽流数：表越满，单次分配越慢，累计 O(N²)。
// 下面两个基准分别量「固定表大小下单次分配」和「从空表填到上限」，优化前后跑同一份
// 代码即可对比。
func benchPkts(n int) [][]byte {
	pkts := make([][]byte, n)
	for i := range pkts {
		pkts[i] = buildTCP([4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}, uint16(1024+i), 443)
	}
	return pkts
}

// BenchmarkFlowTableNewFlow 固定表内已有 filled 条流，反复量「一条新流选槽」的耗时。
// 每轮插完就把探针流从映射里删掉，让下一轮仍走新流分支；只删映射不动计数，因为这里
// 量的是选槽这一步的复杂度，计数是否精确由正确性测试负责。
func BenchmarkFlowTableNewFlow(b *testing.B) {
	pkts := benchPkts(12001)
	for _, filled := range []int{1000, 12000} {
		b.Run(fmt.Sprintf("filled=%d", filled), func(b *testing.B) {
			ft := NewFlowTable()
			for j := 0; j < filled; j++ {
				ft.Slot(pkts[j], 4, nil)
			}
			ft.max = filled + 1
			probe := pkts[12000]
			key, _ := flowKeyOf(probe)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				ft.Slot(probe, 4, nil)
				ft.mu.Lock()
				delete(ft.m, key)
				ft.mu.Unlock()
			}
		})
	}
}

// BenchmarkFlowTableFill 从空表连续插入 size 条互不相同的流直到上限，量累计代价。
func BenchmarkFlowTableFill(b *testing.B) {
	pkts := benchPkts(maxFlows)
	for _, size := range []int{1000, 5000, 20000} {
		b.Run(fmt.Sprintf("flows=%d", size), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				ft := NewFlowTable()
				for j := 0; j < size; j++ {
					ft.Slot(pkts[j], 4, nil)
				}
			}
			b.ReportMetric(float64(size), "flows/op")
		})
	}
}

// checkFlowCounts 校验每槽计数与映射里的真实分布一致。计数漏加会让某个槽看起来永远
// 空着、新流全挤过去；漏减会让死槽背着虚高的流量、再也没人选它 —— 两种漂移都必须
// 在这次优化里被钉死。
func checkFlowCounts(t *testing.T, ft *FlowTable) {
	t.Helper()
	ft.mu.Lock()
	defer ft.mu.Unlock()
	want := make([]int, len(ft.counts))
	for _, e := range ft.m {
		if e.slot < 0 || e.slot >= len(want) {
			t.Fatalf("映射指向槽 %d，超出计数长度 %d", e.slot, len(want))
		}
		want[e.slot]++
	}
	for i := 1; i < len(want); i++ {
		if ft.counts[i] != want[i] {
			t.Fatalf("槽 %d 计数漂移：记录 %d，实际 %d", i, ft.counts[i], want[i])
		}
	}
}

// 计数要跟着每一条新增/迁移/删除路径走，也要跟着槽数增减走；任何一处漏掉，
// 均衡分配都会静默地歪掉。
func TestFlowTableCountsMatchMappings(t *testing.T) {
	ft := NewFlowTable()
	const n = 4
	flows := make([][]byte, 30)
	for i := range flows {
		flows[i] = buildTCP([4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}, uint16(40000+i), 443)
	}
	seen := map[int]int{}
	for _, p := range flows {
		seen[ft.Slot(p, n, nil)]++
	}
	if seen[1] != 10 || seen[2] != 10 || seen[3] != 10 {
		t.Fatalf("新流分配不均：%v", seen)
	}
	checkFlowCounts(t, ft)

	// 槽失效后的迁移：旧槽计数必须同步减一，否则它会一直背着那条流的虚高计数
	probe := buildTCP([4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}, 50000, 443)
	dead := ft.Slot(probe, n, nil)
	moved := ft.Slot(probe, n, func(k int) bool { return k != dead })
	if moved == dead || moved < 1 || moved >= n {
		t.Fatalf("迁移没有换到别的活槽：%d → %d", dead, moved)
	}
	if got := ft.counts[dead]; got != seen[dead] {
		t.Fatalf("迁移后死槽 %d 计数是 %d，期望 %d（原 %d 减 1）", dead, got, seen[dead], seen[dead]+1)
	}
	checkFlowCounts(t, ft)

	// 成员补拨、槽数变多：计数要扩到新长度，老映射一条都不能动
	for _, p := range flows {
		if got := ft.Slot(p, 8, nil); got >= n {
			t.Fatalf("槽数增加后老流被换到新槽 %d（内层 TCP 会乱序）", got)
		}
	}
	if got := ft.Slot(probe, 8, nil); got != moved {
		t.Fatalf("槽数增加后迁移过的流又换槽：%d → %d", moved, got)
	}
	checkFlowCounts(t, ft)

	// 成员断开、槽数变少：落在被移除槽上的映射只能重分，计数必须同步清掉
	for _, p := range flows {
		if got := ft.Slot(p, 2, nil); got != 1 {
			t.Fatalf("n=2 时唯一可用槽是 1，实际 %d", got)
		}
	}
	if got := ft.Slot(probe, 2, nil); got != 1 {
		t.Fatalf("n=2 时迁移过的流应落到槽 1，实际 %d", got)
	}
	checkFlowCounts(t, ft)
	if ft.counts[1] != len(ft.m) || ft.counts[2] != 0 {
		t.Fatalf("缩容后计数没跟上：counts=%v，映射 %d 条", ft.counts, len(ft.m))
	}

	// 槽再补回来：老流钉在槽 1，新流应优先填空着的槽 2、3
	for i := 0; i < 6; i++ {
		p := buildTCP([4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}, uint16(51000+i), 443)
		ft.Slot(p, n, nil)
	}
	if ft.counts[2] != 3 || ft.counts[3] != 3 {
		t.Fatalf("扩容后新流没有填进空槽：counts=%v", ft.counts)
	}
	checkFlowCounts(t, ft)
}

// 表满后的哈希回退只读不写：不能插映射、不能动计数，且同一个包必须稳定落在同一槽，
// 否则同一条内层流会在外层连接之间跳，TCP 直接乱序。
func TestFlowTableFullFallbackDoesNotTouchCounts(t *testing.T) {
	ft := NewFlowTable()
	ft.max = 4
	for i := 0; i < 4; i++ {
		ft.Slot(buildTCP([4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}, uint16(60000+i), 443), 4, nil)
	}
	checkFlowCounts(t, ft)
	wantCounts := append([]int(nil), ft.counts...)
	wantLen := len(ft.m)

	p := buildTCP([4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}, 61000, 443)
	first := ft.Slot(p, 4, nil)
	for i := 0; i < 20; i++ {
		ft.Slot(buildTCP([4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}, uint16(61001+i), 443), 4, nil)
	}
	if len(ft.m) != wantLen {
		t.Fatalf("回退路径插入了映射：%d → %d", wantLen, len(ft.m))
	}
	for i := range wantCounts {
		if ft.counts[i] != wantCounts[i] {
			t.Fatalf("回退路径污染了计数：槽 %d %d → %d", i, wantCounts[i], ft.counts[i])
		}
	}
	if got := ft.Slot(p, 4, nil); got != first {
		t.Fatalf("回退路径对同一个包不稳定：%d → %d", first, got)
	}
}

// 定期清扫每秒至少跑一次，测满表时一次全表遍历的代价。
func BenchmarkFlowTableSweepAtCapacity(b *testing.B) {
	ft := NewFlowTable()
	pkts := benchPkts(maxFlows)
	for j := 0; j < maxFlows; j++ {
		ft.Slot(pkts[j], 4, nil)
	}
	now := time.Now()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ft.mu.Lock()
		ft.sweepLocked(now)
		ft.mu.Unlock()
	}
}
