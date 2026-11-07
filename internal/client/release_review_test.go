//go:build windows

package client

import (
	"context"
	"crypto/tls"
	"errors"
	"github.com/antapp-cc/antapp-link/internal/pki"
	"github.com/antapp-cc/antapp-link/internal/proto"
	"golang.zx2c4.com/wireguard/windows/tunnel/winipcfg"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReleaseReview_MigrationCrashRetainsOldLedger(t *testing.T) {
	old := testOldExitSnapshot()
	w, _, _ := newMigrationTestWatcher(t, old)
	p := filepath.Join(t.TempDir(), "state.json")
	if err := SaveSnapshot(p, old); err != nil {
		t.Fatal(err)
	}
	w.update = func(s Snapshot) error { return SaveSnapshot(p, s) }
	sweepOwnedSplitFn = func(*splitOwnership) (int, error) { return 0, errors.New("injected old route deletion failure") }
	w.reconcile()
	if w.pending == nil || w.pending.old == nil {
		t.Fatal("fixture did not retain pending cleanup")
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"gateway": "`+old.DefaultGateway+`"`) {
		t.Fatalf("old cleanup exists only in RAM; persisted recovery state lost old route ledger for %s: %s", old.DefaultGateway, "(saved JSON omits old route gateway)")
	}
}
func TestReleaseReview_UDPCancelDuringSendLock(t *testing.T) {
	receiver, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	sender, err := net.DialUDP("udp4", nil, receiver.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	seal, err := proto.NewCipher(make([]byte, 32), 1)
	if err != nil {
		t.Fatal(err)
	}
	tun := testTunnel(t)
	s := newClientSession(tun, newRecordConn(), "audit", nil, Ack{Members: 1})
	defer s.closeAll()
	s.udp = &udpChannel{conn: sender, sendSeal: seal, closed: make(chan struct{})}
	s.udp.sendMu.Lock()
	tun.pktCh <- repairTCPPacket()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.pumpFromDevice(ctx) }()
	limit := time.Now().Add(time.Second)
	for tun.Stats.TxBytes.Load() == 0 && time.Now().Before(limit) {
		time.Sleep(time.Millisecond)
	}
	if tun.Stats.TxBytes.Load() == 0 {
		s.udp.sendMu.Unlock()
		t.Fatal("fixture never dequeued packet")
	}
	time.Sleep(10 * time.Millisecond)
	cancel()
	s.udp.sendMu.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("UDP pump did not exit")
	}
	if tun.Stats.TxWritten.Load() != 0 {
		t.Fatal("canceled UDP counted as successful write")
	}
	_ = receiver.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
	if n, _, err := receiver.ReadFromUDP(make([]byte, 2048)); err == nil {
		t.Fatalf("UDP sent %d bytes after cancellation while waiting for send lock", n)
	}
}
func TestReleaseReview_ControlAckCancellation(t *testing.T) {
	for _, mode := range []pki.TunnelMode{pki.ModeTCP, pki.ModeUDP} {
		t.Run(string(mode), func(t *testing.T) { testControlAckCancellation(t, mode) })
	}
}

func testControlAckCancellation(t *testing.T, mode pki.TunnelMode) {
	dir := t.TempDir()
	if err := pki.Init(dir); err != nil {
		t.Fatal(err)
	}
	srvCfg, err := pki.ServerTLSConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", srvCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	gotHello := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		if _, _, err = proto.ReadFrame(c); err != nil {
			return
		}
		close(gotHello)
		<-release
	}()
	inv, err := pki.Issue(dir, "audit-cancel", ln.Addr().String(), pki.TunnelParams{TunnelIP: "10.10.0.2", Gateway: "10.10.0.1", Prefix: 24, MTU: 1400, Members: 1, Mode: mode})
	if err != nil {
		t.Fatal(err)
	}
	tun := NewTunnel(inv, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _, _ = tun.session(ctx); close(done) }()
	select {
	case <-gotHello:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not receive HELLO")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(300 * time.Millisecond):
		t.Error("control connection remains blocked awaiting HELLO_ACK after context cancellation")
	}
}

func TestReleaseReview_HealRejectsUnreadableSnapshot(t *testing.T) {
	// A directory at the snapshot path deterministically makes ReadFile fail without touching the network.
	oldRestore := restoreSnapshotFn
	restoreCalls := 0
	restoreSnapshotFn = func(Snapshot, NetConfig) error { restoreCalls++; return nil }
	defer func() { restoreSnapshotFn = oldRestore }()
	a := &App{statePath: t.TempDir(), log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := a.HealIfNeeded(); err == nil {
		t.Fatal("snapshot read I/O error was treated as missing state; recovery gate remained open")
	}
	if a.pendingRestore == nil {
		t.Fatal("unreadable snapshot did not set a recovery gate")
	}
	if restoreCalls != 1 {
		t.Fatalf("读不到状态文件时应做一次保守还原，实际 %d 次", restoreCalls)
	}
}

func TestReleaseReview_MigrationRecoveryFromDisk(t *testing.T) {
	old := testOldExitSnapshot()
	w, _, _ := newMigrationTestWatcher(t, old)
	file := filepath.Join(t.TempDir(), "state.json")
	w.update = func(s Snapshot) error { return SaveSnapshot(file, s) }
	sweepOwnedSplitFn = func(*splitOwnership) (int, error) { return 0, errors.New("cleanup failed") }
	w.reconcile()
	saved, exists, err := LoadSnapshot(file)
	if err != nil || !exists {
		t.Fatalf("load: %v %v", exists, err)
	}
	var restored []Snapshot
	err = restoreSnapshots(saved, NetConfig{}, func(s Snapshot, _ NetConfig) error { restored = append(restored, s); return nil })
	if err != nil || len(restored) < 2 {
		t.Fatalf("recovery lost snapshots: %d %v", len(restored), err)
	}
	if restored[0].DefaultGateway != old.DefaultGateway || restored[0].Split == nil || restored[0].Interfaces[0].GUID != old.Interfaces[0].GUID {
		t.Fatalf("old recovery incomplete: %+v", restored[0])
	}
	if restored[len(restored)-1].DefaultGateway != "198.51.100.1" {
		t.Fatal("new exit missing")
	}
}

func TestReleaseReview_MigrationPrewriteFailureLeavesNewExitUntouched(t *testing.T) {
	w, _, _ := newMigrationTestWatcher(t, testOldExitSnapshot())
	changed := false
	setAdapterDNSFn = func(winipcfg.LUID, []string) error { changed = true; return nil }
	addSplitRouteFn = func(winipcfg.LUID, netip.Prefix, netip.Addr) error { changed = true; return nil }
	w.update = func(Snapshot) error { return errors.New("disk full") }
	w.reconcile()
	if changed {
		t.Fatal("new exit changed before recovery state was durable")
	}
}
func TestReleaseReview_UnreadableSnapshotRetryReloads(t *testing.T) {
	oldRestore := restoreSnapshotFn
	restoreCalls := 0
	restoreSnapshotFn = func(Snapshot, NetConfig) error { restoreCalls++; return nil }
	defer func() { restoreSnapshotFn = oldRestore }()
	file := filepath.Join(t.TempDir(), "state.json")
	if err := os.Mkdir(file, 0700); err != nil {
		t.Fatal(err)
	}
	a := &App{statePath: file, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := a.HealIfNeeded(); err == nil || a.pendingRestore == nil || !a.pendingRestore.reload {
		t.Fatal("missing reload gate")
	}
	if a.pendingRestore == nil {
		t.Fatal("recovery gate cleared too early")
	}
	if err := a.Connect(); err == nil || !strings.Contains(err.Error(), "还没有还原干净") {
		t.Fatalf("恢复门禁没有挡住 Connect: %v", err)
	}
	// 现场读不到时，重试断开必须走保守还原并清掉门禁，不能永久卡住用户。
	before := restoreCalls
	if err := a.Disconnect(); err != nil {
		t.Fatalf("unreadable state should fall back to conservative restore: %v", err)
	}
	if restoreCalls == before {
		t.Fatal("重试断开没有做保守还原")
	}
	if a.pendingRestore != nil {
		t.Fatal("gate not cleared after conservative restore")
	}
}

func TestReleaseReview_RestoreAttemptsAllSavedExits(t *testing.T) {
	file := filepath.Join(t.TempDir(), "state.json")
	snap := Snapshot{DefaultGateway: "current", Pending: []Snapshot{{DefaultGateway: "older"}, {DefaultGateway: "old"}}}
	if err := SaveSnapshot(file, snap); err != nil {
		t.Fatal(err)
	}
	saved, _, err := LoadSnapshot(file)
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	err = restoreSnapshots(saved, NetConfig{}, func(s Snapshot, _ NetConfig) error {
		calls = append(calls, s.DefaultGateway)
		if s.DefaultGateway == "older" {
			return errors.New("failed")
		}
		return nil
	})
	if err == nil || strings.Join(calls, ",") != "older,old,current" {
		t.Fatalf("failed recovery skipped exits: %v %v", calls, err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatal("failed recovery removed state")
	}
}
