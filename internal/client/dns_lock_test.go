package client

import (
	"strings"
	"testing"
)

// DNS 锁定是「让解析只走隧道」的唯一强制手段，命令一旦没生成对，
// 用户就会退回「解析被本地 IPv6 DNS 劫持」的状态。
//
// 关键设计：按**目标地址**封堵，不按网卡名。照 OpenVPN 的 block-outside-dns 来 ——
// 实测 Windows 防火墙的 -InterfaceAlias 对出站 DNS 不生效（规则 Enabled 但查询照发），
// 而 -RemoteAddress 立刻生效。
// IPv6 的 DNS 必须一起封。
//
// 这是实测踩出来的：只封 IPv4 时，系统会在 IPv4 DNS 被指向隧道之后
// 转用 RA 下发的 IPv6 DNS，查询绕开隧道被劫持 —— minepi.com 打不开、
// Edge 新标签页空白，都是这个原因。IPv6 DNS 没法用 netsh ipv4 改掉，
// 只能封，所以它必须进封堵名单。
// 撤国内直连路由必须是**一条**命令，不能逐条 route delete。
//
// 实测教训：route.exe 删一条不存在的路由会卡住，809 条逐条删把整个还原流程
// 堵死了 —— 客户端卡在「发现上次残留的网络配置，先还原」，连 /1 接管路由
// 都没来得及撤，用户就留在半挂状态。
func TestSplitRouteDeleteIsSingleCommand(t *testing.T) {
	cmd := splitRouteDeleteCommand(testSnapshot())
	joined := strings.Join(cmd.Args, " ")

	if cmd.Name != "powershell" {
		t.Errorf("应该用 powershell 批量删，实际用 %q", cmd.Name)
	}
	if !strings.Contains(joined, "Remove-NetRoute") {
		t.Errorf("应该用 Remove-NetRoute 批量删:\n%s", joined)
	}
	// 按下一跳 + metric 筛，这样网段表更新过也能清干净
	if !strings.Contains(joined, "RouteMetric") || !strings.Contains(joined, "NextHop") {
		t.Errorf("应该按下一跳和 metric 筛选残留路由:\n%s", joined)
	}
	// 绝不能碰用户的默认路由
	if !strings.Contains(joined, "0.0.0.0/0") {
		t.Errorf("必须排除默认路由，否则会把用户的默认路由删掉:\n%s", joined)
	}
	if !strings.Contains(joined, "exit 0") {
		t.Errorf("命令必须自带 exit 0:\n%s", joined)
	}
}

func TestDNSLockIncludesIPv6OutsideDNS(t *testing.T) {
	ifaces := []IfaceDNS{
		{
			Name:  "WLAN",
			DNS:   []string{"192.168.5.1"},
			DNSv6: []string{"fe80::c58b:4f1e:33b9:a864"},
		},
	}
	cmds := dnsLockCommands(ifaces, []string{"10.10.0.1"})
	all := dumpCommands(cmds)

	if !strings.Contains(all, "fe80::c58b:4f1e:33b9:a864") {
		t.Errorf("IPv6 的 DNS 也必须封（只封 IPv4 时系统会转用它）:\n%s", all)
	}
	if !strings.Contains(all, "192.168.5.1") {
		t.Errorf("IPv4 的 DNS 也要封:\n%s", all)
	}
	// 两个地址 × UDP/TCP
	if len(cmds) != 4 {
		t.Errorf("应该 4 条（2 地址 × 2 协议），实际 %d", len(cmds))
	}
}

func TestDNSLockBlocksOutsideAddressesOnly(t *testing.T) {
	ifaces := []IfaceDNS{
		{Name: "WLAN", DNS: []string{"192.168.5.1", "fe80::c58b:4f1e:33b9:a864"}},
	}
	cmds := dnsLockCommands(ifaces, []string{"10.10.0.1"})
	// 2 个外部地址 × UDP/TCP
	if len(cmds) != 4 {
		t.Fatalf("应该 4 条（2 地址 × 2 协议），实际 %d", len(cmds))
	}

	all := dumpCommands(cmds)
	for _, want := range []string{"192.168.5.1", "fe80::c58b:4f1e:33b9:a864", "RemotePort 53", "Block", "Outbound"} {
		if !strings.Contains(all, want) {
			t.Errorf("命令里缺 %q:\n%s", want, all)
		}
	}
	// 隧道自己的 DNS 绝不能封，否则隧道内的解析也断了
	if strings.Contains(all, "10.10.0.1") {
		t.Errorf("不该封隧道 DNS 10.10.0.1:\n%s", all)
	}
	// 不该按接口封 —— 实测无效
	if strings.Contains(all, "InterfaceAlias") {
		t.Errorf("不该用 InterfaceAlias（实测对出站 DNS 无效）:\n%s", all)
	}
	// 不该动网卡本身：上一版用 Disable-NetAdapterBinding 直接把人弄断网了
	if strings.Contains(all, "Disable-NetAdapterBinding") {
		t.Errorf("不该禁用网卡绑定（会重置网卡、清掉 DNS）:\n%s", all)
	}
	// 命令必须自带 exit 0，否则「规则已存在」这类非致命情况会被上层当故障
	for _, c := range cmds {
		if !strings.Contains(strings.Join(c.Args, " "), "exit 0") {
			t.Errorf("命令没保证退出码为 0: %s", c.String())
		}
	}
}

// 网卡的 DNS 就是隧道地址时，不该产生任何封堵规则。
func TestDNSLockSkipsTunnelDNS(t *testing.T) {
	ifaces := []IfaceDNS{{Name: "WLAN", DNS: []string{"10.10.0.1"}}}
	if cmds := dnsLockCommands(ifaces, []string{"10.10.0.1"}); len(cmds) != 0 {
		t.Errorf("隧道 DNS 不该被封锁，实际生成 %d 条", len(cmds))
	}
}

// 多张网卡共用同一个 DNS 时只封一次，别把规则表撑爆。
func TestDNSLockDeduplicatesAddresses(t *testing.T) {
	ifaces := []IfaceDNS{
		{Name: "WLAN", DNS: []string{"192.168.5.1"}},
		{Name: "以太网", DNS: []string{"192.168.5.1"}},
	}
	cmds := dnsLockCommands(ifaces, []string{"10.10.0.1"})
	if len(cmds) != 2 {
		t.Errorf("同一地址应只封一次（UDP+TCP 共 2 条），实际 %d 条", len(cmds))
	}
}

func TestDNSLockNoInterfaces(t *testing.T) {
	if cmds := dnsLockCommands(nil, []string{"10.10.0.1"}); len(cmds) != 0 {
		t.Errorf("没有网卡时不该生成命令，实际 %d 条", len(cmds))
	}
}

// 还原必须**先解锁**：否则后面的步骤万一失败，用户会卡在
// 「DNS 被锁死但隧道已经拆掉」——那等于完全没法解析域名。
func TestRestoreUnlocksDNSFirst(t *testing.T) {
	cmds := RestoreCommands(testSnapshot(), testNetConfig())
	if len(cmds) == 0 {
		t.Fatal("没有还原命令")
	}
	if first := cmds[0].String(); !strings.Contains(first, dnsLockPrefix) {
		t.Errorf("第一条应该是解 DNS 锁定，实际:\n  %s", first)
	}
}

// 解锁按显示名前缀删，不依赖当时封了哪些地址 —— 快照损坏时也要能清干净。
func TestDNSUnlockDeletesByPrefix(t *testing.T) {
	all := dumpCommands(dnsUnlockCommands())
	if !strings.Contains(all, dnsLockPrefix+"*") {
		t.Errorf("应该按前缀删除规则:\n%s", all)
	}
}
