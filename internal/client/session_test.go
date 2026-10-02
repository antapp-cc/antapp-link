package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/antapp-cc/antapp-link/internal/pki"
	"github.com/antapp-cc/antapp-link/internal/proto"
)

func testTunnel(t *testing.T) *Tunnel {
	t.Helper()
	return NewTunnel(pki.Invite{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// 老服务端的 HELLO_ACK 没有 members 字段 → Ack.Members = 0。此时必须自动退化成
// 单连接运行：既不能崩，也不能造出 0 长度槽表。
func TestNewClientSessionFallsBackToSingleSlot(t *testing.T) {
	tun := testTunnel(t)
	_, conn := net.Pipe()
	defer conn.Close()

	sess := newClientSession(tun, conn, "sid-1", nil, Ack{})
	if sess.members != 1 || len(sess.slots) != 1 {
		t.Fatalf("老服务端应退化成单连接，实际 members=%d 槽表长 %d", sess.members, len(sess.slots))
	}
	if sess.slots[0] != conn {
		t.Fatal("槽 0 必须是控制连接")
	}
}

// 服务端批准的数量来自外部输入，越界一律夹到 1..4。
func TestNewClientSessionClampsApprovedMembers(t *testing.T) {
	tun := testTunnel(t)
	for _, tc := range []struct{ in, want int }{
		{-3, 1}, {0, 1}, {1, 1}, {2, 2}, {4, 4}, {99, 4},
	} {
		_, conn := net.Pipe()
		sess := newClientSession(tun, conn, "sid", nil, Ack{Members: tc.in})
		if sess.members != tc.want || len(sess.slots) != tc.want {
			t.Errorf("ack.members=%d → members=%d 槽表长 %d，期望 %d",
				tc.in, sess.members, len(sess.slots), tc.want)
		}
		_ = conn.Close()
	}
}

// 连接码里的请求值同样归一到 1..4（0/1=单连接）。
func TestEffectiveMembers(t *testing.T) {
	for _, tc := range []struct{ in, want int }{
		{-1, 1}, {0, 1}, {1, 1}, {3, 3}, {4, 4}, {5, 4},
	} {
		if got := effectiveMembers(tc.in); got != tc.want {
			t.Errorf("effectiveMembers(%d) = %d，期望 %d", tc.in, got, tc.want)
		}
	}
}

// 会话必须持有握手用的 TLS 配置：成员连接要复用它，缓存里的会话票才可能被复用
// （每次拨号各自新建一份配置，等于每次都是空缓存，§5.3 就白加了）。
func TestNewClientSessionKeepsTLSConfig(t *testing.T) {
	cfg := &tls.Config{ClientSessionCache: tls.NewLRUClientSessionCache(8)}
	_, conn := net.Pipe()
	defer conn.Close()

	sess := newClientSession(testTunnel(t), conn, "sid", cfg, Ack{Members: 2})
	if sess.tlsCfg != cfg {
		t.Fatal("会话没有留下握手用的 TLS 配置，成员连接会各自新建一份空缓存")
	}
}

// blockedConn 让**第一个** Write 卡住（后续写者直接通过），用来把"控制连接正在写"
// 这个瞬间拉长成可观测状态；若第二个写者没被锁挡住，它会直接写进去，测试即失败。
type blockedConn struct {
	mu      sync.Mutex
	writes  [][]byte
	claim   chan struct{}
	held    chan struct{}
	entered chan struct{}
}

func newBlockedConn() *blockedConn {
	return &blockedConn{entered: make(chan struct{}, 1)}
}

func (c *blockedConn) block() {
	c.mu.Lock()
	c.claim = make(chan struct{})
	c.mu.Unlock()
}

func (c *blockedConn) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-c.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("控制帧没有进入 Write")
	}
}

func (c *blockedConn) release() {
	c.mu.Lock()
	if c.held != nil {
		close(c.held)
		c.held = nil
	}
	c.mu.Unlock()
}

func (c *blockedConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	gate := c.claim
	if gate != nil {
		c.claim, c.held = nil, gate
	}
	c.mu.Unlock()
	if gate != nil {
		select {
		case c.entered <- struct{}{}:
		default:
		}
		<-gate
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := make([]byte, len(b))
	copy(cp, b)
	c.writes = append(c.writes, cp)
	return len(b), nil
}

func (c *blockedConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (c *blockedConn) Close() error                     { return nil }
func (c *blockedConn) LocalAddr() net.Addr              { return nil }
func (c *blockedConn) RemoteAddr() net.Addr             { return nil }
func (c *blockedConn) SetDeadline(time.Time) error      { return nil }
func (c *blockedConn) SetReadDeadline(time.Time) error  { return nil }
func (c *blockedConn) SetWriteDeadline(time.Time) error { return nil }

// 槽位为空（成员连接断了、正在补拨）时数据落回控制连接。这条路径必须和心跳共用
// 控制连接的写锁：WriteFrame 是"帧头 + 载荷"两次 Write，插在中间的批会把帧撕开。
func TestFlushBatchWaitsForControlWrite(t *testing.T) {
	conn := newBlockedConn()
	sess := newClientSession(testTunnel(t), conn, "sid", nil, Ack{Members: 2})

	conn.block()
	go func() { _ = sess.write(proto.TypePing, make([]byte, 8)) }()
	conn.waitEntered(t)

	done := make(chan struct{})
	pkt := []byte{0x45, 0x00, 0x00, 0x14, 10, 0, 0, 2, 8, 8, 8, 8}
	go func() {
		_ = flushBatch(sess, 1, batchOf(pkt))
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("槽位落回控制连接时绕过了控制连接的写锁：两个帧会交错")
	case <-time.After(200 * time.Millisecond):
	}
	conn.release()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("释放控制连接的写之后，落回的批仍然没写出去")
	}
}

// 会话进入收尾（ctx 已取消）后，槽位连接会随会话一起关，此时的写失败是预期内的
// 竞态；按 ERROR 记会在排障时误导成真故障。
func TestFlushBatchQuietAfterSessionEnd(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	tun := NewTunnel(pki.Invite{}, nil, logger)

	server, peer := net.Pipe()
	_ = peer.Close() // 对端已断，写必然失败
	defer server.Close()

	sess := newClientSession(tun, server, "sid", nil, Ack{Members: 1})
	ctx, cancel := context.WithCancel(context.Background())
	sess.ctx = ctx
	cancel()

	if err := flushBatch(sess, 0, batchOf([]byte{1, 2, 3})); err == nil {
		t.Fatal("往已关闭的连接写应返回错误")
	}
	if strings.Contains(buf.String(), "level=ERROR") {
		t.Errorf("会话收尾时的写失败不该记 ERROR，实际日志：%s", buf.String())
	}
}

// 反例：会话还在运行时写失败必须仍是 ERROR —— 别把告警一起消掉。
func TestFlushBatchStillReportsErrorWhileRunning(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	tun := NewTunnel(pki.Invite{}, nil, logger)

	server, peer := net.Pipe()
	_ = peer.Close()
	defer server.Close()

	sess := newClientSession(tun, server, "sid", nil, Ack{Members: 1})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sess.ctx = ctx // 未取消：会话仍在跑

	if err := flushBatch(sess, 0, batchOf([]byte{1, 2, 3})); err == nil {
		t.Fatal("往已关闭的连接写应返回错误")
	}
	if !strings.Contains(buf.String(), "level=ERROR") {
		t.Errorf("运行中的写失败必须记 ERROR，实际日志：%s", buf.String())
	}
}

// 成员槽写不动时只摘掉那个槽，会话继续跑。
// 读方向已经是这个语义（断开只摘槽 + 补拨），写方向也要对齐 ——
// 一条成员连接出问题不该重启整条隧道。
func TestMemberSlotWriteFailureDropsSlotOnly(t *testing.T) {
	old := slotWriteTimeout
	slotWriteTimeout = 150 * time.Millisecond
	defer func() { slotWriteTimeout = old }()

	tun := testTunnel(t)
	ctrl, ctrlPeer := net.Pipe()
	defer ctrl.Close()
	defer ctrlPeer.Close()
	sess := newClientSession(tun, ctrl, "sid", nil, Ack{Members: 2})

	dead, deadPeer := net.Pipe() // 对端从不读：这条槽写不动
	defer deadPeer.Close()
	sess.slots[1] = dead

	done := make(chan error, 1)
	go func() { done <- flushBatch(sess, 1, batchOf([]byte{1, 2, 3})) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("成员槽写失败不该结束会话，实际返回 %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("flushBatch 卡住了：成员槽的写必须有超时")
	}
	if sess.slots[1] != nil {
		t.Error("写失败的成员槽应被摘掉，让流量落回控制连接")
	}
}

// 反例：控制连接（槽 0）写失败意味着会话本身断了，必须结束会话让上层重连。
func TestControlSlotWriteFailureEndsSession(t *testing.T) {
	old := slotWriteTimeout
	slotWriteTimeout = 150 * time.Millisecond
	defer func() { slotWriteTimeout = old }()

	tun := testTunnel(t)
	dead, deadPeer := net.Pipe() // 控制连接写不动
	defer deadPeer.Close()
	sess := newClientSession(tun, dead, "sid", nil, Ack{Members: 1})

	done := make(chan error, 1)
	go func() { done <- flushBatch(sess, 0, batchOf([]byte{1, 2, 3})) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("控制连接写失败必须返回错误，让会话重连")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("flushBatch 卡住了：控制连接的写也必须有超时")
	}
}

// batchOf 按写合并的拼法造一批：[hdr|pkt][hdr|pkt]…（帧头已含在内）。
func batchOf(pkts ...[]byte) []byte {
	var batch []byte
	for _, p := range pkts {
		batch = proto.AppendHeader(batch, proto.TypeIP, len(p))
		batch = append(batch, p...)
	}
	return batch
}
