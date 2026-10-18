package client

import (
	"os"
	"path/filepath"
	"testing"
)

// 迁移后的快照必须落盘。
//
// 原来的 updateSnapshot 只改内存：程序被强杀/掉电后，启动自愈从磁盘读到的是迁移前
// 那张网卡的现场，没法恢复新网卡的原 DNS 和路由 —— 用户表现为重启后解析全超时。
// 这正是复审报告 4.1 复现的那条。
func TestUpdateSnapshotPersistsToDisk(t *testing.T) {
	dir := t.TempDir()
	a := NewApp(testInvite(t), dir, nil)

	snap := testSnapshot()
	snap.DefaultIfIndex = 9
	snap.DefaultGateway = "192.168.9.1"
	snap.Interfaces = []IfaceDNS{{Name: "WLAN", Index: 9, DNS: []string{"192.168.9.1"}}}

	if err := a.updateSnapshot(snap); err != nil {
		t.Fatalf("updateSnapshot: %v", err)
	}

	got, exists, err := LoadSnapshot(a.statePath)
	if err != nil || !exists {
		t.Fatalf("磁盘上必须有快照: exists=%v err=%v", exists, err)
	}
	if got.DefaultIfIndex != 9 {
		t.Errorf("磁盘快照记录的还是旧接口 %d，期望 9", got.DefaultIfIndex)
	}
	if len(got.Interfaces) != 1 || got.Interfaces[0].Name != "WLAN" {
		t.Errorf("磁盘快照的网卡现场不对: %+v", got.Interfaces)
	}
}

// 落盘失败必须报错：watcher 靠这个错误决定「不提交本次迁移」，否则下一次出口变化
// 永远不会重试，磁盘上永远留着旧现场。
func TestUpdateSnapshotReportsWriteFailure(t *testing.T) {
	a := NewApp(testInvite(t), t.TempDir(), nil)
	// 把 statePath 指到一个不可能创建成功的路径（父级是文件而不是目录）
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("准备阻碍文件: %v", err)
	}
	a.statePath = filepath.Join(blocker, "state.json")

	if err := a.updateSnapshot(testSnapshot()); err == nil {
		t.Fatal("写盘失败时 updateSnapshot 必须返回错误，否则 watcher 会误以为迁移已提交")
	}
}
