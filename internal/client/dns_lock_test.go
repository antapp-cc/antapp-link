package client

import (
	"strings"
	"testing"
)

// DNS 锁定是「让解析只走隧道」的唯一强制手段，命令一旦没生成对，
// 用户就会退回「解析被本地 IPv6 DNS 劫持」的状态。
func TestDNSLockCommandsCoverEachInterface(t *testing.T) {
	cmds := dnsLockCommands([]string{"WLAN", "以太网"})
	// 每张网卡 UDP/TCP 各一条
	if len(cmds) != 4 {
		t.Fatalf("应该 4 条（2 网卡 × 2 协议），实际 %d", len(cmds))
	}

	all := dumpCommands(cmds)
	for _, want := range []string{"WLAN", "以太网", "UDP", "TCP", "RemotePort 53", "Block", "Outbound"} {
		if !strings.Contains(all, want) {
			t.Errorf("命令里缺 %q:\n%s", want, all)
		}
	}
	// 不该动网卡本身 —— 上一版用 Disable-NetAdapterBinding 直接把人弄断网了
	for _, banned := range []string{"Disable-NetAdapterBinding", "Set-DnsClientServerAddress"} {
		if strings.Contains(all, banned) {
			t.Errorf("不该出现 %q（会动网卡/重置 DNS）:\n%s", banned, all)
		}
	}
	// 命令必须自带 exit 0，否则「规则已存在」这类非致命情况会被上层当故障
	for _, c := range cmds {
		if !strings.Contains(strings.Join(c.Args, " "), "exit 0") {
			t.Errorf("命令没保证退出码为 0: %s", c.String())
		}
	}
}

func TestDNSLockNoInterfaces(t *testing.T) {
	if cmds := dnsLockCommands(nil); len(cmds) != 0 {
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
	first := cmds[0].String()
	if !strings.Contains(first, dnsLockPrefix) {
		t.Errorf("第一条应该是解 DNS 锁定，实际:\n  %s", first)
	}
}

// 解锁按显示名前缀删，不依赖当时锁了哪几张网卡 —— 快照损坏时也要能清干净。
func TestDNSUnlockDeletesByPrefix(t *testing.T) {
	all := dumpCommands(dnsUnlockCommands())
	if !strings.Contains(all, dnsLockPrefix+"*") {
		t.Errorf("应该按前缀删除规则:\n%s", all)
	}
}
