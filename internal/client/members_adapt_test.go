package client

import (
	"io"
	"log/slog"
	"testing"
	"time"
)

func newAdaptTestApp() *App {
	return &App{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// 连续失败才降档：单次失败只是链路抖一下，立刻降会让连接数在临界点上跳。
func TestMembersAdaptLowersAfterTwoFails(t *testing.T) {
	a := newAdaptTestApp()
	if got := a.currentMembers(4); got != 4 {
		t.Fatalf("初始应取连接码的值 4，实际 %d", got)
	}
	a.noteProbeResult(false, 4)
	if got := a.currentMembers(4); got != 4 {
		t.Errorf("单次失败不该降档，实际 %d", got)
	}
	a.noteProbeResult(false, 4)
	if got := a.currentMembers(4); got != 3 {
		t.Errorf("连续两次失败应降到 3，实际 %d", got)
	}

	// 每降一档要再攒两次失败，多给几轮确保能降到 1
	for i := 0; i < 8; i++ {
		a.noteProbeResult(false, 4)
	}
	if got := a.currentMembers(4); got != 1 {
		t.Errorf("反复失败应降到底 1，实际 %d", got)
	}
}

// 连接码本来就是单连接时不该做任何调整，也不该累计连击。
func TestMembersAdaptIgnoresSingle(t *testing.T) {
	a := newAdaptTestApp()
	for i := 0; i < 5; i++ {
		a.noteProbeResult(false, 1)
	}
	if got := a.currentMembers(1); got != 1 {
		t.Errorf("target=1 时应恒为 1，实际 %d", got)
	}
	if s := a.probeStreak.Load(); s != 0 {
		t.Errorf("target=1 时不该累计探测连击，实际 %d", s)
	}
}

// 降档后处于冷却期：连续成功也不升，避免「降一档连上、加一档又失败」来回震。
func TestMembersAdaptCooldownBlocksRaise(t *testing.T) {
	a := newAdaptTestApp()
	a.noteProbeResult(false, 4)
	a.noteProbeResult(false, 4) // 降到 3，同时进入冷却
	if got := a.currentMembers(4); got != 3 {
		t.Fatalf("应已降到 3，实际 %d", got)
	}
	for i := 0; i < 5; i++ {
		a.noteProbeResult(true, 4)
	}
	if got := a.currentMembers(4); got != 3 {
		t.Errorf("冷却期内不该升档，实际 %d", got)
	}
}

// 冷却期结束、且连续探测成功三次，才升回一档。
func TestMembersAdaptRaisesAfterCooldown(t *testing.T) {
	a := newAdaptTestApp()
	a.noteProbeResult(false, 4)
	a.noteProbeResult(false, 4) // 降到 3
	a.lastDowngrade.Store(time.Now().Add(-2 * memberRampCooldown).UnixNano())

	a.noteProbeResult(true, 4) // 连击 1
	a.noteProbeResult(true, 4) // 连击 2
	if got := a.currentMembers(4); got != 3 {
		t.Errorf("两次成功还不够，实际 %d", got)
	}
	a.noteProbeResult(true, 4) // 连击 3 → 升档
	if got := a.currentMembers(4); got != 4 {
		t.Errorf("连续三次成功后应升到 4，实际 %d", got)
	}
}

// 换连接码且新目标更小时，不能请求超过新码允许的连接数。
func TestMembersAdaptClampsToTarget(t *testing.T) {
	a := newAdaptTestApp()
	a.preferredMembers.Store(4)
	if got := a.currentMembers(2); got != 2 {
		t.Errorf("自适应值 4 超过新目标 2，应夹到 2，实际 %d", got)
	}
	a.preferredMembers.Store(1)
	if got := a.currentMembers(4); got != 1 {
		t.Errorf("自适应值 1 低于目标 4 时应保留 1，实际 %d", got)
	}
}
