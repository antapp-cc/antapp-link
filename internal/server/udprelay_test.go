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

// UDP 下行的发送字节要记在会话上：UDP 模式没有槽位状态可看，这是确认数据面
// 真的在动的唯一依据。
func TestUDPSessionCountsTxBytes(t *testing.T) {
	srv, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("监听: %v", err)
	}
	defer srv.Close()

	m := &udpMux{conn: srv, sessions: make(map[string]*udpSession), log: slog.Default()}
	sess := newTestSess(t, "127.0.0.1", srv.LocalAddr().(*net.UDPAddr).Port)

	if err := sess.send(m, []byte{1, 2, 3}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if got := sess.txBytes.Load(); got == 0 {
		t.Fatal("发送成功后 txBytes 应增长")
	}
}

// 同 IP 快速重连时，旧会话读协程的迟到登记不能顶掉新会话的条目：
// map 被顶掉后旧会话 detach 会按守卫把它删掉，新会话的上行就全丢了。
func TestAllowUDPReplace(t *testing.T) {
	oldSess := &session{closed: make(chan struct{})}
	newSess := &session{closed: make(chan struct{})}

	if allowUDPReplace(&udpSession{owner: newSess}, oldSess) {
		t.Fatal("同 IP 已有活会话时不该允许旧会话覆盖")
	}
	close(oldSess.closed)
	if !allowUDPReplace(&udpSession{owner: oldSess}, newSess) {
		t.Fatal("旧会话已收尾时应允许新会话登记")
	}
	if !allowUDPReplace(nil, newSess) {
		t.Fatal("空条目应允许登记")
	}
	if !allowUDPReplace(&udpSession{}, newSess) {
		t.Fatal("没有 owner 的旧条目应允许登记")
	}
	if !allowUDPReplace(&udpSession{owner: newSess}, newSess) {
		t.Fatal("同一会话重复登记（不应发生）不该被拦")
	}
}

// 新会话登记前必须摘掉同 IP 的旧条目：旧 owner 还活着时 allowUDPReplace 会拒绝
// 新会话，随后旧会话 detach 删掉旧条目，map 就空了 —— 新会话上行全丢。
func TestDropOtherSessions(t *testing.T) {
	m := newTestMux()
	ip := net.IPv4(127, 0, 0, 1)
	oldOwner := &session{closed: make(chan struct{})}
	old := &udpSession{owner: oldOwner}
	old.peer.Store(&net.UDPAddr{IP: ip, Port: 1})
	m.sessions[ip.String()] = old

	keep := &session{closed: make(chan struct{})}
	m.dropOtherSessions(ip, keep)
	if _, ok := m.sessions[ip.String()]; ok {
		t.Fatal("不属于 keep 的旧条目应被摘掉")
	}

	own := &udpSession{owner: keep}
	own.peer.Store(&net.UDPAddr{IP: ip, Port: 2})
	m.sessions[ip.String()] = own
	m.dropOtherSessions(ip, keep)
	if m.sessions[ip.String()] != own {
		t.Fatal("keep 自己的条目不该被删")
	}
}
