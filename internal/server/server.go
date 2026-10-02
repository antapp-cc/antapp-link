package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/antapp-cc/antapp-link/internal/pki"
	"github.com/antapp-cc/antapp-link/internal/proto"
)

const (
	// 客户端每 10 秒发一次 PING，这里给六倍宽限，避免网络抖动就误踢。
	clientDeadAfter  = 60 * time.Second
	handshakeTimeout = 15 * time.Second
	readBufferSize   = 65535
	protoVersion     = 1
)

type helloPayload struct {
	Version int    `json:"version"`
	Client  string `json:"client"`
	Sid     string `json:"sid,omitempty"`     // 逻辑会话 ID（多连接时同一伙连接共用）
	Members int    `json:"members,omitempty"` // 客户端请求的连接总数
	Member  int    `json:"member,omitempty"`  // 本连接槽位：0=控制连接，>0=成员
}

type helloAckPayload struct {
	TunnelIP string   `json:"tunnel_ip"`
	Gateway  string   `json:"gateway"`
	Prefix   int      `json:"prefix"`
	MTU      int      `json:"mtu"`
	MSS      int      `json:"mss"`
	DNS      []string `json:"dns"`
	Members  int      `json:"members,omitempty"` // 服务端批准的连接总数（0=单连接）
}

// Server 是一条专线的服务端。同一时刻只服务一个客户端。
type Server struct {
	cfg    Config
	tun    *TUN
	tlsCfg *tls.Config
	log    *slog.Logger

	mu      sync.Mutex
	current *session
}

// Run 阻塞运行隧道，直到 ctx 被取消。
func Run(ctx context.Context, cfg Config, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	tun, err := OpenTUN(cfg.Tunnel.Device)
	if err != nil {
		return err
	}
	defer tun.Close()
	if err := tun.Configure(cfg.Tunnel.ServerIP, cfg.PrefixLen(), cfg.Tunnel.MTU); err != nil {
		return err
	}
	tlsCfg, err := pki.ServerTLSConfig(cfg.PKIDir)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("监听 %s: %w", cfg.Listen, err)
	}
	defer ln.Close()

	s := &Server{cfg: cfg, tun: tun, tlsCfg: tlsCfg, log: logger}
	logger.Info("隧道已就绪",
		"listen", cfg.Listen,
		"device", tun.Name(),
		"server_ip", cfg.Tunnel.ServerIP,
		"client_ip", cfg.Tunnel.ClientIP,
		"network", cfg.Tunnel.Network,
		"mtu", cfg.Tunnel.MTU,
		"forward_ports", fmt.Sprintf("%d-%d", cfg.ForwardPorts.Start, cfg.ForwardPorts.End))

	go s.pumpTun(ctx)
	go s.reportStatus(ctx)

	// 端口转发：本机应答式转发器（握手在云服完成，延迟只算到云服为止；
	// 连接再经隧道转给节点机）。
	stopRelay, err := startPortRelay(ctx, cfg, logger)
	if err != nil {
		logger.Warn("端口转发监听启动失败", "err", err)
	} else {
		defer stopRelay()
	}
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept: %w", err)
		}
		go s.handle(ctx, conn)
	}
}

// pumpTun 把网卡上的包送往当前客户端。没有客户端时直接丢弃：云服自身的流量不会
// 走这张网卡（默认路由还在原网卡上），所以丢包是安全且正确的。
//
// 写合并：首包选定槽位后，非阻塞收割同槽的现成包（TryRead 不等待），拼单缓冲
// 一次写出——外层 TLS 记录减半。不同槽的包先 flush 再单独起批。
func (s *Server) pumpTun(ctx context.Context) {
	buf := make([]byte, readBufferSize)
	batch := make([]byte, 0, proto.MaxBatchBytes)
	for {
		n, err := s.tun.Read(buf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.log.Error("读网卡失败，搬运停止", "err", err)
			return
		}
		if n <= 0 {
			continue
		}
		sess := s.currentSession()
		if sess == nil {
			continue
		}
		k := proto.Slot(buf[:n], sess.membersN())

		batch = proto.AppendHeader(batch[:0], proto.TypeIP, n)
		batch = append(batch, buf[:n]...)

		for len(batch) < proto.MaxBatchBytes-proto.HeaderSize-readBufferSize {
			m, rerr := s.tun.TryRead(buf)
			if rerr != nil || m <= 0 {
				break
			}
			kk := proto.Slot(buf[:m], sess.membersN())
			if kk != k {
				if err := sess.flushBatch(k, batch); err != nil {
					s.log.Debug("转发给客户端失败", "client", sess.name, "err", err)
				}
				batch = batch[:0]
				k = kk
				batch = proto.AppendHeader(batch, proto.TypeIP, m)
				batch = append(batch, buf[:m]...)
				continue
			}
			batch = proto.AppendHeader(batch, proto.TypeIP, m)
			batch = append(batch, buf[:m]...)
		}

		if err := sess.flushBatch(k, batch); err != nil {
			s.log.Debug("转发给客户端失败", "client", sess.name, "err", err)
		}
		batch = batch[:0]
	}
}

func (s *Server) handle(ctx context.Context, raw net.Conn) {
	defer raw.Close()

	tlsConn := tls.Server(raw, s.tlsCfg)
	_ = tlsConn.SetDeadline(time.Now().Add(handshakeTimeout))
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		s.log.Warn("TLS 握手失败（客户端证书不受信或版本不符）",
			"remote", raw.RemoteAddr().String(), "err", err)
		return
	}
	_ = tlsConn.SetDeadline(time.Time{})

	name := "unknown"
	if state := tlsConn.ConnectionState(); len(state.PeerCertificates) > 0 {
		name = state.PeerCertificates[0].Subject.CommonName
	}

	_ = tlsConn.SetReadDeadline(time.Now().Add(handshakeTimeout))
	typ, payload, err := proto.ReadFrame(tlsConn)
	if err != nil {
		s.log.Warn("读首帧失败", "client", name, "err", err)
		return
	}
	if typ != proto.TypeHello {
		s.log.Warn("首帧不是 HELLO", "client", name, "type", typ.String())
		return
	}
	_ = tlsConn.SetReadDeadline(time.Time{})

	var hello helloPayload
	if err := json.Unmarshal(payload, &hello); err != nil {
		s.log.Warn("HELLO 解析失败", "client", name, "err", err)
		return
	}
	if hello.Version != protoVersion {
		// 明确告诉对端为什么连不上，否则老客户端只能看到连接被断开。
		_ = proto.WriteFrame(tlsConn, proto.TypeBye,
			[]byte(fmt.Sprintf("协议版本 %d 不受支持，本服务端为 %d", hello.Version, protoVersion)))
		return
	}

	if hello.Member > 0 {
		s.handleMember(ctx, tlsConn, name, hello)
		return
	}
	s.handleControl(ctx, tlsConn, name, hello)
}

// handleMember 处理成员连接：要求已有同证书、同 sid 的逻辑会话且槽位空闲。
// 任何不满足都只拒绝这一条连接，绝不影响现有会话。
func (s *Server) handleMember(ctx context.Context, tlsConn *tls.Conn, name string, hello helloPayload) {
	sess := s.currentSession()
	if sess == nil || sess.sid == "" || sess.sid != hello.Sid || sess.name != name {
		_ = proto.WriteFrame(tlsConn, proto.TypeBye, []byte("没有匹配的逻辑会话（控制连接未建立或 sid 不符）"))
		return
	}
	if !sess.join(hello.Member, tlsConn) {
		_ = proto.WriteFrame(tlsConn, proto.TypeBye, []byte("槽位不可用"))
		s.noteJoinFailure(sess)
		return
	}
	if err := proto.WriteFrame(tlsConn, proto.TypeMemberAck, nil); err != nil {
		sess.leave(hello.Member, tlsConn)
		return
	}
	s.log.Info("成员连接已加入", "client", name, "member", hello.Member,
		"live", sess.liveMembers())

	defer sess.leave(hello.Member, tlsConn)
	for {
		typ, payload, err := proto.ReadFrame(tlsConn)
		if err != nil {
			s.log.Info("成员连接断开", "client", name, "member", hello.Member, "err", err)
			return
		}
		sess.touch()
		switch typ {
		case proto.TypeIP:
			if _, err := s.tun.Write(payload); err != nil {
				s.log.Error("写网卡失败", "err", err)
				return
			}
		default:
			// 成员连接只搬数据；未知帧按兼容契约忽略
		}
	}
}

// joinFailures 记录各 sid 的连续 JOIN 失败次数（熔断拨接风暴）。
var joinFailures = struct {
	sync.Mutex
	m map[string]int
}{m: map[string]int{}}

func (s *Server) noteJoinFailure(sess *session) {
	joinFailures.Lock()
	joinFailures.m[sess.sid]++
	n := joinFailures.m[sess.sid]
	joinFailures.Unlock()
	if n == 20 {
		s.log.Warn("成员连接连续失败达到阈值，可能为客户端异常，建议检查", "client", sess.name)
	}
}

// handleControl 是原有控制连接路径（member==0），多连接时是会话的"老大"。
func (s *Server) handleControl(ctx context.Context, tlsConn *tls.Conn, name string, hello helloPayload) {
	approved := 1
	if hello.Members > 1 && s.cfg.MaxMembers > 1 {
		approved = hello.Members
		if approved > s.cfg.MaxMembers {
			approved = s.cfg.MaxMembers
		}
	}

	ack, err := json.Marshal(helloAckPayload{
		TunnelIP: s.cfg.Tunnel.ClientIP,
		Gateway:  s.cfg.Tunnel.ServerIP,
		Prefix:   s.cfg.PrefixLen(),
		MTU:      s.cfg.Tunnel.MTU,
		MSS:      s.cfg.Tunnel.MTU - 40,
		DNS:      s.cfg.DNS,
		Members:  approved,
	})
	if err != nil {
		s.log.Error("编码 HELLO_ACK 失败", "err", err)
		return
	}

	sess := &session{
		name:        name,
		sid:         hello.Sid,
		members:     approved,
		closed:      make(chan struct{}),
		connectedAt: time.Now(),
	}
	sess.slots = make([]*slotConn, approved)
	sess.slots[0] = &slotConn{conn: tlsConn}
	sess.touch()
	if err := sess.write(proto.TypeHelloAck, ack); err != nil {
		s.log.Warn("回 HELLO_ACK 失败", "client", name, "err", err)
		return
	}

	s.attach(sess)
	defer s.detach(sess)
	s.log.Info("客户端已接入", "client", name, "tunnel_ip", s.cfg.Tunnel.ClientIP,
		"remote", tlsConn.RemoteAddr().String(), "members", approved)

	s.serveSession(ctx, sess)
	s.log.Info("客户端已断开", "client", name)
}

func (s *Server) serveSession(ctx context.Context, sess *session) {
	defer sess.close()

	go func() {
		for {
			typ, payload, err := proto.ReadFrame(sess.slot(0).conn)
			if err != nil {
				if !errors.Is(err, io.EOF) && ctx.Err() == nil {
					s.log.Debug("读客户端失败", "client", sess.name, "err", err)
				}
				sess.close()
				return
			}
			sess.touch()
			switch typ {
			case proto.TypeIP:
				if _, err := s.tun.Write(payload); err != nil {
					s.log.Error("写网卡失败", "err", err)
					sess.close()
					return
				}
			case proto.TypePing:
				if err := sess.write(proto.TypePong, payload); err != nil {
					sess.close()
					return
				}
			case proto.TypePong:
				// lastSeen 已在上面刷新
			case proto.TypeBye:
				if len(payload) > 0 {
					s.log.Info("客户端主动断开", "client", sess.name, "reason", string(payload))
				}
				sess.close()
				return
			case proto.TypeHello, proto.TypeHelloAck:
				// 握手完成后重复出现，忽略
			default:
				// 未知帧忽略：将来加类型时老服务端不会因此断开连接
				s.log.Debug("忽略未知帧", "client", sess.name, "type", typ.String())
			}
		}
	}()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sess.closed:
			return
		case <-ticker.C:
			if idle := sess.idleFor(); idle > clientDeadAfter {
				s.log.Warn("客户端长时间没有任何帧，判定掉线", "client", sess.name, "idle", idle.Round(time.Second))
				return
			}
		}
	}
}

// attach 接受新会话并踢掉旧的：客户机重装或换机时不该被一个僵死的旧连接卡住。
func (s *Server) attach(sess *session) {
	s.mu.Lock()
	old := s.current
	s.current = sess
	s.mu.Unlock()

	if old != nil {
		s.log.Info("新连接取代旧连接", "new", sess.name, "old", old.name)
		_ = old.write(proto.TypeBye, []byte("已被新的连接取代"))
		old.close()
	}
}

func (s *Server) detach(sess *session) {
	s.mu.Lock()
	if s.current == sess {
		s.current = nil
	}
	s.mu.Unlock()
	sess.close()
}

func (s *Server) currentSession() *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current
}

// reportStatus 定期把运行时状态落到文件，供 `antapp-linkd status` 读取。
// 用文件而不是 IPC：status 是另一个短命进程，读一个 JSON 比引入套接字简单得多。
func (s *Server) reportStatus(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		st := Status{
			Listen:   s.cfg.Listen,
			Device:   s.cfg.Tunnel.Device,
			ServerIP: s.cfg.Tunnel.ServerIP,
			ClientIP: s.cfg.Tunnel.ClientIP,
		}
		if sess := s.currentSession(); sess != nil {
			st.Client = sess.name
			st.ConnectedAt = sess.connectedAt.Format(time.RFC3339)
			st.Members = sess.membersN()
			st.LiveMembers = sess.liveMembers()
		}
		if err := WriteStatus(st); err != nil {
			s.log.Debug("写状态文件失败", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

type session struct {
	name        string
	sid         string // 逻辑会话 ID：多连接的"团队编号"，空=单连接（老客户端）
	members     int    // 生效的连接总数（含控制连接），1=单连接
	closed      chan struct{}
	once        sync.Once
	lastSeen    atomic.Int64
	connectedAt time.Time

	mu    sync.Mutex
	slots []*slotConn // slots[0] 恒为控制连接；成员槽位从 1 开始
}

// slotConn 是逻辑会话里的一条物理连接。
type slotConn struct {
	conn    net.Conn
	writeMu sync.Mutex
}

func (s *slotConn) write(t proto.Type, payload []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return proto.WriteFrame(s.conn, t, payload)
}

// slot 返回槽位 k 的连接；k 越界或槽位已空 → 控制连接（槽 0）兜底。
func (s *session) slot(k int) *slotConn {
	s.mu.Lock()
	defer s.mu.Unlock()
	if k < 0 || k >= len(s.slots) || s.slots[k] == nil {
		return s.slots[0]
	}
	return s.slots[k]
}

// join 把成员连接放进槽 k；槽被占或越界返回 false。
func (s *session) join(k int, conn net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if k <= 0 || k >= len(s.slots) || s.slots[k] != nil {
		return false
	}
	s.slots[k] = &slotConn{conn: conn}
	return true
}

// leave 把槽位摘除（成员连接断开时）。该槽流量此后落回控制连接。
func (s *session) leave(k int, conn net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if k > 0 && k < len(s.slots) && s.slots[k] != nil && s.slots[k].conn == conn {
		s.slots[k] = nil
	}
}

// liveMembers 报告当前存活的连接数（状态展示用）。
func (s *session) liveMembers() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, sl := range s.slots {
		if sl != nil {
			n++
		}
	}
	return n
}

func (s *session) touch() { s.lastSeen.Store(time.Now().UnixNano()) }

func (s *session) idleFor() time.Duration {
	return time.Since(time.Unix(0, s.lastSeen.Load()))
}

// write 走控制连接（槽 0）写：搬运、心跳、BYE 等控制语义都在这条连接上。
func (s *session) write(t proto.Type, payload []byte) error {
	s.mu.Lock()
	ctrl := s.slots[0]
	s.mu.Unlock()
	if ctrl == nil {
		return net.ErrClosed
	}
	return ctrl.write(t, payload)
}

// writePkt 把一个内层 IP 包按流哈希送到对应槽位；槽位死亡落控制连接。
func (s *session) writePkt(pkt []byte) error {
	return s.slot(proto.Slot(pkt, s.membersN())).write(proto.TypeIP, pkt)
}

// flushBatch 把合并缓冲一次写入槽位 k 的连接。
func (s *session) flushBatch(k int, batch []byte) error {
	if len(batch) == 0 {
		return nil
	}
	return s.slot(k).write(proto.TypeIP, batch)
}

// membersN 返回生效的连接总数（读锁内拷贝，避免与握手竞态）。
func (s *session) membersN() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.members
}

func (s *session) close() {
	s.once.Do(func() {
		s.mu.Lock()
		for _, sl := range s.slots {
			if sl != nil {
				_ = sl.conn.Close()
			}
		}
		s.mu.Unlock()
		close(s.closed)
	})
}
