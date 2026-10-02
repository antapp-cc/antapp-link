package client

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
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
	Members  int      `json:"members,omitempty"` // 服务端批准的并行连接数（0/1=单连接）
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

	// RebuildDevice 在适配器被外力干掉时由重连循环调用（App 注入完整重连）。
	// 它会 cancel 本循环，所以调用后循环直接退出；nil 则退回普通退避重试。
	RebuildDevice func()

	// wake 在网络出口迁移时被 watcher 触发：把重连从「最长 30s 退避」变成
	// 「网络一恢复立刻试」。带缓冲，连续多次通知合并成一次。
	wake chan struct{}
}

func NewTunnel(inv pki.Invite, dev Device, logger *slog.Logger) *Tunnel {
	if logger == nil {
		logger = slog.Default()
	}
	return &Tunnel{inv: inv, dev: dev, log: logger, wake: make(chan struct{}, 1)}
}

// Kick 唤醒重连循环。非阻塞：正在连接时通知被丢弃，无害。
func (t *Tunnel) Kick() {
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

// sessionEndInfo 把会话结束的错误归类成日志级别和人话原因，
// 让「会话结束」这行日志一眼能看出断线是什么造成的。
func sessionEndInfo(err error) (slog.Level, string) {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "EOF"):
		// 对端干净关闭：典型为云服重启或部署新版本
		return slog.LevelInfo, "服务端关闭了连接（常见原因：云服重启或部署新版本）"
	case strings.Contains(msg, "unreachable"):
		return slog.LevelWarn, "网络不可达（本机网络变动或出口中断）"
	case strings.Contains(msg, "refused"):
		return slog.LevelWarn, "连接被拒绝（服务端未运行或端口未放行）"
	case strings.Contains(msg, "timeout"):
		return slog.LevelWarn, "网络超时"
	case strings.Contains(msg, "reset"):
		return slog.LevelWarn, "连接被重置"
	default:
		return slog.LevelWarn, ""
	}
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
			// 适配器被外力干掉（系统停用/移除/驱动重置）时，重拨 TCP 救不了 ——
			// 必须重建整个设备与网络配置。交给 App 的完整重连，本循环随之退出。
			if errors.Is(err, ErrAdapterDead) && t.RebuildDevice != nil {
				t.log.Warn("虚拟网卡已失效（可能被系统或其他软件停用），自动重建设备并重连……")
				go t.RebuildDevice()
				return nil
			}
			lvl, reason := sessionEndInfo(err)
			args := []any{"err", err, "retry_in", Backoff(attempt)}
			if reason != "" {
				args = append(args, "reason", reason)
			}
			t.log.Log(ctx, lvl, "会话结束", args...)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.wake:
			// 网络出口刚迁移完（watcher 通知），立刻重试而不是干等退避计时
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

	// 多连接并发数据面：sid 是本次逻辑会话的团队编号，成员连接靠它认亲。
	// members 请求来自连接码（0/1=单连接）；服务端在 ACK 里批准实际值。
	sid := newSessionID()
	wantMembers := t.inv.Members
	if wantMembers < 1 {
		wantMembers = 1
	}
	if wantMembers > 4 {
		wantMembers = 4
	}

	hello, err := json.Marshal(map[string]any{
		"version": protoVersion, "client": t.inv.Name,
		"sid": sid, "members": wantMembers, "member": 0,
	})
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

	sess := &clientSession{
		tunnel: t, dev: t.dev, log: t.log, stats: &t.Stats,
		sid:     sid,
		members: ack.Members,
	}
	sess.slots[0] = conn
	if ack.Members > 1 {
		t.log.Info("多连接并发已启用", "members", ack.Members)
	}
	return true, sess.run(ctx)
}

// newSessionID 生成 16 字节随机数的 hex（32 字符），成员连接的认亲凭证。
func newSessionID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败极罕见；退回时间戳保证唯一性足够
		return fmt.Sprintf("%032x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

type clientSession struct {
	tunnel   *Tunnel
	dev      Device
	log      *slog.Logger
	stats    *Stats
	lastSeen atomic.Int64

	sid     string // 逻辑会话 ID
	members int    // 生效连接数（含控制连接）
	mu      sync.Mutex
	slots   []net.Conn // slots[0]=控制连接；成员槽位从 1 开始，nil=空
	writeMu sync.Mutex // 仅保护控制连接的写（成员连接各自在 writeSlot 内串行）
}

// write 串行化对控制连接的写：搬包和心跳来自不同 goroutine。
func (s *clientSession) write(t proto.Type, payload []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return proto.WriteFrame(s.slot(0), t, payload)
}

// slot 返回槽位 k 的连接；越界或空槽 → 控制连接兜底。
func (s *clientSession) slot(k int) net.Conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	if k < 0 || k >= len(s.slots) || s.slots[k] == nil {
		return s.slots[0]
	}
	return s.slots[k]
}

// writeSlot 把一个内层 IP 包按流哈希送进对应槽位。
func (s *clientSession) writeSlot(pkt []byte) error {
	k := proto.Slot(pkt, s.members)
	conn := s.slot(k)
	if k == 0 {
		s.writeMu.Lock()
		defer s.writeMu.Unlock()
	}
	return proto.WriteFrame(conn, proto.TypeIP, pkt)
}

// dialMember 拨一条成员连接并完成认亲握手；成功后进入该连接的读循环（阻塞）。
func (t *Tunnel) dialMember(ctx context.Context, sess *clientSession, k int) error {
	tlsCfg, err := pki.ClientTLSConfig(t.inv)
	if err != nil {
		return err
	}
	dialer := &net.Dialer{Timeout: handshakeTimeout}
	raw, err := dialer.DialContext(ctx, "tcp", t.inv.Server)
	if err != nil {
		return fmt.Errorf("成员连接 %d 拨号: %w", k, err)
	}
	conn := tls.Client(raw, tlsCfg)
	hctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	if err := conn.HandshakeContext(hctx); err != nil {
		raw.Close()
		return fmt.Errorf("成员连接 %d TLS 握手: %w", k, err)
	}

	hello, _ := json.Marshal(map[string]any{
		"version": protoVersion, "client": t.inv.Name,
		"sid": sess.sid, "members": sess.members, "member": k,
	})
	if err := proto.WriteFrame(conn, proto.TypeHello, hello); err != nil {
		conn.Close()
		return err
	}
	_ = conn.SetReadDeadline(time.Now().Add(handshakeTimeout))
	typ, _, err := proto.ReadFrame(conn)
	if err != nil {
		conn.Close()
		return err
	}
	_ = conn.SetReadDeadline(time.Time{})
	if typ == proto.TypeBye {
		conn.Close()
		return fmt.Errorf("成员连接 %d 被服务端拒绝", k)
	}
	if typ != proto.TypeMemberAck {
		conn.Close()
		return fmt.Errorf("成员连接 %d 期望 MEMBER_ACK 收到 %s", k, typ.String())
	}

	sess.mu.Lock()
	if k >= len(sess.slots) || sess.slots[k] != nil {
		sess.mu.Unlock()
		conn.Close()
		return fmt.Errorf("成员连接 %d 槽位不可用", k)
	}
	sess.slots[k] = conn
	sess.mu.Unlock()
	t.log.Info("成员连接已就位", "member", k, "live", sess.liveCount())

	// 读循环：成员连接上来的包写网卡；断开摘槽（该槽流量落回控制连接）
	for {
		typ, payload, err := proto.ReadFrame(conn)
		if err != nil {
			sess.mu.Lock()
			if sess.slots[k] == conn {
				sess.slots[k] = nil
			}
			sess.mu.Unlock()
			t.log.Info("成员连接断开", "member", k, "err", err)
			return err
		}
		sess.touch()
		switch typ {
		case proto.TypeIP:
			if _, err := sess.dev.Write(payload); err != nil {
				return err
			}
		default:
			// 成员连接只搬数据，其他帧忽略
		}
	}
}

// maintainMembers 维持成员连接：任何槽位空缺（初始或断开）就退避重拨。
// JOIN 失败绝不结束会话——最坏情况所有流量都走控制连接。
func maintainMembers(ctx context.Context, t *Tunnel, sess *clientSession) {
	var wg sync.WaitGroup
	for {
		sess.mu.Lock()
		var missing []int
		for k := 1; k < len(sess.slots); k++ {
			if sess.slots[k] == nil {
				missing = append(missing, k)
			}
		}
		sess.mu.Unlock()

		for _, k := range missing {
			k := k
			wg.Add(1)
			go func() {
				defer wg.Done()
				attempt := 0
				for {
					select {
					case <-ctx.Done():
						return
					case <-time.After(Backoff(attempt)):
					}
					if err := t.dialMember(ctx, sess, k); err != nil {
						if ctx.Err() != nil {
							return
						}
						t.log.Debug("成员连接拨接失败，退避重试", "member", k, "err", err)
						if attempt < 10 {
							attempt++
						}
						continue
					}
					return // 读循环退出（断开）后由下一轮维护循环补拨
				}
			}()
		}

		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case <-time.After(2 * time.Second):
		}
	}
}

// liveCount 报告当前存活连接数（日志用）。
func (s *clientSession) liveCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.slots {
		if c != nil {
			n++
		}
	}
	return n
}

func (s *clientSession) run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.touch()

	errCh := make(chan error, 3)
	go func() { errCh <- s.pumpFromTunnel(ctx) }()
	go func() { errCh <- s.pumpFromDevice(ctx) }()
	go func() { errCh <- s.heartbeat(ctx) }()
	if s.members > 1 {
		go maintainMembers(ctx, s.tunnel, s)
	}

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
		typ, payload, err := proto.ReadFrame(s.slot(0))
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
		if err := s.writeSlot(buf[:n]); err != nil {
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
