package client

import (
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/antapp-cc/antapp-link/internal/pki"
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

// 网络事务锁绝不能被重入。
//
// reconcile / hotApplyRouteTable / restoreNetwork 都会拿这把锁。如果哪条路径
// 在持锁时又调了另一个会拿锁的函数，就会把自己锁死 —— 那种死锁表现为「断开点了
// 没反应、网络还原卡住」，比崩溃难查得多。这个用例把约束钉住。
func TestNetTxnLockIsNotReentrant(t *testing.T) {
	netTxnMu.Lock()
	defer netTxnMu.Unlock()
	if netTxnMu.TryLock() {
		netTxnMu.Unlock()
		t.Fatal("网络事务锁被重入：持锁期间不该还能拿到它")
	}
}

// 还原必须让代际前进，否则排队中的热重铺无法判断「网络已经被撤了」。
func TestNetEpochAdvances(t *testing.T) {
	before := netEpoch.Load()
	netEpoch.Add(1)
	after := netEpoch.Load()
	if after != before+1 {
		t.Fatalf("代际没有前进：%d -> %d", before, after)
	}
}

// 接管时默认用公共 DNS，而不是连接码里的隧道网关。
//
// 用 10.10.0.1 意味着每个查询都要多走一趟跨境隧道：实测解析平均 578ms，
// 公共 DNS 只要 93ms，网页打开速度差 40%。Pi 的域名不依赖系统 DNS ——
// .minepi.com 由 NRPT 强制走隧道网关。
func TestResolveClientDNSPrefersPublic(t *testing.T) {
	t.Setenv("ANTAPP_CLIENT_DNS", "")
	inv := pki.Invite{DNS: []string{"10.10.0.1"}}
	got := resolveClientDNS(inv)
	if len(got) != 2 || got[0] != "223.5.5.5" {
		t.Fatalf("默认应换成公共 DNS，实际 %v", got)
	}
}

// 环境变量能把它切回隧道，方便 A/B 或出问题时回退。
func TestResolveClientDNSTunnelOverride(t *testing.T) {
	t.Setenv("ANTAPP_CLIENT_DNS", "tunnel")
	inv := pki.Invite{DNS: []string{"10.10.0.1"}}
	got := resolveClientDNS(inv)
	if len(got) != 1 || got[0] != "10.10.0.1" {
		t.Fatalf("tunnel 覆盖应保留连接码的地址，实际 %v", got)
	}
}

// 自定义列表按逗号切，空项丢掉。
func TestResolveClientDNSCustomList(t *testing.T) {
	t.Setenv("ANTAPP_CLIENT_DNS", " 1.1.1.1 , ,8.8.8.8 ")
	inv := pki.Invite{DNS: []string{"10.10.0.1"}}
	got := resolveClientDNS(inv)
	if len(got) != 2 || got[0] != "1.1.1.1" || got[1] != "8.8.8.8" {
		t.Fatalf("自定义列表解析错，实际 %v", got)
	}
}

// 连接码已经写了具体的公共解析器时，那是签发者有意选的，不该被默认值覆盖。
func TestResolveClientDNSKeepsExplicitPublic(t *testing.T) {
	t.Setenv("ANTAPP_CLIENT_DNS", "")
	inv := pki.Invite{DNS: []string{"9.9.9.9"}}
	got := resolveClientDNS(inv)
	if len(got) != 1 || got[0] != "9.9.9.9" {
		t.Fatalf("连接码指定的解析器应保留，实际 %v", got)
	}
}

// ---------- 还原失败时的分流账本 ----------

// 还原失败时不能清掉进程内账本：热更新铺的那一版（B）只存在于内存里。
//
// 复现红线：快照账本为 A，热更新后进程内账本为 B。第一次还原删 B 失败，若顺手
// 清空账本，第二次还原就只认快照里的 A、返回成功 —— B 那批路由此后再也没有人
// 知道它们存在，也就永远清不掉。
func TestRestoreSplitRoutesKeepsFailedLiveLedger(t *testing.T) {
	t.Cleanup(clearLiveSplitOwnership)
	snapOwn := &splitOwnership{IfIndex: 12, Gateway: "192.168.1.1", Prefixes: []string{"1.0.1.0/24"}}
	liveOwn := &splitOwnership{IfIndex: 12, Gateway: "192.168.1.1", Prefixes: []string{"1.0.2.0/23"}}
	setLiveSplitOwnership(liveOwn)

	failLive := func(own *splitOwnership) (int, error) {
		if own == liveOwn {
			return 0, errors.New("删除路由 1.0.2.0/23: 拒绝访问")
		}
		return len(own.Prefixes), nil
	}
	if _, err := restoreSplitRoutes(snapOwn, failLive); err == nil {
		t.Fatal("B 没删掉必须报错，否则调用方会删掉 state.json、当还原已经完成")
	}
	if got := currentLiveSplitOwnership(); got != liveOwn {
		t.Fatalf("失败时进程内账本必须保留 B，实际 %+v", got)
	}

	// 第二轮：这次 B 删得掉。必须真的重试到 B，而不是只处理快照里的 A 就报成功。
	var swept []*splitOwnership
	allOK := func(own *splitOwnership) (int, error) {
		swept = append(swept, own)
		return len(own.Prefixes), nil
	}
	if _, err := restoreSplitRoutes(snapOwn, allOK); err != nil {
		t.Fatalf("第二轮应当成功: %v", err)
	}
	if !slices.Contains(swept, liveOwn) {
		t.Fatal("第二轮没有重试 B 的账本，B 的路由永远清不掉")
	}
	if got := currentLiveSplitOwnership(); got != nil {
		t.Errorf("全部清干净后进程内账本应清空，实际 %+v", got)
	}
}

// 热更新时旧账本删失败，没删掉的前缀必须并进新账本：新网段表未必包含它们，
// 一趟丢弃就等于把它们从账本上抹掉，以后热更新和还原都不会再碰。
func TestMergeSplitOwnershipKeepsFailedPrefixes(t *testing.T) {
	old := &splitOwnership{IfGUID: "{eth}", IfIndex: 12, Gateway: "192.168.1.1",
		Prefixes: []string{"1.0.1.0/24", "1.0.2.0/23"}}
	fresh := &splitOwnership{IfGUID: "{eth}", IfIndex: 12, Gateway: "192.168.1.1",
		Prefixes: []string{"1.0.1.0/24", "1.0.3.0/24"}}

	merged := mergeSplitOwnership(fresh, old)
	if merged == nil {
		t.Fatal("同网卡同网关的两份账本应当能合并")
	}
	if len(merged.Prefixes) != 3 {
		t.Fatalf("合并后应是去重的 3 条前缀，实际 %v", merged.Prefixes)
	}
	for _, want := range []string{"1.0.1.0/24", "1.0.2.0/23", "1.0.3.0/24"} {
		if !slices.Contains(merged.Prefixes, want) {
			t.Errorf("合并后丢了 %s: %v", want, merged.Prefixes)
		}
	}

	other := &splitOwnership{IfGUID: "{wlan}", IfIndex: 7, Gateway: "192.168.1.1"}
	if mergeSplitOwnership(fresh, other) != nil {
		t.Error("网卡不同时一条账本记录装不下两套路由，应返回 nil 让调用方报数")
	}

	// 热更新：旧表删失败时新账本必须带着旧前缀；删干净了就只用新账本。
	own, kept := splitLedgerAfterRefresh(fresh, old, true)
	if !kept || len(own.Prefixes) != 3 {
		t.Errorf("旧表删失败时新账本应并回残留前缀，kept=%v own=%v", kept, own)
	}
	if own, kept := splitLedgerAfterRefresh(fresh, old, false); kept || own != fresh {
		t.Error("旧表删干净时不该把上一版前缀再带回来")
	}
}

// ---------- 热重铺的账本持久化 ----------

// 热重铺必须先落盘「旧账本 + 预计新建的前缀」，再动路由。
//
// 否则「删了旧路由、铺了新路由、还没写盘」这段窗口里崩溃，磁盘上只有上一版
// 账本，新铺的那批路由无人认领，重启自愈后永久残留。
func TestRefreshSplitRoutesPersistsSupersetBeforeTouchingRoutes(t *testing.T) {
	t.Cleanup(clearLiveSplitOwnership)
	ref := adapterKey{GUID: "{eth}", Index: 12}
	gw := netip.MustParseAddr("192.168.1.1")
	previous := &splitOwnership{IfGUID: "{eth}", IfIndex: 12, Gateway: "192.168.1.1",
		Prefixes: []string{"1.0.1.0/24"}}
	table := []string{"1.0.1.0/24", "1.0.2.0/23", "1.0.9.0/24"}
	// 1.0.9.0/24 已经在路由表里且不在账本里：那是用户的同形状路由，不能进账本。
	existing := map[string]struct{}{"1.0.9.0/24": {}}
	var order []string
	var persisted []*splitOwnership
	ops := splitRouteOps{
		existing:  func() (map[string]struct{}, error) { return existing, nil },
		sweep:     func(own *splitOwnership) (int, error) { order = append(order, "sweep"); return len(own.Prefixes), nil },
		sweepForm: func() (int, error) { order = append(order, "sweepForm"); return 0, nil },
		install: func(prev []*splitOwnership) ([]string, int) {
			order = append(order, "install")
			if len(prev) != 1 || prev[0] != previous {
				t.Errorf("铺路由必须拿旧账本当归属证据，实际 %v", prev)
			}
			return []string{"1.0.1.0/24", "1.0.2.0/23"}, 1
		},
		persist: func(own *splitOwnership) error {
			order = append(order, "persist")
			persisted = append(persisted, own)
			return nil
		},
	}

	own := refreshSplitRoutes(ref, gw, previous, table, ops)

	if len(order) < 2 || order[0] != "persist" {
		t.Fatalf("第一步必须是落盘账本，实际顺序 %v", order)
	}
	if len(persisted) != 2 {
		t.Fatalf("应当落盘两次（预写 + 最终），实际 %d 次", len(persisted))
	}
	tentative := persisted[0]
	for _, want := range []string{"1.0.1.0/24", "1.0.2.0/23"} {
		if !slices.Contains(tentative.Prefixes, want) {
			t.Errorf("预写账本缺 %s: %v", want, tentative.Prefixes)
		}
	}
	if slices.Contains(tentative.Prefixes, "1.0.9.0/24") {
		t.Errorf("表里已存在且无账本证据的前缀不能提前认领: %v", tentative.Prefixes)
	}
	if own == nil || currentLiveSplitOwnership() != own {
		t.Errorf("热重铺完成后应把最终账本设为 live 账本，实际 own=%v live=%v",
			own, currentLiveSplitOwnership())
	}
}

// 预写盘失败时必须原地放弃：旧路由还没动，系统保持上一版可用状态；
// 硬着头皮铺下去就成了「路由已换、磁盘账本还是上一版」。
func TestRefreshSplitRoutesAbortsWhenPrePersistFails(t *testing.T) {
	t.Cleanup(clearLiveSplitOwnership)
	ref := adapterKey{GUID: "{eth}", Index: 12}
	gw := netip.MustParseAddr("192.168.1.1")
	previous := &splitOwnership{IfGUID: "{eth}", IfIndex: 12, Gateway: "192.168.1.1",
		Prefixes: []string{"1.0.1.0/24"}}
	setLiveSplitOwnership(previous)
	touched := false
	ops := splitRouteOps{
		existing:  func() (map[string]struct{}, error) { return nil, nil },
		sweep:     func(*splitOwnership) (int, error) { touched = true; return 0, nil },
		sweepForm: func() (int, error) { touched = true; return 0, nil },
		install:   func([]*splitOwnership) ([]string, int) { touched = true; return nil, 0 },
		persist:   func(*splitOwnership) error { return errors.New("磁盘只读") },
	}

	own := refreshSplitRoutes(ref, gw, previous, []string{"1.0.2.0/23"}, ops)

	if own != previous {
		t.Errorf("预写盘失败应保留旧账本，实际 %+v", own)
	}
	if touched {
		t.Error("预写盘失败时不能动任何路由（旧路由还没撤）")
	}
	if currentLiveSplitOwnership() != previous {
		t.Error("预写盘失败不能改进程内账本")
	}
}

// 最终账本写盘失败不再回滚：磁盘上留着预写的那份超集，只要它覆盖了所有已铺
// 前缀，崩溃后自愈就清得掉。
func TestRefreshSplitRoutesKeepsSupersetLedgerOnFinalPersistFailure(t *testing.T) {
	t.Cleanup(clearLiveSplitOwnership)
	ref := adapterKey{GUID: "{eth}", Index: 12}
	gw := netip.MustParseAddr("192.168.1.1")
	previous := &splitOwnership{IfGUID: "{eth}", IfIndex: 12, Gateway: "192.168.1.1",
		Prefixes: []string{"1.0.1.0/24"}}
	var persisted []*splitOwnership
	ops := splitRouteOps{
		existing:  func() (map[string]struct{}, error) { return nil, nil },
		sweep:     func(own *splitOwnership) (int, error) { return len(own.Prefixes), nil },
		sweepForm: func() (int, error) { return 0, nil },
		install:   func([]*splitOwnership) ([]string, int) { return []string{"1.0.1.0/24", "1.0.2.0/23"}, 0 },
		persist: func(own *splitOwnership) error {
			persisted = append(persisted, own)
			if len(persisted) == 2 {
				return errors.New("磁盘只读")
			}
			return nil
		},
	}

	own := refreshSplitRoutes(ref, gw, previous, []string{"1.0.1.0/24", "1.0.2.0/23"}, ops)

	if own == nil || currentLiveSplitOwnership() != own {
		t.Fatal("最终账本写盘失败不能阻止进程内账本更新")
	}
	if len(persisted) != 2 {
		t.Fatalf("应当尝试落盘两次，实际 %d 次", len(persisted))
	}
	for _, p := range own.Prefixes {
		if !slices.Contains(persisted[0].Prefixes, p) {
			t.Errorf("预写账本漏了已铺前缀 %s，崩溃后清不掉: %v", p, persisted[0].Prefixes)
		}
	}
}

// 旧账本记的是另一张网卡/网关时，热重铺不能拿它当本网卡的证据继续铺：
// 那是出口迁移该处理的状态，混着铺会把账本钉到错误的接口上。
func TestRefreshSplitRoutesSkipsWhenLedgerBelongsToOtherAdapter(t *testing.T) {
	t.Cleanup(clearLiveSplitOwnership)
	ref := adapterKey{GUID: "{eth}", Index: 12}
	gw := netip.MustParseAddr("192.168.1.1")
	previous := &splitOwnership{IfGUID: "{wlan}", IfIndex: 7, Gateway: "192.168.9.1",
		Prefixes: []string{"1.0.1.0/24"}}
	var touched bool
	ops := splitRouteOps{
		existing:  func() (map[string]struct{}, error) { touched = true; return nil, nil },
		sweep:     func(*splitOwnership) (int, error) { touched = true; return 0, nil },
		sweepForm: func() (int, error) { touched = true; return 0, nil },
		install:   func([]*splitOwnership) ([]string, int) { touched = true; return nil, 0 },
		persist:   func(*splitOwnership) error { touched = true; return nil },
	}

	own := refreshSplitRoutes(ref, gw, previous, []string{"1.0.2.0/23"}, ops)

	if own != previous {
		t.Errorf("应保留旧账本等迁移处理，实际 %+v", own)
	}
	if touched {
		t.Error("账本属于别的网卡时不应写盘或动路由")
	}
}

// 多个地址时不算「只有隧道网关」，原样保留。
func TestIsTunnelDNS(t *testing.T) {
	cases := []struct {
		in   []string
		want bool
	}{
		{[]string{"10.10.0.1"}, true},
		{[]string{"127.0.0.1"}, true},
		{[]string{"192.168.1.1"}, true},
		{[]string{"223.5.5.5"}, false},
		{[]string{"10.10.0.1", "223.5.5.5"}, false},
		{[]string{}, false},
	}
	for _, c := range cases {
		if got := isTunnelDNS(c.in); got != c.want {
			t.Errorf("isTunnelDNS(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}
