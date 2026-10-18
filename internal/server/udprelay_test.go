package server

import (
	"log/slog"
	"net"
	"testing"

	"github.com/antapp-cc/antapp-link/internal/proto"
)

func newTestMux() *udpMux {
	return &udpMux{
		sessions: make(map[string]*udpSession),
		log:      slog.Default(),
	}
}

func newTestSess(t *testing.T, ip string, port int) *udpSession {
	t.Helper()
	key := make([]byte, 32)
	recv, err := proto.NewCipher(key, 1)
	if err != nil {
		t.Fatalf("recv cipher: %v", err)
	}
	send, err := proto.NewCipher(key, 2)
	if err != nil {
		t.Fatalf("send cipher: %v", err)
	}
	s := &udpSession{recv: recv, sendSeal: send}
	s.peer.Store(&net.UDPAddr{IP: net.ParseIP(ip), Port: port})
	return s
}

// 重连时旧会话可能晚于新会话 detach，不能把新会话的映射删掉。
func TestRemoveSessionKeepsNewerSession(t *testing.T) {
	m := newTestMux()
	oldSess := newTestSess(t, "1.2.3.4", 1111)
	newer := newTestSess(t, "1.2.3.4", 2222)

	m.sessions["1.2.3.4"] = newer // 新会话已注册

	m.removeSession(oldSess) // 旧会话延迟 detach

	if m.sessions["1.2.3.4"] != newer {
		t.Fatal("旧会话 detach 把新会话的映射删掉了")
	}
}

// 同一个对象重复 remove 要幂等，且不影响别的会话。
func TestRemoveSessionIdempotent(t *testing.T) {
	m := newTestMux()
	s := newTestSess(t, "1.2.3.4", 1111)
	m.sessions["1.2.3.4"] = s

	m.removeSession(s)
	m.removeSession(s)

	if len(m.sessions) != 0 {
		t.Fatalf("期望索引为空，实际 %d 条", len(m.sessions))
	}
}

// lookup 只能按 IP 找候选，不能改端口 —— 未认证的包不能影响下行目标。
func TestLookupDoesNotChangePeer(t *testing.T) {
	m := newTestMux()
	s := newTestSess(t, "1.2.3.4", 1111)
	m.sessions["1.2.3.4"] = s

	got := m.lookup(&net.UDPAddr{IP: net.ParseIP("1.2.3.4"), Port: 9999})
	if got != s {
		t.Fatal("应该按 IP 命中会话")
	}
	if p := s.peer.Load(); p.Port != 1111 {
		t.Fatalf("lookup 不该改端口，实际变成 %d", p.Port)
	}

	// 只有认证通过后才允许更新（模拟 readLoop 的处理顺序）
	m.confirmPeer(s, &net.UDPAddr{IP: net.ParseIP("1.2.3.4"), Port: 9999})
	if p := s.peer.Load(); p.Port != 9999 {
		t.Fatalf("confirmPeer 后端口应为 9999，实际 %d", p.Port)
	}
}

// 不同 IP 不互相命中（按 IP 收敛没有歧义，但也别串台）。
func TestLookupSeparatesIPs(t *testing.T) {
	m := newTestMux()
	s := newTestSess(t, "1.2.3.4", 1111)
	m.sessions["1.2.3.4"] = s

	if got := m.lookup(&net.UDPAddr{IP: net.ParseIP("5.6.7.8"), Port: 1111}); got != nil {
		t.Fatal("不同 IP 不该命中同一个会话")
	}
}
