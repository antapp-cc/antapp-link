//go:build windows

package client

import (
	"errors"
	"net/netip"
	"testing"

	"golang.zx2c4.com/wireguard/windows/tunnel/winipcfg"
)

func TestStrictReview_ReturnToPreviousExitResolvesPending(t *testing.T) {
	old := testOldExitSnapshot()
	w, _, _ := newMigrationTestWatcher(t, old)
	commits := 0
	w.update = func(Snapshot) error {
		commits++
		if commits == 2 {
			return errors.New("injected final save failure")
		}
		return nil
	}
	dnsWrites := 0
	setAdapterDNSFn = func(winipcfg.LUID, []string) error { dnsWrites++; return nil }
	w.reconcile()
	if w.pending == nil {
		t.Fatal("expected pending B after failed commit")
	}
	previousPending := w.pending.snap.Split
	cleanedPending := false
	sweepOwnedSplitFn = func(own *splitOwnership) (int, error) {
		if own == previousPending {
			cleanedPending = true
		}
		return 1, nil
	}
	bestDefaultRouteFn = func(winipcfg.LUID) (netip.Addr, uint32, error) {
		return netip.MustParseAddr(old.DefaultGateway), uint32(old.DefaultIfIndex), nil
	}
	w.reconcile()
	if w.pending != nil || !cleanedPending || dnsWrites != 2 {
		t.Fatalf("return to A left B pending or failed to reapply A: pending=%v cleaned=%v dnsWrites=%d", w.pending != nil, cleanedPending, dnsWrites)
	}
	if w.snap.DefaultGateway != old.DefaultGateway {
		t.Fatalf("wrong committed exit: %s", w.snap.DefaultGateway)
	}
}
