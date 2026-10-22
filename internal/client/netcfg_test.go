package client

import (
	"net/netip"
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
		ServerIP:       "103.143.11.34",
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

// 快照必须带上隧道网关与云服地址：自愈跑在连接码加载之前（那时 App 手里是零值
// Invite），只有快照里存着这两个值。JSON 字段名写错不会报错，只会让自愈静默
// 退化成「撤不掉 /1 接管路由」—— 用户表现为重启后依然断网，所以这里钉住往返。
func TestSnapshotRoundTripKeepsTunnelInfo(t *testing.T) {
	path := StatePath(t.TempDir())
	want := newSnapshot("192.168.1.1", 12, "192.168.1.1", "103.143.11.34", nil,
		NetConfig{Gateway: "10.10.0.1", ServerIP: "103.143.11.34"})
	if err := SaveSnapshot(path, want); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	got, exists, err := LoadSnapshot(path)
	if err != nil || !exists {
		t.Fatalf("LoadSnapshot: exists=%v err=%v", exists, err)
	}
	if got.TunnelGateway != "10.10.0.1" || got.ServerIP != "103.143.11.34" {
		t.Errorf("往返后隧道信息丢失：gateway=%q server=%q", got.TunnelGateway, got.ServerIP)
	}
	if got.DefaultGateway != "192.168.1.1" || got.DefaultIfIndex != 12 {
		t.Errorf("往返后默认出口信息丢失：gw=%q idx=%d", got.DefaultGateway, got.DefaultIfIndex)
	}
}

// ---------- DNS 来源 ----------

// 还原成同样的静态地址只在同一个网络里成立，换网后可能指向不可达的服务器，
// 所以「自动获取」必须还原成自动；旧快照没有来源字段时按自动处理。
//
// 真正的写入（FlushDNS / SetDNS）是系统调用，这里只验证决策；真机行为未验证。
func TestIfaceDNSRestoreKeepsSource(t *testing.T) {
	auto, static := true, false
	cases := []struct {
		name     string
		iface    IfaceDNS
		wantAuto bool
	}{
		{"DHCP 自动获取的还原成自动", IfaceDNS{DNS: []string{"192.168.1.1"}, DNSAuto: &auto}, true},
		{"静态配置的还原成静态原值", IfaceDNS{DNS: []string{"8.8.8.8", "223.5.5.5"}, DNSAuto: &static}, false},
		{"旧快照没记来源按自动处理", IfaceDNS{DNS: []string{"192.168.1.1"}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.iface.restoreAsAuto(); got != tc.wantAuto {
				t.Errorf("restoreAsAuto = %v，期望 %v", got, tc.wantAuto)
			}
			if !tc.wantAuto && len(tc.iface.DNS) == 0 {
				t.Error("静态来源必须保留原 DNS 地址再写回")
			}
		})
	}
}

// ---------- 向后兼容 ----------

// 旧版本写下的 state.json 没有 guid / dns_auto / split_routes。缺字段不能崩，
// 也不能把「来源未知」当成静态（见 TestIfaceDNSRestoreKeepsSource）。
func TestSnapshotLoadsLegacyJSON(t *testing.T) {
	const legacy = `{
  "default_gateway": "192.168.1.1",
  "default_if_index": 12,
  "interfaces": [
    {"name": "WLAN", "index": 12, "dns": ["192.168.1.1"]}
  ],
  "captured_at": "2026-09-27T20:00:00+08:00",
  "server_next_hop": "192.168.1.1",
  "tunnel_gateway": "10.10.0.1",
  "server_ip": "103.143.11.34"
}`
	path := StatePath(t.TempDir())
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	snap, exists, err := LoadSnapshot(path)
	if err != nil || !exists {
		t.Fatalf("旧快照应能正常加载: exists=%v err=%v", exists, err)
	}
	if len(snap.Interfaces) != 1 {
		t.Fatalf("interface 数量 = %d", len(snap.Interfaces))
	}
	iface := snap.Interfaces[0]
	if iface.GUID != "" || iface.DNSAuto != nil || snap.Split != nil {
		t.Errorf("旧快照的新字段应为零值: guid=%q auto=%v split=%v",
			iface.GUID, iface.DNSAuto, snap.Split)
	}
	if !iface.restoreAsAuto() {
		t.Error("旧快照来源未知时必须按自动处理，不能写死静态地址")
	}
	if len(iface.DNS) != 1 || iface.DNS[0] != "192.168.1.1" {
		t.Errorf("旧快照的 DNS 原值丢了: %v", iface.DNS)
	}
}

// 稳定身份、DNS 来源、分流所有权都要能往返 state.json：
// 自愈不读内存，只有磁盘上这份记录。
func TestSnapshotRoundTripKeepsIdentityAndOwnership(t *testing.T) {
	path := StatePath(t.TempDir())
	static := false
	want := testSnapshot()
	want.Interfaces[0].GUID = "{11111111-2222-3333-4444-555555555555}"
	want.Interfaces[0].DNSAuto = &static
	want.Split = &splitOwnership{
		IfGUID:   want.Interfaces[0].GUID,
		IfIndex:  12,
		Gateway:  "192.168.1.1",
		Prefixes: []string{"1.0.1.0/24", "1.0.2.0/23"},
	}
	if err := SaveSnapshot(path, want); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	got, exists, err := LoadSnapshot(path)
	if err != nil || !exists {
		t.Fatalf("LoadSnapshot: exists=%v err=%v", exists, err)
	}
	if got.Interfaces[0].GUID != want.Interfaces[0].GUID {
		t.Errorf("网卡 GUID 没保住: %q", got.Interfaces[0].GUID)
	}
	if got.Interfaces[0].DNSAuto == nil || *got.Interfaces[0].DNSAuto != false {
		t.Errorf("DNS 来源没保住: %v", got.Interfaces[0].DNSAuto)
	}
	if got.Split == nil || got.Split.IfGUID != want.Split.IfGUID ||
		len(got.Split.Prefixes) != 2 || got.Split.Prefixes[0] != "1.0.1.0/24" {
		t.Errorf("分流所有权没保住: %+v", got.Split)
	}
}

// ---------- 网卡身份 ----------

// 友好名可以被用户改，接口索引会被系统复用，所以 GUID 优先；
// 有 GUID 却找不到时不能退回索引（可能改到另一张网卡）。
func TestPickAdapterPrefersGUID(t *testing.T) {
	keys := []adapterKey{
		{GUID: "{eth}", Index: 12, Name: "以太网"},
		{GUID: "{wlan}", Index: 7, Name: "WLAN"},
	}
	cases := []struct {
		name string
		ref  adapterRef
		want int
		ok   bool
	}{
		{"GUID 命中时忽略索引与名字", adapterRef{GUID: "{wlan}", Index: 12, Name: "改过名的网卡"}, 1, true},
		{"GUID 不在当前系统时不退回索引", adapterRef{GUID: "{gone}", Index: 12, Name: "以太网"}, 0, false},
		{"旧快照按索引", adapterRef{Index: 12, Name: "没有这个名字"}, 0, true},
		{"旧快照没有索引时按名字", adapterRef{Name: "WLAN"}, 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := pickAdapter(tc.ref, keys)
			if ok != tc.ok || (ok && got != tc.want) {
				t.Errorf("pickAdapter = (%d,%v)，期望 (%d,%v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// watcher 复用 DNS 原值前要确认是不是同一张网卡；索引相同不代表同一张。
func TestSameAdapterUsesGUIDWhenAvailable(t *testing.T) {
	if (adapterRef{GUID: "{a}", Index: 3}).sameAdapter(adapterKey{GUID: "{b}", Index: 3}) {
		t.Error("GUID 不同时不该认为是同一张网卡")
	}
	if !(adapterRef{GUID: "{a}", Index: 3}).sameAdapter(adapterKey{GUID: "{a}", Index: 9}) {
		t.Error("GUID 相同就是同一张网卡，索引变了也一样")
	}
	if !(adapterRef{Index: 3}).sameAdapter(adapterKey{GUID: "{x}", Index: 3}) {
		t.Error("旧快照没有 GUID 时应按索引判断")
	}
}

// ---------- 分流路由所有权 ----------

// 只清理「本接口 + 本网关 + metric 5 + 本程序装的前缀」，同形态的用户路由不能碰。
func TestSplitOwnershipOnlyMatchesInstalledRoutes(t *testing.T) {
	gw := netip.MustParseAddr("192.168.1.1")
	own := &splitOwnership{
		IfGUID:   "{eth}",
		IfIndex:  12,
		Gateway:  "192.168.1.1",
		Prefixes: []string{"1.0.1.0/24"},
	}
	cases := []struct {
		name    string
		own     *splitOwnership
		guid    string
		ifIndex uint32
		nextHop netip.Addr
		metric  uint32
		dest    netip.Prefix
		want    bool
	}{
		{"完全匹配", own, "{eth}", 12, gw, splitRouteMetric, netip.MustParsePrefix("1.0.1.0/24"), true},
		{"同接口同网关但前缀不在记录里", own, "{eth}", 12, gw, splitRouteMetric, netip.MustParsePrefix("1.0.2.0/23"), false},
		{"别的网卡上的同类路由", own, "{wlan}", 12, gw, splitRouteMetric, netip.MustParsePrefix("1.0.1.0/24"), false},
		{"网关不同", own, "{eth}", 12, netip.MustParseAddr("192.168.1.254"), splitRouteMetric, netip.MustParsePrefix("1.0.1.0/24"), false},
		{"metric 不同", own, "{eth}", 12, gw, 6, netip.MustParsePrefix("1.0.1.0/24"), false},
		{"旧快照没有 GUID 时按索引", &splitOwnership{IfIndex: 12, Gateway: "192.168.1.1", Prefixes: []string{"1.0.1.0/24"}}, "", 12, gw, splitRouteMetric, netip.MustParsePrefix("1.0.1.0/24"), true},
		{"没有所有权记录时一条都不认", nil, "{eth}", 12, gw, splitRouteMetric, netip.MustParsePrefix("1.0.1.0/24"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.own.ownsRoute(tc.guid, tc.ifIndex, tc.nextHop, tc.metric, tc.dest); got != tc.want {
				t.Errorf("ownsRoute = %v，期望 %v", got, tc.want)
			}
		})
	}
}

// 当前版本只接管 IPv4，IPv6 的隧道地址或网关必须在入口就拒绝。
//
// 不拒绝的后果不是「IPv6 不能用」这么简单：/1 路由、MSS clamp、dnsmasq 都只处理
// IPv4，配置里混进 IPv6 会让写入一半成功一半失败，还原时还可能把宿主的 IPv6
// 配置留在半改状态 —— 那比直接报错难查得多。
func TestValidateIPv4OnlyRejectsIPv6(t *testing.T) {
	cases := []struct {
		name    string
		cfg     NetConfig
		wantErr bool
	}{
		{
			name: "全 IPv4 通过",
			cfg:  NetConfig{ServerIP: "103.143.239.213", TunnelIP: "10.10.0.2", Gateway: "192.168.1.1"},
		},
		{
			name:    "IPv6 隧道地址要拒绝",
			cfg:     NetConfig{ServerIP: "103.143.239.213", TunnelIP: "fd00::2"},
			wantErr: true,
		},
		{
			name:    "IPv6 网关要拒绝",
			cfg:     NetConfig{ServerIP: "103.143.239.213", TunnelIP: "10.10.0.2", Gateway: "fe80::1"},
			wantErr: true,
		},
		{
			name:    "IPv6 服务端要拒绝",
			cfg:     NetConfig{ServerIP: "2001:db8::1", TunnelIP: "10.10.0.2"},
			wantErr: true,
		},
		{
			// 云服在直连网段时不加绕行路由，网关本来就是空的
			name: "空网关是合法的",
			cfg:  NetConfig{ServerIP: "103.143.239.213", TunnelIP: "10.10.0.2"},
		},
		{
			// 域名交给 resolveIPv4 报它自己的错，这里不该误报
			name: "域名跳过校验",
			cfg:  NetConfig{ServerIP: "example.com:62233", TunnelIP: "10.10.0.2"},
		},
		{
			name: "带端口的服务端地址",
			cfg:  NetConfig{ServerIP: "103.143.239.213:62233", TunnelIP: "10.10.0.2"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateIPv4Only(tc.cfg)
			if tc.wantErr && err == nil {
				t.Fatal("该拒绝却通过了")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("不该拒绝: %v", err)
			}
			if tc.wantErr && err != nil && !strings.Contains(err.Error(), "只接管 IPv4") {
				t.Errorf("错误信息没说清原因: %v", err)
			}
		})
	}
}
