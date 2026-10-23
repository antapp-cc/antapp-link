//go:build linux

package server

import (
	"strings"
	"testing"
)

// deploy/install.sh 的 sysctl -w 重启就没了，这份 sysctl.d 才是持久化的那份。
// 两边少一项，灰度时就可能是「重启后调优悄悄失效」。
func TestSysctlContentCoversTuning(t *testing.T) {
	for _, want := range []string{
		"net.ipv4.ip_forward=1",
		"net.core.default_qdisc=fq",
		"net.ipv4.tcp_congestion_control=bbr",
		"net.ipv4.tcp_slow_start_after_idle=0",
		"net.core.rmem_max=16777216",
		"net.core.wmem_max=16777216",
		"net.ipv4.tcp_rmem=4096 87380 16777216",
		"net.ipv4.tcp_wmem=4096 65536 16777216",
		"net.ipv4.tcp_mtu_probing=1",
		"net.ipv4.tcp_fastopen=3",
	} {
		if !strings.Contains(sysctlContent, want) {
			t.Errorf("/etc/sysctl.d 内容缺少 %q", want)
		}
	}
}

// 内置安装入口只写持久化配置，不立即 apply：内核还不是 bbr 时必须提示，
// 否则「装完没生效」会被当成安装失败。
func TestBBRReport(t *testing.T) {
	if got := bbrReport("bbr\n"); got != "" {
		t.Fatalf("已经是 bbr 时不该提示，实际 %q", got)
	}
	got := bbrReport("cubic\n")
	if !strings.Contains(got, "cubic") {
		t.Errorf("提示里要带当前算法，实际 %q", got)
	}
	if !strings.Contains(got, sysctlFile) {
		t.Errorf("提示里要带持久化配置路径，实际 %q", got)
	}
	if !strings.Contains(got, "重启后生效") {
		t.Errorf("提示里要说明何时生效，实际 %q", got)
	}
}
