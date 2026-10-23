//go:build windows

package client

import (
	"errors"
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

// 界面没建起来就不能写 ready：旧进程会据此退出，而新版随后也会因为没有窗口而退出，
// 更新完用户手上一个能用的客户端都没有。
func TestBuildUIThenConfirmSkipsConfirmOnBuildFailure(t *testing.T) {
	buildErr := errors.New("创建托盘图标失败")
	var order []string
	err := buildUIThenConfirm(
		func() error { order = append(order, "build"); return buildErr },
		func() error { order = append(order, "confirm"); return nil },
		func(error) { order = append(order, "warn") },
	)
	if !errors.Is(err, buildErr) {
		t.Fatalf("界面构建错误必须原样返回，实际: %v", err)
	}
	if len(order) != 1 || order[0] != "build" {
		t.Fatalf("界面建失败时不能写 ready，实际顺序: %v", order)
	}
}

// ready 必须等图标、窗口、托盘都建好之后才写。
func TestBuildUIThenConfirmWritesReadyAfterUI(t *testing.T) {
	var order []string
	err := buildUIThenConfirm(
		func() error { order = append(order, "build"); return nil },
		func() error { order = append(order, "confirm"); return nil },
		func(error) { order = append(order, "warn") },
	)
	if err != nil {
		t.Fatalf("buildUIThenConfirm: %v", err)
	}
	if len(order) != 2 || order[0] != "build" || order[1] != "confirm" {
		t.Fatalf("ready 必须在界面建好之后写，实际顺序: %v", order)
	}
}

// 写 ready 失败不该阻止界面启动：状态文件写不进去时旧进程会按超时处理，
// 用户至少还能用这个新版。
func TestBuildUIThenConfirmWarnsWhenConfirmFails(t *testing.T) {
	confirmErr := errors.New("交接状态文件被删了")
	var warned error
	err := buildUIThenConfirm(
		func() error { return nil },
		func() error { return confirmErr },
		func(err error) { warned = err },
	)
	if err != nil {
		t.Fatalf("确认失败不该阻止界面启动: %v", err)
	}
	if !errors.Is(warned, confirmErr) {
		t.Errorf("确认失败必须上报，实际: %v", warned)
	}
}
