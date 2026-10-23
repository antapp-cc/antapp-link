//go:build windows

package client

import (
	"errors"
	"net/netip"
	"testing"

	"golang.org/x/sys/windows"
)

// 「本来就在」且没有任何账本证据的路由不能算成自己装的。
//
// 形状（本接口 + 原网关 + metric 5）与用户手工建的路由、其它 VPN 客户端建的路由
// 完全一样；把它记进账本，断开时就会把用户的配置删掉。
func TestInstallSplitRoutesDoesNotClaimForeignRoute(t *testing.T) {
	ref := adapterKey{GUID: "{eth}", Index: 12, Name: "以太网"}
	gw := netip.MustParseAddr("192.168.1.1")

	installed, failed := installSplitRoutes([]string{"1.0.1.0/24"}, func(netip.Prefix) error {
		return windows.ERROR_OBJECT_ALREADY_EXISTS
	}, ref, gw, nil)

	if failed != 0 {
		t.Errorf("「本来就在」不是写入失败，failed = %d", failed)
	}
	if len(installed) != 0 {
		t.Errorf("没有账本证据就不能认领，实际记下 %v", installed)
	}
}

// 账本证明是自己早先装的（上次删除失败、这次重铺遇到「已存在」）时仍要认领，
// 否则那批路由会从账本上消失。
func TestInstallSplitRoutesClaimsLedgerBackedRoute(t *testing.T) {
	ref := adapterKey{GUID: "{eth}", Index: 12, Name: "以太网"}
	gw := netip.MustParseAddr("192.168.1.1")
	previous := []*splitOwnership{{
		IfGUID:   "{eth}",
		IfIndex:  12,
		Gateway:  "192.168.1.1",
		Prefixes: []string{"1.0.1.0/24"},
	}}

	installed, failed := installSplitRoutes(
		[]string{"1.0.1.0/24", "1.0.2.0/23"},
		func(p netip.Prefix) error {
			if p.String() == "1.0.1.0/24" {
				return windows.ERROR_OBJECT_ALREADY_EXISTS
			}
			return nil
		}, ref, gw, previous)

	if failed != 0 {
		t.Errorf("failed = %d，期望 0", failed)
	}
	if len(installed) != 2 {
		t.Errorf("新建成功与账本已记录的都应计入，实际 %v", installed)
	}
}

// 真正的写入失败（不是「本来就在」）仍要计入失败数，不能静默吞掉。
func TestInstallSplitRoutesCountsWriteFailures(t *testing.T) {
	ref := adapterKey{GUID: "{eth}", Index: 12, Name: "以太网"}
	gw := netip.MustParseAddr("192.168.1.1")
	denied := errors.New("拒绝访问")

	installed, failed := installSplitRoutes([]string{"1.0.1.0/24", "不是前缀"},
		func(netip.Prefix) error { return denied }, ref, gw, nil)

	if failed != 2 {
		t.Errorf("解析失败与写入失败各算一条，failed = %d，期望 2", failed)
	}
	if len(installed) != 0 {
		t.Errorf("失败的路由不能进账本，实际 %v", installed)
	}
}
