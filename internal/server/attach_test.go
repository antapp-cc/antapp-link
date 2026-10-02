package server

import "testing"

// 同 sid 的第二条控制连接必须拒绝：一个逻辑会话只能有一个"老大"，否则旧控制连接
// 被踢掉会让整条隧道的成员连接一起陪葬。不同 sid 是换机接替（沿用踢旧接新），
// 老客户端没有 sid 也要保持原行为。
func TestSameControlSession(t *testing.T) {
	if sameControlSession(nil, "sid-1") {
		t.Fatal("当前没有会话时不该拒")
	}
	cur := &session{sid: "sid-1"}
	if !sameControlSession(cur, "sid-1") {
		t.Fatal("同 sid 的第二条控制连接必须拒绝")
	}
	if sameControlSession(cur, "sid-2") {
		t.Fatal("不同 sid 是换机接替，应踢旧接新")
	}
	if sameControlSession(&session{}, "") {
		t.Fatal("老客户端不带 sid，必须保持踢旧接新")
	}
	if sameControlSession(cur, "") {
		t.Fatal("新会话带 sid、新连接不带，不是同一会话")
	}
}
