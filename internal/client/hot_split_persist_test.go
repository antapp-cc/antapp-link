package client

import (
	"slices"
	"testing"
)

// 热重铺结束后账本必须落到 state.json 里。
//
// 只更新进程内账本的话，程序被强杀后重启，自愈读到的还是接管当时那版账本，
// 热重铺新加的路由就永远清不掉了。
func TestHotApplyPersistsSplitLedgerToStateFile(t *testing.T) {
	oldHook := refreshSplitRoutesHook
	t.Cleanup(func() {
		refreshSplitRoutesHook = oldHook
		clearLiveSplitOwnership()
	})

	a := NewApp(testInvite(t), t.TempDir(), nil)
	snap := testSnapshot()
	a.mu.Lock()
	a.running = true
	a.snapshot = snap
	a.mu.Unlock()
	if err := SaveSnapshot(a.statePath, snap); err != nil {
		t.Fatalf("先落一份接管时的快照: %v", err)
	}

	var gotGw string
	gotIfIndex := -1
	refreshSplitRoutesHook = func(gw string, ifIndex int, _ []string) {
		gotGw, gotIfIndex = gw, ifIndex
		setLiveSplitOwnership(&splitOwnership{
			IfIndex:  ifIndex,
			Gateway:  gw,
			Prefixes: []string{"1.0.2.0/23"},
		})
	}

	a.hotApplyRouteTable()

	if gotGw != "192.168.1.1" || gotIfIndex != 12 {
		t.Fatalf("热重铺用错出口: %s / %d", gotGw, gotIfIndex)
	}
	got, exists, err := LoadSnapshot(a.statePath)
	if err != nil || !exists {
		t.Fatalf("读回状态文件: exists=%v err=%v", exists, err)
	}
	if got.Split == nil || !slices.Contains(got.Split.Prefixes, "1.0.2.0/23") {
		t.Fatalf("热重铺后的账本没有落盘: %+v", got.Split)
	}
	// 只更新 Split，接管现场其余字段必须原样保留。
	if got.DefaultGateway != "192.168.1.1" || got.DefaultIfIndex != 12 ||
		len(got.Interfaces) != len(snap.Interfaces) {
		t.Errorf("落盘时把接管现场改坏了: %+v", got)
	}
}

// 热重铺没真的改账本（比如网络已还原、hook 提前返回）时，不能拿一个空账本
// 覆盖磁盘上的旧账本 —— 那等于把「还有哪些路由要清」这件事抹掉。
func TestHotApplyKeepsLedgerWhenNothingChanged(t *testing.T) {
	oldHook := refreshSplitRoutesHook
	t.Cleanup(func() {
		refreshSplitRoutesHook = oldHook
		clearLiveSplitOwnership()
	})

	a := NewApp(testInvite(t), t.TempDir(), nil)
	snap := testSnapshot()
	old := &splitOwnership{IfIndex: 12, Gateway: "192.168.1.1", Prefixes: []string{"1.0.1.0/24"}}
	snap.Split = old
	a.mu.Lock()
	a.running = true
	a.snapshot = snap
	a.mu.Unlock()
	setLiveSplitOwnership(old)
	if err := SaveSnapshot(a.statePath, snap); err != nil {
		t.Fatalf("先落一份带账本的快照: %v", err)
	}

	refreshSplitRoutesHook = func(string, int, []string) {}

	a.hotApplyRouteTable()

	got, _, err := LoadSnapshot(a.statePath)
	if err != nil {
		t.Fatalf("读回状态文件: %v", err)
	}
	if got.Split == nil || got.Split.Prefixes[0] != "1.0.1.0/24" {
		t.Errorf("没发生热重铺时不能改写磁盘账本: %+v", got.Split)
	}
}
