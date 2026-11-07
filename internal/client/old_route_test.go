//go:build windows

package client

import (
	"errors"
	"net/netip"
	"testing"

	"golang.org/x/sys/windows"
	"golang.zx2c4.com/wireguard/windows/tunnel/winipcfg"
)

func TestRestoreOldServerRoute(t *testing.T) {
	old := testOldExitSnapshot()
	old.ServerNextHop = "192.0.2.254"
	old.ServerIP = "203.0.113.1"
	srv := netip.MustParseAddr("203.0.113.1")

	restoreAdapter, restoreAdd := defaultAdapterFn, addServerRouteFn
	t.Cleanup(func() { defaultAdapterFn, addServerRouteFn = restoreAdapter, restoreAdd })
	defaultAdapterFn = func(Snapshot) (adapterInfo, bool) { return adapterInfo{}, true }

	var got []netip.Prefix
	addServerRouteFn = func(_ winipcfg.LUID, dest netip.Prefix, gw netip.Addr) error {
		if gw.String() != "192.0.2.254" {
			t.Fatalf("下一跳 = %s", gw)
		}
		got = append(got, dest)
		return nil
	}
	if err := restoreOldServerRoute(old, srv); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].String() != "203.0.113.1/32" {
		t.Fatalf("恢复的路由 = %v", got)
	}

	// 已经存在同一条路由说明旧绕行本来就在，按成功处理。
	addServerRouteFn = func(winipcfg.LUID, netip.Prefix, netip.Addr) error {
		return windows.ERROR_OBJECT_ALREADY_EXISTS
	}
	if err := restoreOldServerRoute(old, srv); err != nil {
		t.Fatalf("已存在被当成失败: %v", err)
	}

	// 其它错误必须如实返回，不能像旧实现那样吞掉。
	addServerRouteFn = func(winipcfg.LUID, netip.Prefix, netip.Addr) error { return errors.New("boom") }
	if err := restoreOldServerRoute(old, srv); err == nil {
		t.Fatal("路由失败被吞掉")
	}

	// 快照里没有下一跳时不做任何尝试，但要报错说明原因。
	if err := restoreOldServerRoute(testOldExitSnapshot(), srv); err == nil {
		t.Fatal("缺少下一跳应当报错")
	}
}
