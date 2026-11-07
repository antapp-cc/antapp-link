package pki

import (
	"testing"
)

func TestReleaseReview_RejectUnknownInviteMode(t *testing.T) {
	inv := mustIssue(t, mustInit(t), "release-audit")
	inv.Mode = "udpp"
	if err := inv.Validate(); err == nil {
		t.Fatalf("invalid data mode accepted, UseUDP=%v (client will choose TCP)", inv.UseUDP())
	}
}

func TestReleaseReview_InviteModeRoundTrip(t *testing.T) {
	base := mustIssue(t, mustInit(t), "mode-roundtrip")
	for _, tc := range []struct {
		input TunnelMode
		want  TunnelMode
	}{{"", ModeTCP}, {"tcp", ModeTCP}, {" TCP ", ModeTCP}, {"udp", ModeUDP}, {" UDP ", ModeUDP}} {
		inv := base
		inv.Mode = tc.input
		code, err := inv.Encode()
		if err != nil {
			t.Fatal(err)
		}
		got, err := Decode(code)
		if err != nil {
			t.Fatal(err)
		}
		if got.Mode != tc.want || got.UseUDP() != (tc.want == ModeUDP) {
			t.Fatalf("%q became %q", tc.input, got.Mode)
		}
	}
}
