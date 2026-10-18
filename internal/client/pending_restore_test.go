package client

import (
	"strings"
	"testing"
)

// 有待还原的网络时，Connect 必须拒绝。
//
// 上次断开没还原干净时，网络处于半接管状态（DNS 指着隧道网关、/1 路由还在）。
// 这时去抓「原值」抓到的是我们自己的值，存进快照就把磁盘上正确的恢复信息覆盖掉，
// 之后自愈再也还原不回去。第三轮复审 C2 用入口测试复现过。
func TestConnectRefusedWhilePendingRestore(t *testing.T) {
	a := NewApp(testInvite(t), t.TempDir(), nil)
	a.pendingRestore = &pendingRestoreState{snap: testSnapshot(), cfg: testNetConfig()}

	err := a.Connect()
	if err == nil {
		t.Fatal("有待还原状态时 Connect 必须拒绝，不能进入新的网络接管")
	}
	if !strings.Contains(err.Error(), "还没有还原干净") {
		t.Errorf("错误信息应提示待还原状态，实际: %v", err)
	}
	// 被拒绝时不能留下 connecting 标志，否则后续 Connect/Disconnect 全部卡住
	if a.connecting {
		t.Error("拒绝后不该把 connecting 置位")
	}
}

// 没有待还原状态时，Connect 不该被这条检查挡住（防误拦）。
func TestConnectNotBlockedWithoutPendingRestore(t *testing.T) {
	a := NewApp(testInvite(t), t.TempDir(), nil)
	// 不设 pendingRestore；Connect 会因为别的原因失败（没有真实网卡/服务端），
	// 但错误不该是「待还原」那条。
	err := a.Connect()
	if err != nil && strings.Contains(err.Error(), "还没有还原干净") {
		t.Errorf("没有待还原状态时不该报这条错: %v", err)
	}
}
