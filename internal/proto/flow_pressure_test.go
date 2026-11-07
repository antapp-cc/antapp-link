package proto

import (
	"testing"
	"time"
)

func TestFlowTableNewFlowPrefersQueueWithLessBytes(t *testing.T) {
	ft := NewFlowTable()
	packet := buildTCP([4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}, 45000, 443)
	loads := []int64{0, 1024, 0, 0}

	got := ft.SlotWithPressure(packet, 4, nil, loads)
	if got != 2 {
		t.Fatalf("new flow should avoid queued slot 1; got slot %d", got)
	}
}

func TestFlowTableExistingFlowKeepsSlotWhenQueuePressureChanges(t *testing.T) {
	ft := NewFlowTable()
	packet := buildTCP([4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}, 45001, 443)
	first := ft.SlotWithPressure(packet, 4, nil, []int64{0, 0, 1024, 0})
	got := ft.SlotWithPressure(packet, 4, nil, []int64{0, 4096, 0, 0})
	if got != first {
		t.Fatalf("existing flow moved from slot %d to %d", first, got)
	}
}

func TestFlowTableFullFallbackPrefersQueueWithLessBytesAndStaysPinned(t *testing.T) {
	ft := NewFlowTable()
	ft.max = 1
	filler := buildTCP([4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}, 45003, 443)
	ft.SlotWithPressure(filler, 4, nil, []int64{0, 0, 0, 0})
	packet := buildTCP([4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}, 45002, 443)
	loads := []int64{0, 2048, 0, 0}

	first := ft.SlotWithPressure(packet, 4, nil, loads)
	if first != 2 {
		t.Fatalf("fallback flow should choose least queued slot 2; got %d", first)
	}
	got := ft.SlotWithPressure(packet, 4, nil, []int64{0, 0, 8192, 0})
	if got != first {
		t.Fatalf("fallback flow moved from slot %d to %d", first, got)
	}
}

func TestFlowTableUntrackedFallbackKeepsStableHashSlot(t *testing.T) {
	ft := NewFlowTable()
	ft.max = 1
	filler := buildTCP([4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}, 45004, 443)
	ft.SlotWithPressure(filler, 4, nil, []int64{0, 0, 0, 0})
	ft.fallback = make(map[flowKey]flowEntry, maxFallback)
	for i := 0; i < maxFallback; i++ {
		var key flowKey
		key.b[0], key.len = byte(i>>8), 2
		key.b[1] = byte(i)
		ft.fallback[key] = flowEntry{slot: 1, lastSeen: ft.now().UnixNano(), proto: 6}
	}
	if len(ft.fallback) != maxFallback {
		t.Fatalf("fallback table size = %d, want %d", len(ft.fallback), maxFallback)
	}

	packet := buildTCP([4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}, 45005, 443)
	first := ft.SlotWithPressure(packet, 4, nil, []int64{0, 4096, 0, 0})
	got := ft.SlotWithPressure(packet, 4, nil, []int64{0, 0, 4096, 0})
	if got != first {
		t.Fatalf("untracked fallback flow changed slot from %d to %d", first, got)
	}
}

func TestFlowTableOverflowKeepsAffinityWhenCapacityReturns(t *testing.T) {
	for _, freeMain := range []bool{false, true} {
		name := "fallback_space"
		if freeMain {
			name = "main_space"
		}
		t.Run(name, func(t *testing.T) {
			ft := NewFlowTable()
			ft.max = 1
			src, dst := [4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}
			loads := []int64{0, 0, 0, 0}
			route := func(p []byte) int { return ft.SlotWithPressure(p, 4, nil, loads) }
			route(buildTCP(src, dst, 9000, 443))
			for i := 0; i < maxFallback; i++ {
				route(buildTCP(src, dst, uint16(10000+i), 443))
			}
			pinned := route(buildTCP(src, dst, 10001, 443))
			p := buildTCP(src, dst, 45005, 443)
			first := route(p)
			loads[first] = 8192
			port := uint16(10000)
			if freeMain {
				port = 9000
			}
			route(buildTCPWithFlags(src, dst, port, 443, 0x14))
			if freeMain {
				ft.SlotWithPressure(p, 4, func(int) bool { return false }, loads)
				if len(ft.m) != 0 {
					t.Fatal("全员失效时登记了死槽映射")
				}
			}
			// 旁路表腾出位置后这条流第一次能登记：此刻按负载选一次槽（溢出期间它只能
			// 按哈希走，这次迁移不可避免），之后必须固定。
			got := route(p)
			if again := route(p); again != got {
				t.Fatalf("登记后仍未固定: %d -> %d", got, again)
			}
			if got := route(buildTCP(src, dst, 10001, 443)); got != pinned {
				t.Fatalf("已登记旁路流换槽: %d -> %d", pinned, got)
			}
			checkFlowCounts(t, ft)
		})
	}
}

func TestFlowTableOverflowedFlowKeepsHashSlotAcrossMemberRecovery(t *testing.T) {
	ft := NewFlowTable()
	ft.max = 1
	src, dst := [4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}
	loads := []int64{0, 0, 0, 0}
	ft.SlotWithPressure(buildTCP(src, dst, 9000, 443), 4, nil, loads)
	for i := 0; i < maxFallback; i++ {
		ft.SlotWithPressure(buildTCP(src, dst, uint16(10000+i), 443), 4, nil, loads)
	}
	p := buildTCP(src, dst, 52000, 443)
	first := ft.SlotWithPressure(p, 4, nil, loads)
	if !ft.fallbackOverflowed {
		t.Fatal("测试没有触发旁路表溢出")
	}
	// 溢出后不再按队列压力逐包选槽，同一流保持固定。
	if got := ft.SlotWithPressure(p, 4, nil, loads); got != first {
		t.Fatalf("溢出流换槽: %d -> %d", first, got)
	}
	// 原槽失效时允许迁移到活槽：溢出不等于把整条流黑洞到死槽。
	alive := func(k int) bool { return k != first }
	moved := ft.SlotWithPressure(p, 4, alive, loads)
	if moved == first {
		t.Fatalf("原槽失效仍留在死槽 %d", first)
	}
	// 旁路表满、无法登记时哈希是稳定的：原槽恢复后回到同一个哈希槽，
	// 不会因为存活集合变化再换一次。
	if got := ft.SlotWithPressure(p, 4, nil, loads); got != first {
		t.Fatalf("原槽恢复后流从 %d 迁到了 %d", first, got)
	}
}

func TestFINRetainsLiveConnectionAffinity(t *testing.T) {
	for _, pressure := range []bool{false, true} {
		name := "flow_count_control"
		if pressure {
			name = "current_pressure"
		}
		t.Run(name, func(t *testing.T) {
			ft := NewFlowTable()
			src, dst := [4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}
			loads := []int64{0, 0, 0, 0}
			route := func(p []byte) int {
				if pressure {
					return ft.SlotWithPressure(p, 4, nil, loads)
				}
				return ft.Slot(p, 4, nil)
			}
			first := route(buildTCPWithFlags(src, dst, 46000, 443, 0x02))
			fin := route(buildTCPWithFlags(src, dst, 46000, 443, 0x11))
			loads[first] = 8192
			next := route(buildTCPWithFlags(src, dst, 46000, 443, 0x10))
			t.Logf("all slots alive: SYN=%d FIN=%d subsequent ACK=%d", first, fin, next)
			if next != first {
				t.Fatalf("FIN half-close changed a live connection's slot: %d -> %d", first, next)
			}
		})
	}
}

func TestFallbackFINRetainsAffinity(t *testing.T) {
	ft := NewFlowTable()
	ft.max = 1
	src, dst := [4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}
	loads := []int64{0, 0, 0, 0}
	ft.SlotWithPressure(buildTCPWithFlags(src, dst, 45999, 443, 0x02), 4, nil, loads)
	p := buildTCPWithFlags(src, dst, 46000, 443, 0x02)
	first := ft.SlotWithPressure(p, 4, nil, loads)
	ft.SlotWithPressure(buildTCPWithFlags(src, dst, 46000, 443, 0x11), 4, nil, loads)
	loads[first] = 8192
	next := ft.SlotWithPressure(buildTCPWithFlags(src, dst, 46000, 443, 0x10), 4, nil, loads)
	t.Logf("fallback live connection after FIN: %d -> %d", first, next)
	if next != first {
		t.Fatal("fallback FIN cleared affinity before the TCP connection completed")
	}
}

func TestFINAffinityRefreshesUntilIdleInBothTables(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		name := "main"
		if fallback {
			name = "fallback"
		}
		t.Run(name, func(t *testing.T) {
			now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
			ft := NewFlowTable()
			ft.clock = func() time.Time { return now }
			src, dst := [4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}
			load := []int64{0, 0, 0, 0}
			filler := buildTCPWithFlags(src, dst, 46999, 443, 0x10)
			if fallback {
				ft.max = 1
				ft.SlotWithPressure(filler, 4, nil, load)
			}
			p := buildTCPWithFlags(src, dst, 47000, 443, 0x11)
			first := ft.SlotWithPressure(p, 4, nil, load)
			load[first] = 8192
			for i := 0; i < 3; i++ {
				now = now.Add(4 * time.Minute)
				if fallback {
					ft.SlotWithPressure(filler, 4, nil, load)
				}
				p[33] = 0x10
				if i == 1 {
					p[33] = 0x11
				}
				if got := ft.SlotWithPressure(p, 4, nil, load); got != first {
					t.Fatalf("半关闭活跃期换槽: %d -> %d", first, got)
				}
			}
			now = now.Add(tcpFlowIdle + time.Second)
			ft.mu.Lock()
			ft.sweepLocked(now)
			ft.mu.Unlock()
			if len(ft.m) != 0 || len(ft.fallback) != 0 {
				t.Fatal("FIN静默项未按空闲期限回收")
			}
			checkFlowCounts(t, ft)
			if got := ft.SlotWithPressure(p, 4, nil, load); got != first {
				t.Fatalf("近期空闲回收后亲和未恢复: %d -> %d", first, got)
			}
		})
	}
}
func TestRecentFallbackPinsRemainWhenMainRestoreIsFull(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	ft := NewFlowTable()
	ft.max = 1
	ft.clock = func() time.Time { return now }
	src, dst := [4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}
	load := []int64{0, 0, 0, 0}
	filler := buildTCPWithFlags(src, dst, 48000, 443, 0x10)
	ft.SlotWithPressure(filler, 4, nil, load)
	a, b := buildTCPWithFlags(src, dst, 48001, 443, 0x11), buildTCPWithFlags(src, dst, 48002, 443, 0x11)
	sa := ft.SlotWithPressure(a, 4, nil, load)
	load[sa] = 1024
	sb := ft.SlotWithPressure(b, 4, nil, load)
	now = now.Add(tcpFlowIdle + time.Second)
	ft.SlotWithPressure(filler, 4, nil, load)
	ft.mu.Lock()
	ft.sweepLocked(now)
	ft.mu.Unlock()
	if got := ft.SlotWithPressure(a, 4, nil, load); got != sa {
		t.Fatal("首个恢复流换槽")
	}
	load[sb] = 8192
	for i := 0; i < 3; i++ {
		if got := ft.SlotWithPressure(b, 4, nil, load); got != sb {
			t.Fatalf("恢复空间不足导致换槽: %d -> %d", sb, got)
		}
		now = now.Add(30 * time.Second)
	}
	if len(ft.m) > ft.max+1 {
		t.Fatal("恢复导致主表无界增长")
	}
	checkFlowCounts(t, ft)
	b[33] = 0x14
	ft.SlotWithPressure(b, 4, nil, load)
	key, _, _, _ := flowKeyMeta(b)
	if _, ok := ft.recent[key]; ok {
		t.Fatal("RST未清理待恢复映射")
	}
}
func TestUnknownRSTDoesNotCreateMapping(t *testing.T) {
	ft := NewFlowTable()
	p := buildTCPWithFlags([4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}, 49000, 443, 0x14)
	ft.SlotWithPressure(p, 4, nil, []int64{0, 0, 0, 0})
	if ft.Stats().Size != 0 {
		t.Fatal("未知RST创建了持久映射")
	}
}

func TestRSTUsesLiveSlotAfterOriginalMemberFails(t *testing.T) {
	ft := NewFlowTable()
	p := buildTCPWithFlags([4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}, 49100, 443, 0x10)
	first := ft.Slot(p, 4, nil)
	p[33] = 0x14
	alive := func(k int) bool { return k != first }
	if got := ft.SlotWithPressure(p, 4, alive, []int64{0, 0, 0, 0}); !alive(got) {
		t.Fatalf("RST仍走死槽: %d", got)
	}
	if st := ft.Stats(); st.Size != 0 || st.Reclaimed != 1 {
		t.Fatalf("RST回收异常: %+v", st)
	}
	checkFlowCounts(t, ft)
}
func TestZeroValueFallbackCanExpireAndRestore(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	var ft FlowTable
	ft.clock = func() time.Time { return now }
	p := buildTCPWithFlags([4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}, 49200, 443, 0x11)
	first := ft.Slot(p, 4, nil)
	now = now.Add(tcpFlowIdle + time.Second)
	ft.mu.Lock()
	ft.sweepLocked(now)
	ft.mu.Unlock()
	if got := ft.Slot(p, 4, nil); got != first {
		t.Fatalf("零值流表回收后换槽: %d -> %d", first, got)
	}
	checkFlowCounts(t, &ft)
}

func TestKnownFlowTrafficReclaimsIdleFINMappings(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	ft := NewFlowTable()
	ft.max = 2
	ft.clock = func() time.Time { return now }
	src, dst := [4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}
	closing := buildTCPWithFlags(src, dst, 49300, 443, 0x11)
	active := buildTCPWithFlags(src, dst, 49301, 443, 0x10)
	fallback := buildTCPWithFlags(src, dst, 49302, 443, 0x11)
	ft.Slot(closing, 4, nil)
	ft.Slot(active, 4, nil)
	ft.Slot(fallback, 4, nil)
	now = now.Add(4 * time.Minute)
	ft.Slot(active, 4, nil)
	now = now.Add(2 * time.Minute)
	ft.Slot(active, 4, nil)
	if st := ft.Stats(); st.Size != 1 || len(ft.fallback) != 0 || st.Reclaimed != 1 {
		t.Fatalf("只有已有流活动时未清理静默FIN映射: %+v fallback=%d", st, len(ft.fallback))
	}
	checkFlowCounts(t, ft)
}
