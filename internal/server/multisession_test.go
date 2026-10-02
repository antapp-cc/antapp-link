package server

import (
	"encoding/binary"
	"net"
	"testing"

	"github.com/antapp-cc/antapp-link/internal/proto"
)

// fakeConn 是 slotConn.conn 的最小桩（只需满足 net.Conn 接口给 join/leave 用）。
type fakeConn struct{ net.Conn }

func TestSessionSlotRouting(t *testing.T) {
	sess := &session{sid: "abc", members: 3}
	sess.slots = make([]*slotConn, 3)
	sess.slots[0] = &slotConn{}
	sess.slots[2] = &slotConn{}

	// 槽 1 空 → 落控制连接（槽 0）
	if got := sess.slot(1); got != sess.slots[0] {
		t.Fatal("空槽必须落控制连接")
	}
	// 越界 → 控制连接
	if got := sess.slot(5); got != sess.slots[0] {
		t.Fatal("越界必须落控制连接")
	}
	// 占槽/重复占槽（slotFree 预检 + commitSlot 提交两段式）
	c := &fakeConn{}
	if !sess.slotFree(1) {
		t.Fatal("空槽预检应通过")
	}
	if !sess.commitSlot(1, c) {
		t.Fatal("空槽提交应成功")
	}
	if sess.slotFree(1) {
		t.Fatal("已占槽预检应失败")
	}
	if sess.commitSlot(1, c) {
		t.Fatal("重复提交应失败")
	}
	if sess.slotFree(3) || sess.commitSlot(3, c) {
		t.Fatal("越界槽应失败")
	}
	if got := sess.slot(1); got == sess.slots[0] {
		t.Fatal("提交后槽 1 应可用")
	}
	sess.leave(1, c)
	if got := sess.slot(1); got != sess.slots[0] {
		t.Fatal("leave 后应落回控制连接")
	}
}

func TestSessionRoutingConsistency(t *testing.T) {
	// 同一五元组反复路由必须同槽（正确性基石在会话层的体现）
	sess := &session{members: 4}
	sess.slots = make([]*slotConn, 4)
	sess.slots[0] = &slotConn{}

	pkt := make([]byte, 24)
	pkt[0] = 0x40
	pkt[9] = 6
	binary.BigEndian.PutUint16(pkt[20:22], 5555)
	binary.BigEndian.PutUint16(pkt[22:24], 443)

	want := proto.Slot(pkt, 4)
	for i := 0; i < 50; i++ {
		if got := proto.Slot(pkt, 4); got != want {
			t.Fatalf("第 %d 次路由漂移: %d != %d", i, got, want)
		}
	}
	// N=1 时必须走控制连接
	if proto.Slot(pkt, 1) != 0 {
		t.Fatal("N=1 必须全走控制连接")
	}
}
