package proto

import (
	"testing"
	"time"
)

func TestFlowOverflowDiagnosticsSurviveIdleSweep(t *testing.T) {
	now := time.Now()
	ft := NewFlowTable()
	ft.max = 1
	ft.clock = func() time.Time { return now }
	packet := func(i int) []byte { return buildTCP([4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}, uint16(1000+i), 443) }
	for i := 0; i <= maxFallback; i++ {
		ft.Slot(packet(i), 4, nil)
	}
	p := packet(maxFallback + 1)
	dead := Slot(p, 4)
	alive := func(k int) bool { return k != dead }

	// 溢出后全新流也不能被送进死槽：首次按活槽哈希选槽。
	if got := ft.Slot(p, 4, alive); got == dead {
		t.Fatalf("溢出后新流被送进死槽 %d", dead)
	}
	if st := ft.Stats(); !st.Overflowed || st.FallbackSize != maxFallback {
		t.Fatalf("缺少溢出诊断: %+v", st)
	}

	// 空闲清扫把旁路表清空后，「曾溢出」这个状态仍要可见。
	now = now.Add(6 * time.Minute)
	ft.Slot(packet(0), 4, nil)
	if !ft.Stats().Overflowed {
		t.Fatal("溢出状态被空闲清扫复位")
	}

	// 新会话必须复位。
	if st := NewFlowTable().Stats(); st.Overflowed || st.FallbackSize != 0 || st.OverflowDeadSelections != 0 {
		t.Fatalf("新会话没有复位: %+v", st)
	}
}

func TestFlowDiagnosticsDoNotCountNormalMemberReselection(t *testing.T) {
	ft := NewFlowTable()
	p := buildTCP([4]byte{10, 0, 0, 2}, [4]byte{8, 8, 8, 8}, 1234, 443)
	first := ft.Slot(p, 4, nil)
	if got := ft.Slot(p, 4, func(k int) bool { return k != first }); got == first {
		t.Fatal("未溢出的已登记流没有避开故障成员")
	}
	st := ft.Stats()
	if st.Overflowed || st.FallbackSize != 0 || st.OverflowDeadSelections != 0 {
		t.Fatalf("正常成员重选被误记为溢出: %+v", st)
	}
}
