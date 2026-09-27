package client

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

func dumpCommands(cmds []Command) string {
	var b strings.Builder
	for i, c := range cmds {
		fmt.Fprintf(&b, "%2d. %s\n", i+1, c.String())
	}
	return b.String()
}

func indexOfCommand(cmds []Command, substr string) int {
	for i, c := range cmds {
		if strings.Contains(c.String(), substr) {
			return i
		}
	}
	return -1
}

func TestMaskFromPrefix(t *testing.T) {
	cases := map[int]string{
		8:  "255.0.0.0",
		16: "255.255.0.0",
		24: "255.255.255.0",
		32: "255.255.255.255",
	}
	for prefix, want := range cases {
		if got := MaskFromPrefix(prefix); got != want {
			t.Errorf("MaskFromPrefix(%d) = %s, want %s", prefix, got, want)
		}
	}
	if got := MaskFromPrefix(99); got != "255.255.255.0" {
		t.Errorf("越界的 prefix 该退回默认掩码而不是崩，实际 %s", got)
	}
}

// Review Focus #2：绕行路由必须排在默认路由之前。顺序反了，承载隧道的 TCP 连接
// 自己会被送进隧道形成自噬，表现是「连不上，且没有任何报错」。
func TestApplyPutsBypassRouteBeforeDefaultRoute(t *testing.T) {
	cmds := ApplyCommands(testSnapshot(), testNetConfig())
	bypass := indexOfCommand(cmds, "route add 103.143.11.34 mask 255.255.255.255 192.168.1.1")
	def := indexOfCommand(cmds, "route add 0.0.0.0 mask 0.0.0.0 10.10.0.1")

	if bypass < 0 {
		t.Fatalf("缺少云服 IP 的绕行路由:\n%s", dumpCommands(cmds))
	}
	if def < 0 {
		t.Fatalf("缺少指向隧道的默认路由:\n%s", dumpCommands(cmds))
	}
	if bypass > def {
		t.Errorf("绕行路由在第 %d 条、默认路由在第 %d 条 —— 绕行必须在前，否则隧道自噬",
			bypass+1, def+1)
	}
}

// 云服就在直连网段里时（本地拿 WSL 当服务端验证），既有的直连路由已经比默认路由更具体。
// 这时再加一条指向默认网关的 /32 会把它覆盖掉 —— 隧道自己就把自己掐死了。
func TestApplySkipsBypassWhenServerIsOnLink(t *testing.T) {
	snap := testSnapshot()
	snap.ServerNextHop = ""

	s := dumpCommands(ApplyCommands(snap, testNetConfig()))
	if strings.Contains(s, "route add 103.143.11.34") {
		t.Errorf("云服是直连时不该加绕行路由:\n%s", s)
	}
	if !strings.Contains(s, "route add 0.0.0.0 mask 0.0.0.0 10.10.0.1") {
		t.Errorf("默认路由仍要配上:\n%s", s)
	}
}

func TestRestoreRemovesDefaultBeforeBypass(t *testing.T) {
	cmds := RestoreCommands(testSnapshot(), testNetConfig())
	def := indexOfCommand(cmds, "route delete 0.0.0.0 mask 0.0.0.0 10.10.0.1")
	bypass := indexOfCommand(cmds, "route delete 103.143.11.34 mask 255.255.255.255")

	if def < 0 {
		t.Fatalf("缺少删除默认路由的命令:\n%s", dumpCommands(cmds))
	}
	if bypass < 0 {
		t.Fatalf("缺少删除绕行路由的命令:\n%s", dumpCommands(cmds))
	}
	if def > bypass {
		t.Errorf("删默认路由在第 %d 条、删绕行在第 %d 条 —— 必须与 Apply 严格逆序，"+
			"否则中间会留下「流量被送进一条已不通的隧道」的窗口", def+1, bypass+1)
	}
}

func TestApplyConfiguresAdapterAndRewritesEveryDNS(t *testing.T) {
	cfg := testNetConfig()
	s := dumpCommands(ApplyCommands(testSnapshot(), cfg))

	if !strings.Contains(s, "10.10.0.2") || !strings.Contains(s, "255.255.255.0") {
		t.Errorf("没给隧道网卡配地址:\n%s", s)
	}
	if !strings.Contains(s, "mtu=1400") {
		t.Errorf("没设 MTU，大包会被黑洞掉:\n%s", s)
	}
	if !strings.Contains(s, "metric=1") {
		t.Errorf("没把隧道网卡的跃点数压到最低，DNS 可能仍走本地:\n%s", s)
	}
	for _, iface := range []string{"以太网", "WLAN"} {
		if !strings.Contains(s, iface) {
			t.Errorf("没有为网卡 %s 改写 DNS:\n%s", iface, s)
		}
	}
	if !strings.Contains(s, "8.8.8.8") || !strings.Contains(s, "149.112.112.112") {
		t.Errorf("下发的 DNS 没写进去:\n%s", s)
	}
	if !strings.Contains(s, "/flushdns") {
		t.Errorf("改完 DNS 要刷缓存，否则旧解析还留着:\n%s", s)
	}
}

func TestRestorePutsOriginalDNSBack(t *testing.T) {
	s := dumpCommands(RestoreCommands(testSnapshot(), testNetConfig()))
	if !strings.Contains(s, "223.5.5.5") {
		t.Errorf("WLAN 原来的第二个 DNS 没还回去:\n%s", s)
	}
	if !strings.Contains(s, "192.168.1.1") {
		t.Errorf("原 DNS 没还回去:\n%s", s)
	}

	// 原来压根没有静态 DNS 的网卡，要还回自动获取，而不是塞一个编造的地址
	empty := Snapshot{DefaultGateway: "192.168.1.1", Interfaces: []IfaceDNS{{Name: "以太网", Index: 12}}}
	if s := dumpCommands(RestoreCommands(empty, testNetConfig())); !strings.Contains(s, "source=dhcp") {
		t.Errorf("原 DNS 为空时应还回 DHCP:\n%s", s)
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
	if want := filepath.Join(dir, "state.json"); path != want {
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
	if err := os.WriteFile(path, []byte("{ broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := LoadSnapshot(path); !exists || err == nil {
		t.Errorf("损坏的快照应返回 exists=true 且带错误，实际 exists=%v err=%v", exists, err)
	}
}
