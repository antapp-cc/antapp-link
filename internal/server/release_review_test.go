package server

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"
)

func TestReleaseReview_ConfigModeIsCanonical(t *testing.T) {
	c := Default()
	c.Mode = " UDP "
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Mode != "udp" {
		t.Fatalf("accepted mode %q will not enable Run's UDP listener (requires exact udp)", c.Mode)
	}
}
func TestReleaseReview_UDPForwardPreservesLargeDatagram(t *testing.T) {
	target, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	f, err := newUDPForward(0, target.LocalAddr().(*net.UDPAddr).Port, net.IPv4(127, 0, 0, 1), discardLogger(), func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer f.close()
	go f.run(ctx)
	sender, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: f.listen.LocalAddr().(*net.UDPAddr).Port})
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()

	for _, size := range []int{1, 1600, 1601, 4096, 65507} {
		payload := bytes.Repeat([]byte{byte(size)}, size)
		if _, err := sender.Write(payload); err != nil {
			t.Fatal(err)
		}
		_ = target.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 65535)
		n, from, err := target.ReadFromUDP(buf)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(buf[:n], payload) {
			t.Fatalf("forward size %d became %d", size, n)
		}
		if _, err := target.WriteToUDP(payload, from); err != nil {
			t.Fatal(err)
		}
		_ = sender.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, err = sender.Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(buf[:n], payload) {
			t.Fatalf("reverse size %d became %d", size, n)
		}
	}
}

func TestReleaseReview_NetfilterStateAndCommentNormalization(t *testing.T) {
	expected := Rule{Chain: "FORWARD", Args: []string{"-d", "10.10.0.0/24", "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT", "-m", "comment", "--comment", "AntAppLink"}}
	actual := Rule{Chain: "FORWARD", Args: []string{"-d", "10.10.0.0/24", "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-m", "comment", "--comment", "\"AntAppLink\"", "-j", "ACCEPT"}}
	if !sameRule(expected, actual, false) {
		t.Fatal("kernel normalization broke owned rule match")
	}
	actual.Args[9] = "OtherVPN"
	if sameRule(expected, actual, false) {
		t.Fatal("other owner matched")
	}
}
