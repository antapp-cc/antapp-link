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
}

type helloAckPayload struct {
	TunnelIP string   `json:"tunnel_ip"`
	Gateway  string   `json:"gateway"`
	Prefix   int      `json:"prefix"`
	MTU      int      `json:"mtu"`
	MSS      int      `json:"mss"`
	DNS      []string `json:"dns"`
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
func (s *Server) pumpTun(ctx context.Context) {
	buf := make([]byte, readBufferSize)
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
		if err := sess.write(proto.TypeIP, buf[:n]); err != nil {
			s.log.Debug("转发给客户端失败", "client", sess.name, "err", err)
		}
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

	ack, err := json.Marshal(helloAckPayload{
		TunnelIP: s.cfg.Tunnel.ClientIP,
		Gateway:  s.cfg.Tunnel.ServerIP,
		Prefix:   s.cfg.PrefixLen(),
		MTU:      s.cfg.Tunnel.MTU,
		MSS:      s.cfg.Tunnel.MTU - 40,
		DNS:      s.cfg.DNS,
	})
	if err != nil {
		s.log.Error("编码 HELLO_ACK 失败", "err", err)
		return
	}

	sess := &session{conn: tlsConn, name: name, closed: make(chan struct{}), connectedAt: time.Now()}
	sess.touch()
	if err := sess.write(proto.TypeHelloAck, ack); err != nil {
		s.log.Warn("回 HELLO_ACK 失败", "client", name, "err", err)
		return
	}

	s.attach(sess)
	defer s.detach(sess)
	s.log.Info("客户端已接入", "client", name, "tunnel_ip", s.cfg.Tunnel.ClientIP,
		"remote", raw.RemoteAddr().String())

	s.serveSession(ctx, sess)
	s.log.Info("客户端已断开", "client", name)
}

func (s *Server) serveSession(ctx context.Context, sess *session) {
	defer sess.close()

	go func() {
		for {
			typ, payload, err := proto.ReadFrame(sess.conn)
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
	conn        net.Conn
	name        string
	closed      chan struct{}
	once        sync.Once
	writeMu     sync.Mutex
	lastSeen    atomic.Int64
	connectedAt time.Time
}

func (s *session) touch() { s.lastSeen.Store(time.Now().UnixNano()) }

func (s *session) idleFor() time.Duration {
	return time.Since(time.Unix(0, s.lastSeen.Load()))
}

// write 串行化对同一条 TLS 连接的写：搬运包、回 PONG、发 BYE 来自不同 goroutine。
func (s *session) write(t proto.Type, payload []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return proto.WriteFrame(s.conn, t, payload)
}

func (s *session) close() {
	s.once.Do(func() {
		close(s.closed)
		_ = s.conn.Close()
	})
}
