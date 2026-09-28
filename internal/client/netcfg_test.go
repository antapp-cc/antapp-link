package client

import (
	"os"
	"path/filepath"
	"testing"
)

func testSnapshot() Snapshot {
	return Snapshot{
		DefaultGateway: "192.168.1.1",
		DefaultIfIndex: 12,
		ServerNextHop:  "192.168.1.1",
		Interfaces: []IfaceDNS{
			{Name: "以太网", Index: 12, DNS: []string{"192.168.1.1"}},
			{Name: "WLAN", Index: 7, DNS: []string{"192.168.1.1", "223.5.5.5"}},
		},
		CapturedAt: "2026-09-27T20:00:00+08:00",
	}
}

func testNetConfig() NetConfig {
	return NetConfig{
		AdapterName: AdapterName,
		ServerIP:    "103.143.11.34",
		TunnelIP:    "10.10.0.2",
		Gateway:     "10.10.0.1",
		Prefix:      24,
		MTU:         1400,
		DNS:         []string{"8.8.8.8", "149.112.112.112"},
	}
}

func TestBuildNetConfigSplitsServerAddress(t *testing.T) {
	inv := testInvite(t)
	cfg := BuildNetConfig(inv)

	if cfg.ServerIP != "103.143.11.34" {
		t.Errorf("ServerIP = %q，应该从 host:port 里拆出来", cfg.ServerIP)
	}
	if cfg.TunnelIP != "10.10.0.2" || cfg.Gateway != "10.10.0.1" || cfg.MTU != 1400 || cfg.Prefix != 24 {
		t.Errorf("隧道参数没从连接码带过来: %+v", cfg)
	}
	if len(cfg.DNS) != 1 || cfg.DNS[0] != "8.8.8.8" {
		t.Errorf("DNS = %v", cfg.DNS)
	}
	if cfg.AdapterName != AdapterName {
		t.Errorf("AdapterName = %q, want %q", cfg.AdapterName, AdapterName)
	}
	if got := ServerIPOf(inv); got != "103.143.11.34" {
		t.Errorf("ServerIPOf = %q", got)
	}
}

func TestSnapshotPersistence(t *testing.T) {
	dir := t.TempDir()
	path := StatePath(dir)
	if want := filepath.Join(dir, "data", "state.json"); path != want {
		t.Errorf("StatePath = %q, want %q", path, want)
	}

	if _, exists, err := LoadSnapshot(path); err != nil || exists {
		t.Fatalf("没写过时应该是 (不存在, 无错)，实际 exists=%v err=%v", exists, err)
	}

	snap := testSnapshot()
	if err := SaveSnapshot(path, snap); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	got, exists, err := LoadSnapshot(path)
	if err != nil || !exists {
		t.Fatalf("读回失败: exists=%v err=%v", exists, err)
	}
	if got.DefaultGateway != snap.DefaultGateway || len(got.Interfaces) != len(snap.Interfaces) {
		t.Errorf("往返后内容不一致: %+v", got)
	}
	if got.ServerNextHop != "192.168.1.1" {
		t.Errorf("ServerNextHop 没保住: %q", got.ServerNextHop)
	}
	if got.Interfaces[1].DNS[1] != "223.5.5.5" {
		t.Errorf("多值 DNS 没保住: %v", got.Interfaces[1].DNS)
	}

	if err := RemoveSnapshot(path); err != nil {
		t.Fatalf("RemoveSnapshot: %v", err)
	}
	if _, exists, _ := LoadSnapshot(path); exists {
		t.Error("删掉之后不该还存在")
	}
	if err := RemoveSnapshot(path); err != nil {
		t.Errorf("重复删除应当幂等: %v", err)
	}
}

// Review Focus #1：文件坏了也必须告诉调用方「曾经接管过」。否则会跳过自愈，
// 留着坏路由和坏 DNS 让用户断网，而且重启程序也救不回来。
func TestLoadSnapshotTreatsCorruptFileAsExisting(t *testing.T) {
	path := StatePath(t.TempDir())
	// 走 SaveSnapshot 建目录是不行的（它会把快照覆盖掉），手动补 data\
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{ broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := LoadSnapshot(path); !exists || err == nil {
		t.Errorf("损坏的快照应返回 exists=true 且带错误，实际 exists=%v err=%v", exists, err)
	}
}
