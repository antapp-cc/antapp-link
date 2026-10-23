package client

import (
	"testing"
	"time"
)

// 热重铺读完出口之后、拿到网络事务锁之前，可能刚好完成一次出口迁移。
// 此时手里的网关和网卡都过期了，再拿去铺等于把国内分流钉在已经失效的出口上。
// 审阅报告第 4 项：在读完现场后暂停，插进一次迁移，热重铺仍用旧网关/旧网卡。
func TestHotApplyDropsStaleExitAfterMigration(t *testing.T) {
	oldHook, oldSeam := refreshSplitRoutesHook, afterHotSnapshot
	oldEpoch := netEpoch.Load()
	t.Cleanup(func() {
		refreshSplitRoutesHook, afterHotSnapshot = oldHook, oldSeam
		netEpoch.Store(oldEpoch)
	})

	a := NewApp(testInvite(t), t.TempDir(), nil)
	a.mu.Lock()
	a.running = true
	a.snapshot = Snapshot{DefaultGateway: "192.168.1.1", DefaultIfIndex: 12}
	a.mu.Unlock()

	captured, proceed := make(chan struct{}), make(chan struct{})
	afterHotSnapshot = func() {
		close(captured)
		<-proceed
	}
	var gotGw string
	gotIfIndex := -1
	refreshSplitRoutesHook = func(gw string, ifIndex int, _ []string) {
		gotGw, gotIfIndex = gw, ifIndex
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		a.hotApplyRouteTable()
	}()
	select {
	case <-captured:
	case <-time.After(2 * time.Second):
		t.Fatal("热重铺没有走到「读完现场」的注入点")
	}

	// 模拟排队期间一次迁移提交了新出口：快照换了，代际也前进。
	a.mu.Lock()
	a.snapshot = Snapshot{DefaultGateway: "192.168.9.1", DefaultIfIndex: 9}
	a.mu.Unlock()
	netEpoch.Add(1)
	close(proceed)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("热重铺没有返回")
	}

	if gotIfIndex != -1 {
		t.Fatalf("迁移已提交，这一轮不该再铺路由（用了 %s / %d）", gotGw, gotIfIndex)
	}
}

// 出口没变时代际不该挡路：正常的热重铺必须照常执行。
func TestHotApplyRepavesWithCurrentExit(t *testing.T) {
	oldHook, oldSeam := refreshSplitRoutesHook, afterHotSnapshot
	t.Cleanup(func() { refreshSplitRoutesHook, afterHotSnapshot = oldHook, oldSeam })
	afterHotSnapshot = nil

	a := NewApp(testInvite(t), t.TempDir(), nil)
	a.mu.Lock()
	a.running = true
	a.snapshot = Snapshot{DefaultGateway: "192.168.1.1", DefaultIfIndex: 12}
	a.mu.Unlock()

	var gotGw string
	gotIfIndex := -1
	refreshSplitRoutesHook = func(gw string, ifIndex int, _ []string) {
		gotGw, gotIfIndex = gw, ifIndex
	}
	a.hotApplyRouteTable()

	if gotGw != "192.168.1.1" || gotIfIndex != 12 {
		t.Fatalf("热重铺用错了出口: %s / %d", gotGw, gotIfIndex)
	}
}

// 迁移提交会换掉 App 里的快照，代际必须跟着走 —— 否则排队中的热重铺
// 持锁后仍发现不了手里的出口已过期。
func TestUpdateSnapshotAdvancesEpoch(t *testing.T) {
	a := NewApp(testInvite(t), t.TempDir(), nil)
	before := netEpoch.Load()
	if err := a.updateSnapshot(testSnapshot()); err != nil {
		t.Fatalf("updateSnapshot: %v", err)
	}
	if netEpoch.Load() == before {
		t.Fatal("迁移提交后网络代际没有前进")
	}
}
