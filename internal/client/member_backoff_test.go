package client

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// stubMemberDial 把成员拨号换成桩：每次调用计数，并返回 up 作为「在槽里待了多久」。
func stubMemberDial(t *testing.T, up time.Duration) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	orig := memberDial
	memberDial = func(ctx context.Context, _ *Tunnel, _ *clientSession, _ int) (time.Duration, error) {
		calls.Add(1)
		time.Sleep(time.Millisecond)
		return up, errors.New("连接断开")
	}
	t.Cleanup(func() { memberDial = orig })
	return &calls
}

// 稳定用过一阵才断开的成员必须立即补拨：原来读循环的「正常断开」也被当成失败，
// 退避每次加一档，断几次之后每次补拨都要白等十几秒。
func TestMaintainMembersResetsBackoffAfterStableMember(t *testing.T) {
	calls := stubMemberDial(t, time.Minute)
	tun := testTunnel(t)
	sess := newClientSession(tun, newRecordConn(), "sid", nil, Ack{Members: 2})

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	maintainMembers(ctx, tun, sess)

	if n := calls.Load(); n < 5 {
		t.Fatalf("稳定成员断开后应立即补拨，300ms 内只拨了 %d 次（退避没有清零）", n)
	}
}

// 刚入槽就断的成员仍要退避，否则一条反复闪断的连接会变成 TLS 握手风暴。
func TestMaintainMembersBacksOffFlappingMember(t *testing.T) {
	calls := stubMemberDial(t, time.Second)
	tun := testTunnel(t)
	sess := newClientSession(tun, newRecordConn(), "sid", nil, Ack{Members: 2})

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	maintainMembers(ctx, tun, sess)

	if n := calls.Load(); n != 1 {
		t.Fatalf("闪断成员第二次补拨前应退避 1 秒，300ms 内却拨了 %d 次", n)
	}
}

// 新会话开跑前要丢掉断线期间积压的旧包，不能拿它们先把槽队列占满。
func TestDrainPendingDropsStalePackets(t *testing.T) {
	tun := testTunnel(t)
	for i := 0; i < 5; i++ {
		tun.pktCh <- []byte{0x45}
	}
	if n := tun.drainPending(); n != 5 {
		t.Fatalf("应丢弃 5 个积压包，实际 %d", n)
	}
	if n := len(tun.pktCh); n != 0 {
		t.Fatalf("清空后 pktCh 仍有 %d 个包", n)
	}
	if n := tun.drainPending(); n != 0 {
		t.Fatalf("空队列应返回 0，实际 %d", n)
	}
}
