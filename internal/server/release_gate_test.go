package server

import (
	"context"
	"crypto/tls"
	"github.com/antapp-cc/antapp-link/internal/proto"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

type releaseGateTun struct {
	entered chan struct{}
	resume  chan struct{}
	second  chan []byte
	calls   atomic.Int64
}

func (d *releaseGateTun) Read([]byte) (int, error)    { return 0, io.EOF }
func (d *releaseGateTun) TryRead([]byte) (int, error) { return 0, io.EOF }
func (d *releaseGateTun) Write(p []byte) (int, error) {
	if d.calls.Add(1) == 1 {
		close(d.entered)
		<-d.resume
	} else {
		d.second <- append([]byte(nil), p...)
	}
	return len(p), nil
}

func TestReleaseGateClosedControlCannotDeliverBufferedFrame(t *testing.T) {
	for _, state := range []string{"closed", "replaced-before-close", "canceled"} {
		t.Run(state, func(t *testing.T) { releaseGateControlFrames(t, state) })
	}
}

func releaseGateControlFrames(t *testing.T, state string) {
	s, clientCfg := newTestServer(t, newHandshakeLimiter(2))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	rawClient, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	rawServer, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	sc, cc := tls.Server(rawServer, s.tlsCfg), tls.Client(rawClient, clientCfg)
	defer sc.Close()
	defer cc.Close()
	sc.SetDeadline(time.Now().Add(5 * time.Second))
	cc.SetDeadline(time.Now().Add(5 * time.Second))
	hs := make(chan error, 1)
	go func() { hs <- cc.Handshake() }()
	if err := sc.Handshake(); err != nil {
		t.Fatal(err)
	}
	if err := <-hs; err != nil {
		t.Fatal(err)
	}
	sc.SetDeadline(time.Time{})
	cc.SetDeadline(time.Time{})
	tun := &releaseGateTun{entered: make(chan struct{}), resume: make(chan struct{}), second: make(chan []byte, 2)}
	s.tun = tun
	sess := &session{name: "old", sid: "old", members: 1, closed: make(chan struct{}), slots: []*slotConn{newSlotConn(sc)}}
	s.attach(sess)
	defer sess.close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); s.serveSession(ctx, sess) }()
	first, second := []byte("first-before-close"), []byte("second-buffered-before-close")
	batch := proto.AppendHeader(nil, proto.TypeIP, len(first))
	batch = append(batch, first...)
	batch = proto.AppendHeader(batch, proto.TypeIP, len(second))
	batch = append(batch, second...)
	if _, err := cc.Write(batch); err != nil {
		t.Fatal(err)
	}
	select {
	case <-tun.entered:
	case <-time.After(time.Second):
		t.Fatal("未进入首包的受控 TUN 写入")
	}
	go io.Copy(io.Discard, cc)
	next := &session{name: "new", sid: "new", closed: make(chan struct{})}
	switch state {
	case "closed":
		s.attach(next)
	case "replaced-before-close":
		s.mu.Lock()
		s.current = next
		s.mu.Unlock()
	case "canceled":
		cancel()
	}
	defer next.close()
	close(tun.resume)
	select {
	case p := <-tun.second:
		t.Fatalf("旧会话已关闭且被新会话取代，仍写入第二帧: %q", p)
	case <-time.After(200 * time.Millisecond):
	}
	<-done
}

func TestReleaseGateClosedMemberCannotDeliverBufferedFrame(t *testing.T) {
	s, clientCfg := newTestServer(t, newHandshakeLimiter(2))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	rawClient, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	rawServer, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	sc, cc := tls.Server(rawServer, s.tlsCfg), tls.Client(rawClient, clientCfg)
	defer sc.Close()
	defer cc.Close()
	sc.SetDeadline(time.Now().Add(5 * time.Second))
	cc.SetDeadline(time.Now().Add(5 * time.Second))
	hs := make(chan error, 1)
	go func() { hs <- cc.Handshake() }()
	if err := sc.Handshake(); err != nil {
		t.Fatal(err)
	}
	if err := <-hs; err != nil {
		t.Fatal(err)
	}
	sc.SetDeadline(time.Time{})
	cc.SetDeadline(time.Time{})
	tun := &releaseGateTun{entered: make(chan struct{}), resume: make(chan struct{}), second: make(chan []byte, 2)}
	s.tun = tun
	control, controlPeer := net.Pipe()
	defer control.Close()
	defer controlPeer.Close()
	sess := &session{name: "old", sid: "old", members: 2, closed: make(chan struct{}), slots: []*slotConn{newSlotConn(control), nil}}
	s.attach(sess)
	defer sess.close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); s.handleMember(ctx, sc, "old", helloPayload{Sid: "old", Member: 1}) }()
	for i := 0; i < 2; i++ {
		typ, _, err := proto.ReadFrame(cc)
		if err != nil || typ != proto.TypeMemberAck {
			t.Fatalf("member ACK: %v %v", typ, err)
		}
	}
	first, second := []byte("first-before-close"), []byte("second-buffered-before-close")
	batch := proto.AppendHeader(nil, proto.TypeIP, len(first))
	batch = append(batch, first...)
	batch = proto.AppendHeader(batch, proto.TypeIP, len(second))
	batch = append(batch, second...)
	if _, err := cc.Write(batch); err != nil {
		t.Fatal(err)
	}
	select {
	case <-tun.entered:
	case <-time.After(time.Second):
		t.Fatal("未进入首包的受控 TUN 写入")
	}
	go io.Copy(io.Discard, cc)
	next := &session{name: "new", sid: "new", closed: make(chan struct{})}
	s.attach(next)
	defer next.close()
	close(tun.resume)
	select {
	case p := <-tun.second:
		t.Fatalf("旧会话已关闭且被新会话取代，仍写入第二帧: %q", p)
	case <-time.After(200 * time.Millisecond):
	}
	<-done
}
