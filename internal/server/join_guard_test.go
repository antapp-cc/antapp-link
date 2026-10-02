package server

import (
	"testing"
	"time"
)

// 熔断要真的拦住：窗口内失败超阈值就拒绝该 sid 的后续加入，窗口过后自动恢复，
// 且会话结束时清掉计数（不能像全局 map 那样只涨不消）。
func TestJoinGuardBlocksAfterThreshold(t *testing.T) {
	g := newJoinGuard()
	now := time.Now()

	if !g.allow("sid-1", now) {
		t.Fatal("尚未失败时应允许加入")
	}
	for i := 1; i <= joinMaxFail; i++ {
		hit := g.fail("sid-1", now)
		if want := i == joinMaxFail; hit != want {
			t.Fatalf("第 %d 次失败：hit=%v，期望 %v（阈值只该报一次）", i, hit, want)
		}
	}
	if g.allow("sid-1", now) {
		t.Fatal("达到阈值后必须拒绝该 sid 的后续加入")
	}
	if !g.allow("sid-2", now) {
		t.Fatal("别的 sid 不该被连坐")
	}
	if got := g.failures("sid-1"); got != joinMaxFail {
		t.Fatalf("failures = %d，期望 %d", got, joinMaxFail)
	}

	// 窗口过后恢复尝试，计数从头开始
	later := now.Add(joinWindow + time.Second)
	if !g.allow("sid-1", later) {
		t.Fatal("窗口过后应重新允许加入")
	}
	if got := g.failures("sid-1"); got != 0 {
		t.Fatalf("窗口过期后计数应归零，实际 %d", got)
	}

	if g.fail("sid-1", later) {
		t.Fatal("窗口过期后第一次失败不该直接算作达到阈值")
	}
	g.forget("sid-1")
	if got := g.failures("sid-1"); got != 0 {
		t.Fatalf("forget 后计数应为 0，实际 %d", got)
	}
}
