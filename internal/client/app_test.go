package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHealIfNeededIsNoopWithoutStateFile(t *testing.T) {
	app := NewApp(testInvite(t), t.TempDir(), nil)
	if err := app.HealIfNeeded(); err != nil {
		t.Errorf("没有残留状态时不该报错: %v", err)
	}
}

func TestStatusBeforeConnect(t *testing.T) {
	app := NewApp(testInvite(t), t.TempDir(), nil)
	st := app.Status()
	if st.Running || st.Online {
		t.Error("还没连上就报告已连接")
	}
	if st.Server != "103.143.11.34:62233" {
		t.Errorf("Server = %q", st.Server)
	}
	if st.TunnelIP != "" {
		t.Errorf("没连上时 TunnelIP 该是空的，实际 %q", st.TunnelIP)
	}
}

func TestDisconnectWithoutConnectIsNoop(t *testing.T) {
	app := NewApp(testInvite(t), t.TempDir(), nil)
	if err := app.Disconnect(); err != nil {
		t.Errorf("没连接时断开应当直接返回，实际 %v", err)
	}
}

// 更新交接期间不能再去接管网络：旧版此时已经让出单实例锁（复审第 4 项）。
func TestConnectRejectedWhileUpdating(t *testing.T) {
	app := NewApp(testInvite(t), t.TempDir(), nil)
	app.mu.Lock()
	app.updating = true
	app.mu.Unlock()

	err := app.Connect()
	if err == nil || !strings.Contains(err.Error(), "正在更新") {
		t.Fatalf("更新期间 Connect 必须被拒绝，实际: %v", err)
	}
	if st := app.Status(); !st.Updating {
		t.Fatal("Status 必须报告更新中")
	}
}

func TestStatePathUnderRuntimeDir(t *testing.T) {
	dir := t.TempDir()
	app := NewApp(testInvite(t), dir, nil)
	if got, want := app.RootDir(), dir; got != want {
		t.Errorf("RootDir = %q, want %q", got, want)
	}
	if got, want := StatePath(app.RootDir()), filepath.Join(dir, "data", "state.json"); got != want {
		t.Errorf("StatePath = %q, want %q", got, want)
	}
}

// 三个子目录各司其职，别再把配置和日志混进 data。
func TestDirLayout(t *testing.T) {
	root := `C:\Program Files\AntApp Link`
	cases := []struct{ got, want, what string }{
		{ConfigDir(root), filepath.Join(root, "config"), "ConfigDir"},
		{LogsDir(root), filepath.Join(root, "logs"), "LogsDir"},
		{RuntimeDir(root), filepath.Join(root, "data"), "RuntimeDir"},
		{StatePath(root), filepath.Join(root, "data", "state.json"), "StatePath"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.what, c.got, c.want)
		}
	}
}

func TestOpenLogWritesAndRotates(t *testing.T) {
	dir := t.TempDir()
	l, err := OpenLog(StatePath(dir), 64)
	if err != nil {
		t.Fatalf("OpenLog: %v", err)
	}
	defer l.Close()

	line := []byte("0123456789abcdef0123456789abcdef\n")
	for i := 0; i < 10; i++ {
		if _, err := l.Write(line); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	// 超过上限后应当已轮转出 .1 备份
	if _, err := os.Stat(l.Path() + ".1"); err != nil {
		t.Errorf("超过大小上限后应该轮转出备份文件: %v", err)
	}
}
