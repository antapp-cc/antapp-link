package client

import (
	"os"
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

func TestStatePathUnderDataDir(t *testing.T) {
	dir := t.TempDir()
	app := NewApp(testInvite(t), dir, nil)
	if got, want := app.DataDir(), dir; got != want {
		t.Errorf("DataDir = %q, want %q", got, want)
	}
	if got := StatePath(app.DataDir()); got == "" {
		t.Error("状态文件路径不该为空")
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
