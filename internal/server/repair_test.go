package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/antapp-cc/antapp-link/internal/pki"
	"github.com/antapp-cc/antapp-link/internal/proto"
)

func TestSlotQueueCapacityIsSixteen(t *testing.T) {
	if slotQueueLen != 16 {
		t.Fatalf("server slot queue depth = %d, want 16", slotQueueLen)
	}
}

func TestServerPendingBytesTrackWriteLifecycle(t *testing.T) {
	serverConn, peer := net.Pipe()
	defer serverConn.Close()
	defer peer.Close()
	s := newSlotConn(serverConn)
	defer s.closeWrite()
	s.enqueue([]byte{1, 2, 3})
	deadline := time.Now().Add(2 * time.Second)
	for len(s.out) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := s.pendingBytes.Load(); got != 3 {
		t.Fatalf("in-flight write bytes = %d, want 3", got)
	}
	s.enqueue([]byte{4, 5})
	if got := s.pendingBytes.Load(); got != 5 {
		t.Fatalf("pending bytes behind blocked write = %d, want 5", got)
	}
	go func() { _, _ = io.Copy(io.Discard, peer) }()
	deadline = time.Now().Add(2 * time.Second)
	for s.pendingBytes.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := s.pendingBytes.Load(); got != 0 {
		t.Fatalf("pending bytes after writes complete = %d, want 0", got)
	}
}

func TestServerQueueFullDropsWithoutGrowingPendingBytes(t *testing.T) {
	s := &slotConn{out: make(chan []byte, 1), closed: make(chan struct{})}
	s.enqueue([]byte{1, 2, 3})
	s.enqueue([]byte{4, 5})
	if got := s.drops.Load(); got != 1 {
		t.Fatalf("dropped batches = %d, want 1", got)
	}
	if got := s.pendingBytes.Load(); got != 3 {
		t.Fatalf("pending bytes after rejected batch = %d, want 3", got)
	}
}

func repairServerTCPPacket() []byte {
	p := make([]byte, 24)
	p[0], p[9] = 0x45, 6
	copy(p[12:16], []byte{10, 0, 0, 2})
	copy(p[16:20], []byte{8, 8, 8, 8})
	binary.BigEndian.PutUint16(p[20:22], 45000)
	binary.BigEndian.PutUint16(p[22:24], 443)
	return p
}
func TestServerNewFlowAvoidsFullSmallBatchQueue(t *testing.T) {
	sess := &session{members: 3, slots: make([]*slotConn, 3), flow: proto.NewFlowTable()}
	for k := range sess.slots {
		sess.slots[k] = &slotConn{out: make(chan []byte, slotQueueLen), closed: make(chan struct{})}
	}
	for i := 0; i < slotQueueLen; i++ {
		sess.slots[1].enqueue(make([]byte, 64))
	}
	sess.slots[2].enqueue(make([]byte, 4096))
	p := repairServerTCPPacket()
	k := sess.flowSlot(p)
	if k != 2 {
		t.Fatalf("满队列仍被选中: slot=%d", k)
	}
	sess.flushBatch(k, p)
	if sess.slots[k].drops.Load() != 0 {
		t.Fatal("有空位却丢弃新流")
	}
	for len(sess.slots[2].out) < cap(sess.slots[2].out) {
		sess.slots[2].enqueue(make([]byte, 64))
	}
	if got := sess.flowSlot(p); got != k {
		t.Fatalf("队列满导致已有流迁移: %d -> %d", k, got)
	}
}

func TestSingleMemberStatusShowsSlotPressure(t *testing.T) {
	for _, n := range []int{1, 4} {
		var out bytes.Buffer
		writeSlotStatus(&out, Status{Members: n, LiveMembers: n, Slots: []SlotStatus{{Slot: 0, QueueDepth: 3, PendingBytes: 4096, Drops: 7}}})
		for _, want := range []string{"槽 0", "队列 3 批", "待写/写中 4096 字节", "丢批 7"} {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("members=%d 缺少 %q: %s", n, want, out.String())
			}
		}
	}
}
func TestServerUDPDownlinkNeverFallsBack(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		only, registered, want bool
	}{
		{"tcp", false, false, false}, {"udp_waiting", true, false, true}, {"udp_mux_missing", true, true, true}, {"legacy_udp_registered", false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{log: discardLogger()}
			sess := &session{}
			sess.udpOnly.Store(tc.only)
			if tc.registered {
				sess.udp = &udpSession{}
			}
			if got := s.sendUDP(sess, repairServerTCPPacket()); got != tc.want {
				t.Fatalf("UDP 数据面误入 TCP 分支: handled=%v want=%v", got, tc.want)
			}
		})
	}
}
func TestControlHandshakeDataModeCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mode    pki.TunnelMode
		enabled bool
		want    proto.Type
		members int
	}{
		{"legacy_tcp_on_udp_server", "", true, proto.TypeHelloAck, 4},
		{"tcp_on_udp_server", pki.ModeTCP, true, proto.TypeHelloAck, 4},
		{"udp_pending_registration", pki.ModeUDP, true, proto.TypeHelloAck, 1},
		{"udp_unavailable", pki.ModeUDP, false, proto.TypeBye, 0},
		{"unknown_mode", "unknown", true, proto.TypeBye, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, clientCfg := newTestServer(t, newHandshakeLimiter(2))
			if tc.enabled {
				mux, err := newUDPMux("127.0.0.1:0", nil, discardLogger())
				if err != nil {
					t.Fatal(err)
				}
				s.udp = mux
				t.Cleanup(mux.close)
			}
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() {
				raw, err := ln.Accept()
				if err == nil {
					s.serveConn(ctx, raw)
				}
			}()
			conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 2 * time.Second}, "tcp", ln.Addr().String(), clientCfg)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
			hello, _ := json.Marshal(map[string]any{"version": protoVersion, "sid": "repair-mode", "members": 4, "mode": tc.mode})
			if err := proto.WriteFrame(conn, proto.TypeHello, hello); err != nil {
				t.Fatal(err)
			}
			typ, payload, err := proto.ReadFrame(conn)
			if err != nil {
				t.Fatal(err)
			}
			if typ != tc.want {
				t.Fatalf("模式 %q 响应 %v: %s", tc.mode, typ, payload)
			}
			if typ == proto.TypeBye {
				return
			}
			var ack helloAckPayload
			if err := json.Unmarshal(payload, &ack); err != nil {
				t.Fatal(err)
			}
			if ack.Members != tc.members {
				t.Fatalf("模式 %q members=%d want=%d", tc.mode, ack.Members, tc.members)
			}
			waitFor(t, time.Second, func() bool { return s.currentSession() != nil }, "ACK 后会话未登记")
			sess := s.currentSession()
			if sess == nil {
				t.Fatal("ACK 后会话未登记")
			}
			defer s.detach(sess)
			if got := sess.udpOnly.Load(); got != (tc.mode == pki.ModeUDP) {
				t.Fatalf("模式未在 UDP 登记前锁定: %v", got)
			}
			if tc.mode == pki.ModeUDP {
				if err := proto.WriteFrame(conn, proto.TypePing, []byte("ping")); err != nil {
					t.Fatal(err)
				}
				if typ, _, err := proto.ReadFrame(conn); err != nil || typ != proto.TypePong {
					t.Fatalf("UDP 控制心跳异常: %v %v", typ, err)
				}
				if err := proto.WriteFrame(conn, proto.TypeIP, repairServerTCPPacket()); err != nil {
					t.Fatal(err)
				}
				if _, _, err := proto.ReadFrame(conn); err == nil {
					t.Fatal("UDP 会话接受了 TCP 业务数据")
				}
				waitFor(t, time.Second, func() bool { return !sessionAlive(sess) }, "拒收 TCP 业务数据后未关闭 UDP 会话")
			}
		})
	}
}

func TestServerUDPDownlinkUsesSocketAndKeepsModeOnWriteFailure(t *testing.T) {
	peer, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	key := bytes.Repeat([]byte{9}, 32)
	seal, err := proto.NewCipher(key, 1)
	if err != nil {
		t.Fatal(err)
	}
	open, err := proto.NewCipher(key, 1)
	if err != nil {
		t.Fatal(err)
	}
	u := &udpSession{sendSeal: seal}
	u.peer.Store(peer.LocalAddr().(*net.UDPAddr))
	sess := &session{udp: u}
	sess.udpOnly.Store(true)
	s := &Server{udp: &udpMux{conn: conn}, log: discardLogger()}
	packet := repairServerTCPPacket()
	if !s.sendUDP(sess, packet) {
		t.Fatal("已登记 UDP 包进入 TCP 分支")
	}
	_ = peer.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 2048)
	n, _, err := peer.ReadFromUDP(buf)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := open.Open(buf[:n])
	if err != nil || !bytes.Equal(plain, packet) {
		t.Fatalf("UDP 下行业务不一致: %x %v", plain, err)
	}
	before := u.txBytes.Load()
	_ = conn.Close()
	if !s.sendUDP(sess, packet) {
		t.Fatal("UDP 写失败后改走 TCP")
	}
	if got := u.txBytes.Load(); got != before {
		t.Fatalf("失败发送被计入成功字节: %d -> %d", before, got)
	}
}

func BenchmarkServerFlowSlotExisting(b *testing.B) {
	packet := repairServerTCPPacket()
	s := &session{members: 4, slots: make([]*slotConn, 4), flow: proto.NewFlowTable()}
	for k := range s.slots {
		s.slots[k] = &slotConn{out: make(chan []byte, slotQueueLen)}
	}
	s.flowSlot(packet)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.flowSlot(packet)
	}
}

func TestUDPSessionCannotQueueTCPBatch(t *testing.T) {
	slot := &slotConn{out: make(chan []byte, slotQueueLen), closed: make(chan struct{})}
	sess := &session{members: 1, slots: []*slotConn{slot}}
	sess.udpOnly.Store(true)
	sess.flushBatch(0, repairServerTCPPacket())
	if len(slot.out) != 0 || slot.pendingBytes.Load() != 0 {
		t.Fatal("UDP 会话仍把业务批次放入 TCP 队列")
	}
}
func TestLegacyUDPReportKeepsModeWhenRegistrationFails(t *testing.T) {
	s := &Server{log: discardLogger()}
	sess := &session{}
	s.registerUDP(sess, []byte{0x30, 0x39})
	if !sess.udpOnly.Load() || !s.sendUDP(sess, repairServerTCPPacket()) {
		t.Fatal("旧客户端 UDP 登记失败后恢复了 TCP 数据分支")
	}
}

type lifecycleTun struct {
	read    func([]byte) (int, error)
	tryRead func([]byte) (int, error)
}

func (t *lifecycleTun) Read(p []byte) (int, error)    { return t.read(p) }
func (t *lifecycleTun) TryRead(p []byte) (int, error) { return t.tryRead(p) }
func (*lifecycleTun) Write(p []byte) (int, error)     { return len(p), nil }
func lifecycleServerSession() *session {
	s := &session{members: 3, closed: make(chan struct{}), slots: make([]*slotConn, 3), flow: proto.NewFlowTable()}
	for k := range s.slots {
		s.slots[k] = &slotConn{out: make(chan []byte, slotQueueLen), closed: make(chan struct{})}
	}
	return s
}
func lifecyclePackets() [][]byte {
	p := repairServerTCPPacket()
	q := append([]byte(nil), p...)
	binary.BigEndian.PutUint16(q[20:22], 45001)
	return [][]byte{p, q}
}
func TestServerMixedFlowHarvestHasTotalBudget(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sess := lifecycleServerSession()
	packets := lifecyclePackets()
	for _, p := range packets {
		sess.flowSlot(p)
	}
	reads, tries := 0, 0
	s := &Server{current: sess, log: discardLogger(), fatal: make(chan error, 1)}
	s.tun = &lifecycleTun{read: func(buf []byte) (int, error) {
		reads++
		if reads > 1 {
			cancel()
			return 0, ctx.Err()
		}
		return copy(buf, packets[0]), nil
	}, tryRead: func(buf []byte) (int, error) {
		tries++
		if tries >= 2000 {
			return 0, nil
		}
		return copy(buf, packets[tries%2]), nil
	}}
	s.pumpTun(ctx)
	maxPackets := proto.MaxBatchBytes/(proto.HeaderSize+len(packets[0])) + 1
	if tries > maxPackets {
		t.Fatalf("切槽使整轮合批失去预算: TryRead=%d limit=%d", tries, maxPackets)
	}
}
func TestServerStopsHarvestingClosedSession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	old, next := lifecycleServerSession(), lifecycleServerSession()
	next.udpOnly.Store(true)
	packets := lifecyclePackets()
	for _, p := range packets {
		old.flowSlot(p)
	}
	reads, tries, queuedAtSwitch := 0, 0, 0
	queued := func() int { return len(old.slots[1].out) + len(old.slots[2].out) }
	s := &Server{current: old, log: discardLogger(), fatal: make(chan error, 1)}
	s.tun = &lifecycleTun{read: func(buf []byte) (int, error) {
		reads++
		if reads > 2 {
			cancel()
			return 0, ctx.Err()
		}
		return copy(buf, packets[0]), nil
	}, tryRead: func(buf []byte) (int, error) {
		tries++
		if tries == 4 {
			s.mu.Lock()
			s.current = next
			s.mu.Unlock()
			close(old.closed)
			queuedAtSwitch = queued()
		}
		if tries >= 2000 {
			return 0, nil
		}
		return copy(buf, packets[tries%2]), nil
	}}
	s.pumpTun(ctx)
	if tries > 4 || queued() != queuedAtSwitch {
		t.Fatalf("旧会话关闭后仍收割/入队: tries=%d queued=%d atSwitch=%d", tries, queued(), queuedAtSwitch)
	}
	if reads != 3 {
		t.Fatalf("未回到外层处理新UDP会话: reads=%d", reads)
	}
}
