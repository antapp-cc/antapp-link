//go:build windows

package client

import (
	"errors"
	"net/netip"
	"testing"

	"golang.zx2c4.com/wireguard/windows/tunnel/winipcfg"
)

// 上一笔迁移没撤干净时，对账不能继续往新出口迁移。
//
// 迁移残留会摊在两张网卡上，而现场只有一份，之后谁也说不清该还原哪一张。
// 审阅报告第 2 项：reconcile 忽略 revertPending 的错误，会直接开始下一次迁移。
func TestReconcileStopsWhenPendingRevertFails(t *testing.T) {
	oldBest, oldAdapter := bestDefaultRouteFn, adapterByIndexFn
	oldRevert := revertPendingFn
	t.Cleanup(func() {
		bestDefaultRouteFn, adapterByIndexFn = oldBest, oldAdapter
		revertPendingFn = oldRevert
	})

	fakeGw := netip.MustParseAddr("192.168.9.1")
	bestDefaultRouteFn = func(winipcfg.LUID) (netip.Addr, uint32, error) {
		return fakeGw, 42, nil
	}
	adapterCalls := 0
	adapterByIndexFn = func(uint32) (adapterInfo, bool, error) {
		adapterCalls++
		return adapterInfo{}, false, errors.New("不该开始新迁移")
	}
	reverts := 0
	revertPendingFn = func(*sessionWatcher) (Snapshot, error) {
		reverts++
		return Snapshot{}, errors.New("注入：旧网卡 DNS 回滚失败")
	}

	// snap 不记网卡：万一实现继续往下走，也只会做一次无副作用的空还原。
	w := &sessionWatcher{
		cfg:     testNetConfig(),
		srv:     netip.MustParseAddr("103.143.11.34"),
		snap:    Snapshot{DefaultGateway: "192.168.1.1", DefaultIfIndex: 12},
		lastGw:  netip.MustParseAddr("192.168.1.1"),
		lastIdx: 12,
		pending: &pendingMigration{snap: Snapshot{DefaultGateway: "172.16.9.1", DefaultIfIndex: 7}},
	}

	w.reconcile()

	if reverts != 1 {
		t.Fatalf("出口已换、pending 对不上时必须先回滚，实际调用 %d 次", reverts)
	}
	if adapterCalls != 0 {
		t.Fatal("回滚没成功就开始了新迁移：残留会留在两张网卡上")
	}
	if w.pending == nil {
		t.Fatal("回滚失败必须保留 pending，下次事件或断开时还要重试")
	}
	if w.lastIdx != 12 || w.lastGw.String() != "192.168.1.1" {
		t.Errorf("失败时不能把出口记为已迁移: gateway=%s ifIndex=%d", w.lastGw, w.lastIdx)
	}
}
