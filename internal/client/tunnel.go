package client

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/antapp-cc/antapp-link/internal/pki"
	"github.com/antapp-cc/antapp-link/internal/proto"
)

const (
	pingInterval     = 10 * time.Second
	deadAfter        = 30 * time.Second
	handshakeTimeout = 15 * time.Second
	readBufferSize   = 65535
	protoVersion     = 1
)

// Device 是虚拟网卡。Windows 上是 Wintun，其它平台是桩实现。
//
// 网卡这边只做「读写原始 IP 包」一件事：包交给操作系统内核的 TCP/IP 栈处理，
// 所以客户端不需要用户态协议栈，也不需要端口转发器。
type Device interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	Name() string
	Close() error
}

// Ack 是服务端 HELLO_ACK 的内容。
type Ack struct {
	TunnelIP string   `json:"tunnel_ip"`
	Gateway  string   `json:"gateway"`
	Prefix   int      `json:"prefix"`
	MTU      int      `json:"mtu"`
	MSS      int      `json:"mss"`
	DNS      []string `json:"dns"`
}

type Stats struct {
	Connected atomic.Bool
	RxBytes   atomic.Uint64
	TxBytes   atomic.Uint64
	RTTNanos  atomic.Int64
}

func (s *Stats) RTT() time.Duration { return time.Duration(s.RTTNanos.Load()) }

// Backoff 返回第 attempt 次重连前应等待的时长（attempt 从 0 开始）。
// 指数退避但封顶 30 秒：云服重启后客户端要能自己恢复，又不能把云服打爆。
func Backoff(attempt int) time.Duration {
	const (
		base = time.Second
		cap  = 30 * time.Second
	)
	if attempt <= 0 {
		return base
	}
	if attempt > 5 {
		return cap
	}
	d := base << attempt
	if d > cap {
		return cap
	}
	return d
}

type Tunnel struct {
	inv   pki.Invite
	dev   Device
	log   *slog.Logger
	Stats Stats
}

func NewTunnel(inv pki.Invite, dev Device, logger *slog.Logger) *Tunnel {
	if logger == nil {
		logger = slog.Default()
	}
	return &Tunnel{inv: inv, dev: dev, log: logger}
}

// Run 保持隧道可用，断线自动重连，直到 ctx 结束。
//
// 网卡与路由在整个过程中保持不动：隧道断掉时靠「原默认路由仍在、只是 metric 更高」
// 自动回落到本地线路，不会整机断网。
func (t *Tunnel) Run(ctx context.Context) error {
	attempt := 0
	for {
		if ctx.Err() != nil {
			return nil
		}
		connected, err := t.session(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if connected {
			// 成功握手过就重置退避，避免一次偶发断线让后续重连一直等 30 秒
			attempt = 0
		}
		if err != nil {
			t.log.Warn("会话结束", "err", err, "retry_in", Backoff(attempt))
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(Backoff(attempt)):
		}
		if attempt < 10 {
			attempt++
		}
	}
}

// session 建立一次完整会话并在其上搬运数据，返回是否成功握过手。
func (t *Tunnel) session(ctx context.Context) (bool, error) {
	tlsCfg, err := pki.ClientTLSConfig(t.inv)
	if err != nil {
		return false, err
	}

	dialer := &net.Dialer{Timeout: handshakeTimeout}
	raw, err := dialer.DialContext(ctx, "tcp", t.inv.Server)
	if err != nil {
		return false, fmt.Errorf("连接 %s: %w", t.inv.Server, err)
	}
	conn := tls.Client(raw, tlsCfg)

	hctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	if err := conn.HandshakeContext(hctx); err != nil {
		raw.Close()
		return false, fmt.Errorf("TLS 握手（证书或网络问题）: %w", err)
	}
	defer conn.Close()

	hello, err := json.Marshal(map[string]any{"version": protoVersion, "client": t.inv.Name})
	if err != nil {
		return false, err
	}
	if err := proto.WriteFrame(conn, proto.TypeHello, hello); err != nil {
		return false, fmt.Errorf("发 HELLO: %w", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(handshakeTimeout))
	typ, payload, err := proto.ReadFrame(conn)
	if err != nil {
		return false, fmt.Errorf("等 HELLO_ACK: %w", err)
	}
	_ = conn.SetReadDeadline(time.Time{})
	if typ == proto.TypeBye {
		return false, fmt.Errorf("服务端拒绝连接: %s", string(payload))
	}
	if typ != proto.TypeHelloAck {
		return false, fmt.Errorf("期望 HELLO_ACK，收到 %s", typ.String())
	}

	var ack Ack
	if err := json.Unmarshal(payload, &ack); err != nil {
		return false, fmt.Errorf("解析 HELLO_ACK: %w", err)
	}
	t.log.Info("隧道已建立",
		"server", t.inv.Server, "tunnel_ip", ack.TunnelIP, "gateway", ack.Gateway, "mtu", ack.MTU)
	t.Stats.Connected.Store(true)
	defer t.Stats.Connected.Store(false)

	sess := &clientSession{conn: conn, dev: t.dev, log: t.log, stats: &t.Stats}
	return true, sess.run(ctx)
}

type clientSession struct {
	conn     net.Conn
	dev      Device
	log      *slog.Logger
	stats    *Stats
	writeMu  sync.Mutex
	lastSeen atomic.Int64
}

// write 串行化对同一条 TLS 连接的写：搬包和心跳来自不同 goroutine。
func (s *clientSession) write(t proto.Type, payload []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return proto.WriteFrame(s.conn, t, payload)
}

func (s *clientSession) run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.touch()

	errCh := make(chan error, 3)
	go func() { errCh <- s.pumpFromTunnel(ctx) }()
	go func() { errCh <- s.pumpFromDevice(ctx) }()
	go func() { errCh <- s.heartbeat(ctx) }()

	select {
	case <-ctx.Done():
		return nil
	case err := <-errCh:
		cancel()
		return err
	}
}

func (s *clientSession) touch() { s.lastSeen.Store(time.Now().UnixNano()) }

// pumpFromTunnel：隧道来的 IP 包写进网卡，交给 Windows 内核处理。
func (s *clientSession) pumpFromTunnel(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return nil
		}
		typ, payload, err := proto.ReadFrame(s.conn)
		if err != nil {
			return fmt.Errorf("读隧道: %w", err)
		}
		s.touch()
		switch typ {
		case proto.TypeIP:
			if _, err := s.dev.Write(payload); err != nil {
				return fmt.Errorf("写网卡: %w", err)
			}
			s.stats.RxBytes.Add(uint64(len(payload)))
		case proto.TypePing:
			if err := s.write(proto.TypePong, payload); err != nil {
				return fmt.Errorf("回 PONG: %w", err)
			}
		case proto.TypePong:
			if len(payload) == 8 {
				sent := int64(binary.BigEndian.Uint64(payload))
				s.stats.RTTNanos.Store(time.Now().UnixNano() - sent)
			}
		case proto.TypeBye:
			return fmt.Errorf("服务端断开连接: %s", string(payload))
		}
	}
}

// pumpFromDevice：网卡上出现的包（也就是内核要发往外网的包）送进隧道。
func (s *clientSession) pumpFromDevice(ctx context.Context) error {
	buf := make([]byte, readBufferSize)
	for {
		if ctx.Err() != nil {
			return nil
		}
		n, err := s.dev.Read(buf)
		if err != nil {
			return fmt.Errorf("读网卡: %w", err)
		}
		if n <= 0 {
			continue
		}
		if err := s.write(proto.TypeIP, buf[:n]); err != nil {
			return fmt.Errorf("写隧道: %w", err)
		}
		s.stats.TxBytes.Add(uint64(n))
	}
}

func (s *clientSession) heartbeat(ctx context.Context) error {
	send := func() error {
		ts := make([]byte, 8)
		binary.BigEndian.PutUint64(ts, uint64(time.Now().UnixNano()))
		return s.write(proto.TypePing, ts)
	}
	if err := send(); err != nil {
		return err
	}

	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if idle := time.Since(time.Unix(0, s.lastSeen.Load())); idle > deadAfter {
				return fmt.Errorf("超过 %s 没收到任何帧，判定链路已死", deadAfter)
			}
			if err := send(); err != nil {
				return fmt.Errorf("发 PING: %w", err)
			}
		}
	}
}
