package client

import (
	"errors"
	"testing"
)

func testSplitLedger() *splitOwnership {
	return &splitOwnership{
		IfIndex:  12,
		Gateway:  "192.168.1.1",
		Prefixes: []string{"1.0.1.0/24", "1.0.2.0/23"},
	}
}

// Apply 之后必须把带 Split 的快照补写进磁盘。
//
// Apply 之前那份快照还不知道实际铺了哪些分流；普通连接（没发生过出口迁移）
// 崩溃后，磁盘上的 Split 是空的，自愈只能退回「按网关 + metric」的形态扫描，
// 可能碰掉形态相同的用户路由。审阅报告第 5 项。
func TestTakeOverPersistsSplitAfterApply(t *testing.T) {
	a := NewApp(testInvite(t), t.TempDir(), nil)
	snap := testSnapshot()

	apply := func(s *Snapshot, _ NetConfig) error {
		s.Split = testSplitLedger()
		return nil
	}
	restore := func(Snapshot, NetConfig) error {
		t.Error("Apply 成功时不该还原")
		return nil
	}

	if err := a.takeOver(&snap, testNetConfig(), a.statePath, apply, SaveSnapshot, restore); err != nil {
		t.Fatalf("takeOver: %v", err)
	}

	got, exists, err := LoadSnapshot(a.statePath)
	if err != nil || !exists {
		t.Fatalf("状态文件没写成: exists=%v err=%v", exists, err)
	}
	if got.Split == nil || len(got.Split.Prefixes) != 2 {
		t.Fatalf("Apply 填好的分流账本没有落盘: %+v", got.Split)
	}
}

// 系统已经改完、但账本写盘失败：不能当成接管成功继续跑，要把网络还回去。
func TestTakeOverRestoresWhenLedgerWriteFails(t *testing.T) {
	a := NewApp(testInvite(t), t.TempDir(), nil)
	snap := testSnapshot()
	// 模拟 connectSlow 在 Apply 之前已经写过一份快照
	if err := SaveSnapshot(a.statePath, snap); err != nil {
		t.Fatalf("准备状态文件: %v", err)
	}

	apply := func(s *Snapshot, _ NetConfig) error {
		s.Split = testSplitLedger()
		return nil
	}
	save := func(string, Snapshot) error { return errors.New("注入：磁盘写满") }
	restored := false
	restore := func(Snapshot, NetConfig) error {
		restored = true
		return nil
	}

	err := a.takeOver(&snap, testNetConfig(), a.statePath, apply, save, restore)
	if err == nil {
		t.Fatal("账本写盘失败不能当成接管成功")
	}
	if !restored {
		t.Fatal("系统已经改完却无法落盘账本，必须把网络还原")
	}
	if a.pendingRestore != nil {
		t.Error("还原成功后不该留待还原状态")
	}
	if _, exists, _ := LoadSnapshot(a.statePath); exists {
		t.Error("还原干净后状态文件必须删掉")
	}
}

// 账本写盘失败、还原也失败：Apply 后那份完整现场（含 Split）必须留给
// 下一次断开重试，状态文件也不能清 —— 否则这张网卡上的配置没人负责。
func TestTakeOverKeepsRetryStateWhenRollbackAlsoFails(t *testing.T) {
	a := NewApp(testInvite(t), t.TempDir(), nil)
	snap := testSnapshot()
	if err := SaveSnapshot(a.statePath, snap); err != nil {
		t.Fatalf("准备状态文件: %v", err)
	}

	apply := func(s *Snapshot, _ NetConfig) error {
		s.Split = testSplitLedger()
		return nil
	}
	save := func(string, Snapshot) error { return errors.New("注入：磁盘写满") }
	restore := func(Snapshot, NetConfig) error { return errors.New("注入：还原失败") }

	err := a.takeOver(&snap, testNetConfig(), a.statePath, apply, save, restore)
	if err == nil {
		t.Fatal("写盘失败且还原失败时必须报错")
	}

	p := a.pendingRestore
	if p == nil {
		t.Fatal("还欠一次还原的现场不能丢，否则网络状态没人负责")
	}
	if p.snap.Split == nil {
		t.Error("待还原现场要带 Apply 后填好的分流账本，重试才能精确清理")
	}
	if _, exists, _ := LoadSnapshot(a.statePath); !exists {
		t.Fatal("还欠还原时状态文件必须保留")
	}
}
