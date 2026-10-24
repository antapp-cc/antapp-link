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

// 有待还原现场时必须先恢复：反复调 Connect 只会一直撞「请再点一次断开连接」。
func TestReconnectStepPrefersRecoverOverConnect(t *testing.T) {
	if got := reconnectStepFor(AppStatus{PendingRestore: true}, true); got != stepRecover {
		t.Fatalf("待还原时应先恢复，实际 %v", got)
	}
	// 用户点「断开连接」之后也一样：隧道可以不要，半接管的路由/DNS 必须还原，
	// 否则用户既连不上、也没人把系统网络改回去。
	if got := reconnectStepFor(AppStatus{PendingRestore: true}, false); got != stepRecover {
		t.Fatalf("用户断开后仍应先把网络还原干净，实际 %v", got)
	}
}

// 更新交接期间不动网络：ApplyUpdate 在断开失败时会立刻复位 updating，
// 下一轮自然走 stepRecover，不会把待还原现场一直晾着。
func TestReconnectStepLeavesNetworkAloneWhileUpdating(t *testing.T) {
	if got := reconnectStepFor(AppStatus{Updating: true, PendingRestore: true}, true); got != stepReset {
		t.Fatalf("更新交接期间不该动网络，实际 %v", got)
	}
}

// 用户不想连 / 更新中 / 已在线：都只重置退避，不做任何网络动作。
func TestReconnectStepResetsWhenIdle(t *testing.T) {
	cases := []struct {
		name string
		st   AppStatus
		want bool
	}{
		{"用户断开", AppStatus{}, false},
		{"更新中", AppStatus{Updating: true}, true},
		{"已在线", AppStatus{Online: true}, true},
	}
	for _, c := range cases {
		if got := reconnectStepFor(c.st, c.want); got != stepReset {
			t.Errorf("%s: 应为 stepReset，实际 %v", c.name, got)
		}
	}
}

// 正在连接/断开：等它收尾，不要插一脚。
func TestReconnectStepWaitsWhileBusy(t *testing.T) {
	if got := reconnectStepFor(AppStatus{Connecting: true}, true); got != stepWait {
		t.Errorf("连接中应为 stepWait，实际 %v", got)
	}
	if got := reconnectStepFor(AppStatus{Disconnecting: true}, true); got != stepWait {
		t.Errorf("断开中应为 stepWait，实际 %v", got)
	}
}

// 普通离线：去连接。
func TestReconnectStepConnectsWhenOffline(t *testing.T) {
	if got := reconnectStepFor(AppStatus{Running: true}, true); got != stepConnect {
		t.Fatalf("离线时应去连接，实际 %v", got)
	}
}
