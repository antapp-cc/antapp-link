//go:build windows && (amd64 || arm64)

package client

import "testing"

// 内嵌的 dll 是发行包能不能单文件跑起来的前提。留一条测试挡住
// 「embed 指向了一个空占位文件」这种会在用户机器上才炸掉的失误。
func TestWintunDLLIsEmbeddedAndLooksLikePE(t *testing.T) {
	if len(wintunDLL) == 0 {
		t.Fatal("这个架构没有内嵌 wintun.dll，分发出去会跑不起来")
	}
	if len(wintunDLL) < 100_000 {
		t.Fatalf("内嵌的 wintun.dll 只有 %d 字节，不像真的驱动库", len(wintunDLL))
	}
	if wintunDLL[0] != 'M' || wintunDLL[1] != 'Z' {
		t.Fatalf("内嵌的数据没有 PE 文件头（前两字节 %q），不是有效的 dll", wintunDLL[:2])
	}
}
