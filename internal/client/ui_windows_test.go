//go:build windows

package client

import (
	"os"
	"testing"
)

// 打开目录会真的弹一个资源管理器窗口，所以默认跳过。
// 需要人工确认时：$env:ANTAPP_TEST_OPEN='1'; go test ./internal/client/ -run OpenInExplorer -v
func TestOpenInExplorer(t *testing.T) {
	if os.Getenv("ANTAPP_TEST_OPEN") == "" {
		t.Skip("会弹出资源管理器窗口；设 ANTAPP_TEST_OPEN=1 才跑")
	}
	dir := t.TempDir()
	if err := openInExplorer(dir); err != nil {
		t.Fatalf("打开目录失败: %v", err)
	}
	t.Logf("已请求打开 %s（窗口应当弹出来了）", dir)
}
