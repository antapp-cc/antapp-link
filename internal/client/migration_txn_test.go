//go:build windows

package client

import (
	"errors"
	"net/netip"
	"testing"

	"golang.org/x/sys/windows"
	"golang.zx2c4.com/wireguard/windows/tunnel/winipcfg"
)

// 迁移测试的旧现场：同一张网卡（WLAN）换了网关，旧账本记的是旧网关。
// 换网关是迁移里最难的一种 —— 旧分流路由和新分流路由落在同一张网卡上。
func testOldExitSnapshot() Snapshot {
	return Snapshot{
		DefaultGateway: "192.0.2.1",
		DefaultIfIndex: 42,
		Interfaces: []IfaceDNS{
			{Name: "WLAN", Index: 42, GUID: "{new}", DNS: []string{"192.0.2.1"}},
		},
		Split: &splitOwnership{
			IfGUID:   "{new}",
			IfIndex:  42,
			Gateway:  "192.0.2.1",
			Prefixes: []string{"1.0.1.0/24"},
		},
	}
}

// newMigrationTestWatcher 造一个「出口已换到 198.51.100.1 / WLAN」的对账现场，
// 并把迁移路径上的系统调用全部换成桩：真机上没法安全地伪造换网关和用户路由。
func newMigrationTestWatcher(t *testing.T, old Snapshot) (*sessionWatcher, *int, *Snapshot) {
	t.Helper()

	oldBest, oldAdapter, oldHop := bestDefaultRouteFn, adapterByIndexFn, bestRouteToFn
	oldDNS, oldAdd, oldSweep := setAdapterDNSFn, addSplitRouteFn, sweepOwnedSplitFn
	t.Cleanup(func() {
		bestDefaultRouteFn, adapterByIndexFn, bestRouteToFn = oldBest, oldAdapter, oldHop
		setAdapterDNSFn, addSplitRouteFn, sweepOwnedSplitFn = oldDNS, oldAdd, oldSweep
		clearLiveSplitOwnership()
	})
	clearLiveSplitOwnership()

	newGw := netip.MustParseAddr("198.51.100.1")
	bestDefaultRouteFn = func(winipcfg.LUID) (netip.Addr, uint32, error) {
		return newGw, 42, nil
	}
	adapterByIndexFn = func(uint32) (adapterInfo, bool, error) {
		return adapterInfo{Name: "WLAN", Index: 42, GUID: "{new}"}, true, nil
	}
	// 云服在直连网段：不加绕行路由，少一次系统调用，也让关键配置全部写好。
	bestRouteToFn = func(netip.Addr, winipcfg.LUID) (netip.Addr, uint32, error) {
		return netip.Addr{}, 0, nil
	}
	setAdapterDNSFn = func(winipcfg.LUID, []string) error { return nil }
	addSplitRouteFn = func(winipcfg.LUID, netip.Prefix, netip.Addr) error { return nil }
	sweepOwnedSplitFn = func(*splitOwnership) (int, error) { return 1, nil }

	updates := 0
	var last Snapshot
	w := &sessionWatcher{
		cfg:     testNetConfig(),
		srv:     netip.MustParseAddr("103.143.11.34"),
		snap:    old,
		lastGw:  netip.MustParseAddr(old.DefaultGateway),
		lastIdx: uint32(old.DefaultIfIndex),
		update: func(s Snapshot) error {
			updates++
			last = s
			return nil
		},
	}
	return w, &updates, &last
}

// 旧网卡清理失败不能提交只含新出口的快照：旧快照里的 DNS 原值和路由账本
// 是那张网卡唯一的恢复依据，提交后就再没记录可查了。
func TestMigrationKeepsOldLedgerWhenCleanupFails(t *testing.T) {
	old := testOldExitSnapshot()
	w, updates, last := newMigrationTestWatcher(t, old)
	sweeps := 0
	sweepOwnedSplitFn = func(*splitOwnership) (int, error) {
		sweeps++
		return 0, errors.New("注入：旧分流删不掉")
	}

	w.reconcile()

	if *updates != 0 {
		t.Fatal("旧出口没清干净就提交了新现场")
	}
	if sweeps != 1 {
		t.Fatalf("迁移应当尝试清理一次旧账本，实际 %d 次", sweeps)
	}
	p := w.pending
	if p == nil || p.old == nil {
		t.Fatal("旧出口的待办和账本必须留在 pending 里")
	}
	if p.snap.DefaultGateway != "198.51.100.1" {
		t.Errorf("待提交的新现场不对: %s", p.snap.DefaultGateway)
	}
	if w.snap.DefaultGateway != "192.0.2.1" {
		t.Errorf("没提交成功时当前生效现场不该变，实际 %s", w.snap.DefaultGateway)
	}

	// 下一轮事件出口没再变，补完清理后才提交新现场。
	sweepOwnedSplitFn = func(*splitOwnership) (int, error) {
		sweeps++
		return 1, nil
	}
	w.reconcile()

	if sweeps != 2 {
		t.Fatalf("重试必须重新清理旧出口，实际 %d 次", sweeps)
	}
	if w.pending != nil {
		t.Fatal("旧出口清理完成后 pending 应当结束")
	}
	if *updates != 1 {
		t.Fatalf("清理完成后应当提交一次新现场，实际 %d 次", *updates)
	}
	if last.DefaultGateway != "198.51.100.1" || last.DefaultIfIndex != 42 {
		t.Errorf("提交的现场不对: %s / %d", last.DefaultGateway, last.DefaultIfIndex)
	}
	if w.snap.DefaultGateway != "198.51.100.1" {
		t.Errorf("提交后当前现场应当换到新出口，实际 %s", w.snap.DefaultGateway)
	}
}

// 撤销 pending 时，旧出口的清理也要补做 —— 否则旧账本随 pending 一起丢掉。
func TestRevertPendingCleansOldAndNewLedgers(t *testing.T) {
	old := testOldExitSnapshot()
	w, _, _ := newMigrationTestWatcher(t, old)

	cleaned := 0
	sweepOwnedSplitFn = func(*splitOwnership) (int, error) {
		cleaned++
		return 1, nil
	}
	w.pending = &pendingMigration{
		snap: Snapshot{
			DefaultGateway: "198.51.100.1",
			DefaultIfIndex: 99,
			Interfaces:     []IfaceDNS{{Name: "没这张网卡", Index: 99, GUID: "{gone}"}},
			Split:          &splitOwnership{IfGUID: "{gone}", IfIndex: 99, Gateway: "198.51.100.1", Prefixes: []string{"1.0.2.0/23"}},
		},
		old: &old,
	}

	if err := w.revertPending(); err != nil {
		t.Fatalf("revertPending: %v", err)
	}
	if w.pending != nil {
		t.Fatal("全部撤干净后 pending 应当结束")
	}
	if cleaned != 2 {
		t.Fatalf("旧账本和新账本都要清，实际清理 %d 次", cleaned)
	}
}

// 迁移也要走 installSplitRoutes：新出口上本来就有同前缀的路由，没有账本证据
// 就不能认领，否则断开时会把用户手工建的配置删掉。
func TestMigrationDoesNotClaimForeignSplitRoutes(t *testing.T) {
	old := testOldExitSnapshot()
	w, updates, _ := newMigrationTestWatcher(t, old)
	addSplitRouteFn = func(winipcfg.LUID, netip.Prefix, netip.Addr) error {
		return windows.ERROR_OBJECT_ALREADY_EXISTS
	}

	w.reconcile()

	if *updates != 1 {
		t.Fatalf("迁移应当正常提交，实际提交 %d 次", *updates)
	}
	if w.snap.Split == nil {
		t.Fatal("新现场必须带分流账本")
	}
	if len(w.snap.Split.Prefixes) != 0 {
		t.Fatalf("把用户已有的路由记进了账本: %v", w.snap.Split.Prefixes)
	}
}

// 真正新建成功的分流路由仍要进账本，别把修复做成「一条都不记」。
func TestMigrationClaimsNewlyInstalledSplitRoutes(t *testing.T) {
	old := testOldExitSnapshot()
	w, updates, _ := newMigrationTestWatcher(t, old)

	w.reconcile()

	if *updates != 1 {
		t.Fatalf("迁移应当正常提交，实际提交 %d 次", *updates)
	}
	want := len(ActiveCNRoutes())
	if w.snap.Split == nil || len(w.snap.Split.Prefixes) != want {
		t.Fatalf("新建成功的分流应当全部入账：want %d，got %+v", want, w.snap.Split)
	}
}
