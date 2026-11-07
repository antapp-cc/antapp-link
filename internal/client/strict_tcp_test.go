package client

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

type strictHeartbeatFailConn struct {
	net.Conn
	deadlineError bool
}

func (c *strictHeartbeatFailConn) Write([]byte) (int, error) {
	return 0, errors.New("injected heartbeat write failure")
}
func (c *strictHeartbeatFailConn) SetWriteDeadline(time.Time) error {
	if c.deadlineError {
		return errors.New("injected deadline failure")
	}
	return nil
}

func TestStrictReview_HeartbeatWriteFailureDropsMember(t *testing.T) {
	oldInterval := memberPingInterval
	memberPingInterval = 5 * time.Millisecond
	defer func() { memberPingInterval = oldInterval }()
	for _, deadlineError := range []bool{false, true} {
		t.Run(map[bool]string{false: "write", true: "deadline"}[deadlineError], func(t *testing.T) {
			left, right := net.Pipe()
			defer left.Close()
			defer right.Close()
			conn := &strictHeartbeatFailConn{Conn: left, deadlineError: deadlineError}
			sess := newClientSession(testTunnel(t), newRecordConn(), "sid", nil, Ack{Members: 2})
			defer sess.closeAll()
			if !sess.attachSlot(1, conn) {
				t.Fatal("attach")
			}
			readDone := make(chan error, 1)
			go func() { _, err := conn.Read(make([]byte, 1)); readDone <- err }()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() { defer close(done); sess.memberHeartbeat(ctx, 1, conn) }()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("heartbeat failed to exit")
			}
			if sess.slotConn(1) != nil {
				t.Fatal("heartbeat write failure left dead member eligible for business traffic")
			}
			select {
			case err := <-readDone:
				if err == nil {
					t.Fatal("expected closed reader")
				}
			case <-time.After(time.Second):
				t.Fatal("member reader still blocked; redial cannot start")
			}
		})
	}
}
