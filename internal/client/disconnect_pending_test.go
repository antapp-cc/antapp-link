//go:build windows

package client

import (
	"errors"
	"strings"
	"testing"
)

// stubRevertPending 替换「撤掉未完成迁移」的入口，注入失败或记录调用。
func stubRevertPending(fn func(*sessionWatcher) (Snapshot, error)) func() {
	old := revertPendingFn
	revertPendingFn = fn
	return func() { revertPendingFn = old }
}

// 停掉 watcher 后若它还有一笔没撤回的迁移，Disconnect 不能报成功。
//
// 那张网卡上还留着隧道 DNS 和云服绕行路由，而 App 已经把 watcher 丢掉了 ——
// 再没人会去撤它。审阅报告第 2 项用隔离测试复现过：注入 DNS 回滚失败后
// Disconnect 仍返回 nil，App.watcher 和 pendingRestore 都是空的。
func TestDisconnectReportsPendingRevertFailure(t *testing.T) {
	a := NewApp(testInvite(t), t.TempDir(), nil)
	a.mu.Lock()
	a.noNetCfg = true
	a.running = true
	a.snapshot = testSnapshot()
	a.mu.Unlock()

	pendingSnap := Snapshot{
		DefaultGateway: "172.16.9.1",
		DefaultIfIndex: 7,
		Interfaces:     []IfaceDNS{{Name: "WLAN", Index: 7, DNS: []string{"172.16.9.1"}}},
	}
	w := &sessionWatcher{
		pending: &pendingMigration{snap: pendingSnap},
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	close(w.done)
	a.mu.Lock()
	a.watcher = w
	a.mu.Unlock()

	restoreStub := stubRevertPending(func(*sessionWatcher) (Snapshot, error) {
		return pendingSnap, errors.New("注入：DNS 回滚失败")
	})
	defer restoreStub()

	err := a.Disconnect()
	if err == nil {
		t.Fatal("迁移回滚失败时 Disconnect 必须报错，不能报「已断开」")
	}
	if !strings.Contains(err.Error(), "DNS 回滚失败") {
		t.Errorf("错误里要带上回滚失败的原因，实际: %v", err)
	}

	p := a.pendingRestore
	if p == nil || p.watch != w {
		t.Fatal("没撤干净的迁移现场必须留给下一次断开重试")
	}

	// 主快照已经还原干净，磁盘上该留的是那张没撤干净的网卡，
	// 下次启动自愈才照着它清。
	got, exists, lerr := LoadSnapshot(a.statePath)
	if lerr != nil || !exists {
		t.Fatalf("全部恢复完成前状态文件不能删: exists=%v err=%v", exists, lerr)
	}
	if got.DefaultIfIndex != pendingSnap.DefaultIfIndex {
		t.Errorf("状态文件里该是待回滚网卡的现场，实际 ifIndex=%d", got.DefaultIfIndex)
	}
}

// 下一次断开要接着把上次没撤干净的迁移撤完，撤完才允许清状态文件。
func TestRetryDisconnectFinishesPendingRevert(t *testing.T) {
	a := NewApp(testInvite(t), t.TempDir(), nil)
	a.mu.Lock()
	a.noNetCfg = true
	a.running = true
	a.snapshot = testSnapshot()
	a.mu.Unlock()

	pendingSnap := Snapshot{DefaultGateway: "172.16.9.1", DefaultIfIndex: 7}
	w := &sessionWatcher{
		pending: &pendingMigration{snap: pendingSnap},
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	close(w.done)
	a.mu.Lock()
	a.watcher = w
	a.mu.Unlock()

	failing := stubRevertPending(func(*sessionWatcher) (Snapshot, error) {
		return pendingSnap, errors.New("注入：DNS 回滚失败")
	})
	if err := a.Disconnect(); err == nil {
		failing()
		t.Fatal("第一次断开应当因为回滚失败而报错")
	}
	failing()

	retried := 0
	success := stubRevertPending(func(*sessionWatcher) (Snapshot, error) {
		retried++
		return Snapshot{}, nil
	})
	defer success()

	if err := a.Disconnect(); err != nil {
		t.Fatalf("补还原成功后不该再报错: %v", err)
	}
	if retried != 1 {
		t.Errorf("待还原路径必须重试回滚，实际调用 %d 次", retried)
	}
	if a.pendingRestore != nil {
		t.Error("全部还原完成后必须清掉待还原状态")
	}
	if _, exists, _ := LoadSnapshot(a.statePath); exists {
		t.Error("全部还原完成后状态文件必须删掉")
	}
}
