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
	"io"
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

	// maxMembers 是单会话允许的并行连接数上限（与连接码、服务端 max_members 同界）。
	maxMembers = 4
)

// Device 是虚拟网卡。Windows 上是 Wintun，其它平台是桩实现。
//
// 网卡这边只做「读写原始 IP 包」一件事：包交给操作系统内核的 TCP/IP 栈处理，
// 所以客户端不需要用户态协议栈，也不需要端口转发器。
type Device interface {
	Read(p []byte) (int, error)
	// TryRead 非阻塞收割一个已就绪的包：无包立即返回 (0, nil)，不等待。
	// 供写合并使用；实现方不支持时返回 (0, ErrNoTryRead)。
	TryRead(p []byte) (int, error)
	Write(p []byte) (int, error)
	Name() string
	Close() error
}

// ErrNoTryRead 表示设备不支持非阻塞读（写合并自动退化为单包模式）。
var ErrNoTryRead = errors.New("client: 设备不支持 TryRead")

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

	// Trace 是联调用的顺序追踪（nil = 关闭，零开销）。它记录每个包的槽位，
	// 用来断言「同一内层流始终走同一条外层连接」。
	Trace *OrderTracer
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

// connBrokenReason 把连接断开的原始 error 翻成一句人话。
// 按原文匹配是因为 Windows 的 syscall 文案很长、且不含 timeout 这类关键词；
// 认不出来就返回空串，不编原因。
func connBrokenReason(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "use of closed network connection"):
		return "本端主动关闭（写超时或会话正在收尾）"
	case strings.Contains(msg, "did not properly respond after a period of time"),
		strings.Contains(msg, "host has failed to respond"),
		strings.Contains(msg, "i/o timeout"),
		strings.Contains(msg, "timed out"):
		return "对端一段时间没有响应（线路中断，或对端进程卡住/被防火墙丢包）"
	case strings.Contains(msg, "forcibly closed"),
		strings.Contains(msg, "connection reset"),
		strings.Contains(msg, "connection abort"):
		return "对端强制断开（连接被重置）"
	case strings.Contains(msg, "eof"):
		return "对端正常关闭了连接"
	case strings.Contains(msg, "unreachable"):
		return "网络不可达（本机网络变动或出口中断）"
	case strings.Contains(msg, "refused"):
		return "连接被拒绝（服务端没在运行或端口没放行）"
	case strings.Contains(msg, "address already in use"):
		return "端口已被占用（另一个进程在监听这个端口）"
	case strings.Contains(msg, "didn't provide a certificate"),
		strings.Contains(msg, "certificate required"):
		return "对端没有提供证书"
	case strings.Contains(msg, "bad certificate"),
		strings.Contains(msg, "unknown authority"):
		return "证书不被信任（签发者不认识）"
	case strings.Contains(msg, "certificate has expired"):
		return "证书已过期"
	case strings.Contains(msg, "broken pipe"):
		return "对端已经断开（写不进去）"
	case strings.Contains(msg, "no route to host"):
		return "没有到对端的路由（网络不通）"
	default:
		return ""
	}
}

// connLogAttrs 组装断开日志的字段，认不出原因时才附上原始 error（它是英文）。
func connLogAttrs(err error, conn net.Conn, attrs ...any) []any {
	reason := connBrokenReason(err)
	out := append([]any{}, attrs...)
	out = append(out, "reason", reason)
	if conn != nil {
		out = append(out, "local", conn.LocalAddr(), "remote", conn.RemoteAddr())
	}
	if reason == "" {
		out = append(out, "err", err)
	}
	return out
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
			// 同样不摆英文原文：认得出原因就只给中文，认不出才附 err 当线索
			args := []any{"reason", reason, "retry_in", Backoff(attempt)}
			if reason == "" {
				args = append(args, "err", err)
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
	wantMembers := effectiveMembers(t.inv.Members)

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
		"server", t.inv.Server, "tunnel_ip", ack.TunnelIP, "gateway", ack.Gateway, "mtu", ack.MTU,
		"sid", sid, "want_members", wantMembers, "ack_members", ack.Members)
	t.Stats.Connected.Store(true)
	defer t.Stats.Connected.Store(false)

	sess := newClientSession(t, conn, sid, tlsCfg, ack)
	sess.trace = t.Trace
	if sess.members > 1 {
		t.log.Info(fmt.Sprintf("多连接并发已启用：%d 条并行连接（含控制连接）", sess.members))
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

// effectiveMembers 把连接数归一到 1..maxMembers：连接码里 0/1=单连接，老服务端
// 不回 members 字段也是 0 —— 两种都必须退化成单连接，绝不能造出 0 长度槽表。
func effectiveMembers(n int) int {
	if n < 1 {
		return 1
	}
	if n > maxMembers {
		return maxMembers
	}
	return n
}

// newClientSession 组装一次逻辑会话：槽 0 恒为控制连接，成员槽从 1 开始。
// tlsCfg 是控制连接握手用的那份，成员连接必须复用它——各自新建会得到空会话缓存。
func newClientSession(t *Tunnel, conn net.Conn, sid string, tlsCfg *tls.Config, ack Ack) *clientSession {
	n := effectiveMembers(ack.Members)
	sess := &clientSession{
		tunnel: t, dev: t.dev, log: t.log, stats: &t.Stats,
		sid: sid, members: n, tlsCfg: tlsCfg,
		slots:  make([]net.Conn, n),
		writes: make([]sync.Mutex, n),
	}
	sess.slots[0] = conn
	return sess
}

type clientSession struct {
	tunnel   *Tunnel
	dev      Device
	log      *slog.Logger
	stats    *Stats
	lastSeen atomic.Int64

	sid     string // 逻辑会话 ID
	members int    // 生效连接数（含控制连接）
	tlsCfg  *tls.Config
	trace   *OrderTracer    // 联调顺序追踪（nil = 关闭）
	ctx     context.Context // 会话上下文（run 里赋值），收尾时用来判断写失败是否预期内
	mu      sync.Mutex
	slots   []net.Conn   // slots[0]=控制连接；成员槽位从 1 开始，nil=空
	writes  []sync.Mutex // 每槽一把写锁；成员槽空时流量落回控制连接，锁也跟着落回
}

// sessionEnded 报告会话是否已进入收尾（此时连接会随会话一起关，写失败是预期内的）。
func (s *clientSession) sessionEnded() bool {
	return s.ctx != nil && s.ctx.Err() != nil
}

// slotWriteTimeout 限制单次写连接。卡住的连接必须能摘掉，否则会拖停整条分发链路。
var slotWriteTimeout = 5 * time.Second

// writeSlot 返回实际承载的连接、它的写锁与实际槽号。锁必须跟着落回控制连接，
// 否则会和心跳的两次 Write 交错把帧撕开；实际槽号用来区分写不动的是成员还是会话命脉。
func (s *clientSession) writeSlot(k int) (int, net.Conn, *sync.Mutex) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if k <= 0 || k >= len(s.slots) || s.slots[k] == nil {
		return 0, s.slots[0], &s.writes[0]
	}
	return k, s.slots[k], &s.writes[k]
}

// write 串行化对控制连接的写：搬包和心跳来自不同 goroutine。
func (s *clientSession) write(t proto.Type, payload []byte) error {
	_, conn, mu := s.writeSlot(0)
	mu.Lock()
	defer mu.Unlock()
	if err := conn.SetWriteDeadline(time.Now().Add(slotWriteTimeout)); err != nil {
		return err
	}
	return proto.WriteFrame(conn, t, payload)
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

// closeOnDone 在 ctx 结束时关闭 conn，把阻塞在 ReadFrame 上的读循环放出来；
// 返回的 stop 供会话正常结束时收掉监听协程。
func closeOnDone(ctx context.Context, conn io.Closer) (stop func()) {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	return func() { close(done) }
}

// dialMember 拨一条成员连接并完成认亲握手；成功后进入该连接的读循环（阻塞）。
// TLS 配置复用会话里那一份：会话缓存在成员连接之间共享，才有机会走会话恢复。
func (t *Tunnel) dialMember(ctx context.Context, sess *clientSession, k int) error {
	dialer := &net.Dialer{Timeout: handshakeTimeout}
	raw, err := dialer.DialContext(ctx, "tcp", t.inv.Server)
	if err != nil {
		return fmt.Errorf("成员连接 %d 拨号: %w", k, err)
	}
	conn := tls.Client(raw, sess.tlsCfg)
	// 握手失败、被拒、读循环退出、ctx 结束都收口到这一处：不关连接就会漏 fd，
	// 读循环也会永远卡在 ReadFrame 上
	defer conn.Close()
	defer closeOnDone(ctx, conn)()

	hctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	if err := conn.HandshakeContext(hctx); err != nil {
		return fmt.Errorf("成员连接 %d TLS 握手: %w", k, err)
	}
	if conn.ConnectionState().DidResume {
		// 每次补拨都有，日常日志里纯噪声；验收会话恢复时开 Debug 看
		t.log.Debug("成员连接复用了 TLS 会话", "member", k, "did_resume", true)
	}

	hello, _ := json.Marshal(map[string]any{
		"version": protoVersion, "client": t.inv.Name,
		"sid": sess.sid, "members": sess.members, "member": k,
	})
	if err := proto.WriteFrame(conn, proto.TypeHello, hello); err != nil {
		return err
	}
	_ = conn.SetReadDeadline(time.Now().Add(handshakeTimeout))
	typ, _, err := proto.ReadFrame(conn)
	if err != nil {
		return err
	}
	if typ == proto.TypeBye {
		return fmt.Errorf("成员连接 %d 被服务端拒绝: %s", k, "见服务端日志")
	}
	if typ != proto.TypeMemberAck {
		return fmt.Errorf("成员连接 %d 期望 MEMBER_ACK 收到 %s", k, typ.String())
	}
	// 第一个 ACK 收到：加入被接受。继续等第二个 ACK（带槽位号）=
	// 服务端读循环已启动的信号，收到它才把连接当可用
	typ2, _, err := proto.ReadFrame(conn)
	if err != nil {
		return err
	}
	_ = conn.SetReadDeadline(time.Time{})
	if typ2 != proto.TypeMemberAck {
		return fmt.Errorf("成员连接 %d 期望就绪 ACK 收到 %s", k, typ2.String())
	}

	sess.mu.Lock()
	if k >= len(sess.slots) || sess.slots[k] != nil {
		sess.mu.Unlock()
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
			t.log.Info("成员连接断开", connLogAttrs(err, conn, "member", k)...)
			return err
		}
		sess.touch()
		switch typ {
		case proto.TypeIP:
			sess.trace.Record("rx", k, payload)
			if _, err := sess.dev.Write(payload); err != nil {
				return err
			}
		case proto.TypeBye:
			// 服务端拒绝了这条成员连接，或正在关它：立刻摘槽，别继续往里写
			sess.mu.Lock()
			if sess.slots[k] == conn {
				sess.slots[k] = nil
			}
			sess.mu.Unlock()
			t.log.Warn("成员连接被服务端关闭", "member", k, "reason", string(payload))
			return fmt.Errorf("成员连接 %d 被服务端关闭: %s", k, string(payload))
		default:
			// 成员连接只搬数据，其他帧忽略
		}
	}
}

// memberRetryDelay 是补拨成员连接前的等待：首次立刻拨（几条连接本来就该并发建），
// 失败之后才按退避来。
func memberRetryDelay(attempt int) time.Duration {
	if attempt <= 0 {
		return 0
	}
	return Backoff(attempt - 1)
}

// maintainMembers 每个成员槽位一条常驻协程：退避重拨，断开后自动补位。
// JOIN 失败绝不结束会话——最坏情况所有流量都走控制连接。
func maintainMembers(ctx context.Context, t *Tunnel, sess *clientSession) {
	var wg sync.WaitGroup
	for k := 1; k < sess.members; k++ {
		k := k
		wg.Add(1)
		go func() {
			defer wg.Done()
			attempt := 0
			for {
				if d := memberRetryDelay(attempt); d > 0 {
					select {
					case <-ctx.Done():
						return
					case <-time.After(d):
					}
				} else if ctx.Err() != nil {
					return
				}
				if err := t.dialMember(ctx, sess, k); err != nil {
					if ctx.Err() != nil {
						return
					}
					// 补拨失败原本是完全静默的，出问题时看不出卡在哪一步
					t.log.Debug("成员连接补拨失败", connLogAttrs(err, nil,
						"member", k, "retry_in", memberRetryDelay(attempt+1))...)
					if attempt < 10 {
						attempt++
					}
					continue
				}
				attempt = 0 // 读循环退出（连接断开），重置退避继续补拨
			}
		}()
	}
	wg.Wait()
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
	s.ctx = ctx
	s.touch()

	errCh := make(chan error, 3)
	go func() { errCh <- s.pumpFromTunnel(ctx) }()
	go func() { errCh <- s.pumpFromDevice(ctx) }()
	go func() { errCh <- s.heartbeat(ctx) }()
	if s.members > 1 {
		s.log.Info(fmt.Sprintf("多连接模式：目标 %d 条，启动成员维护", s.members))
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
			s.trace.Record("rx", 0, payload)
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
//
// 写合并：首包选定槽位后，非阻塞地继续收割内核缓冲里现成的、同槽的包，
// 拼成单缓冲一次写出——外层 TLS 记录数量减半（帧头+载荷合并成一趟）。
// 收不到就停，绝不等待：打字、ping 这类单包场景行为不变。
func (s *clientSession) pumpFromDevice(ctx context.Context) error {
	buf := make([]byte, readBufferSize)
	batch := make([]byte, 0, proto.MaxBatchBytes)
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
		pktLen := n
		k := proto.Slot(buf[:n], s.members)
		s.trace.Record("tx", k, buf[:pktLen])
		s.stats.TxBytes.Add(uint64(pktLen))

		batch = proto.AppendHeader(batch[:0], proto.TypeIP, pktLen)
		batch = append(batch, buf[:pktLen]...)

		// 收割现成的同槽包：不等待、批量不超上限；设备不支持就单包模式
		for len(batch) < proto.MaxBatchBytes-proto.HeaderSize-proto.MaxPayload {
			m, rerr := s.dev.TryRead(buf)
			if rerr != nil || m <= 0 {
				break
			}
			kk := proto.Slot(buf[:m], s.members)
			s.trace.Record("tx", kk, buf[:m])
			if kk != k {
				// 不同槽的包放回没有手段（Device 是单读接口），直接单发处理完再收
				if err := flushBatch(s, k, batch); err != nil {
					return err
				}
				k = kk
				batch = batch[:0]
				batch = proto.AppendHeader(batch, proto.TypeIP, m)
				batch = append(batch, buf[:m]...)
				s.stats.TxBytes.Add(uint64(m))
				continue
			}
			batch = proto.AppendHeader(batch, proto.TypeIP, m)
			batch = append(batch, buf[:m]...)
			s.stats.TxBytes.Add(uint64(m))
		}

		if err := flushBatch(s, k, batch); err != nil {
			return err
		}
		batch = batch[:0]
	}
}

// flushBatch 把合并缓冲一次写出。写失败按实际落槽分流：成员连接只摘那一条并补拨，
// 会话继续跑；只有控制连接写不动才算会话断了。
func flushBatch(s *clientSession, k int, batch []byte) error {
	if len(batch) == 0 {
		return nil
	}
	actual, conn, mu := s.writeSlot(k)
	if conn == nil {
		return fmt.Errorf("写隧道: 槽 %d 连接为 nil", k)
	}
	mu.Lock()
	defer mu.Unlock()
	// SetWriteDeadline 在连接已关时就会失败，所以它和 Write 的失败要一起分流：
	// 只处理 Write 的话，"对端已经关了"这条最常见的路径会绕过日志与摘槽。
	werr := conn.SetWriteDeadline(time.Now().Add(slotWriteTimeout))
	if werr == nil {
		_, werr = conn.Write(batch)
	}
	if werr != nil {
		if s.sessionEnded() {
			// 会话收尾：槽位连接随会话一起关，写失败是预期内的竞态，记成 ERROR 会误导排障
			s.log.Debug("会话收尾中写隧道失败", connLogAttrs(werr, conn, "slot", k, "bytes", len(batch))...)
			return fmt.Errorf("写隧道: %w", werr)
		}
		if actual > 0 {
			// 一条成员连接出问题不该重启整条隧道：摘掉它，让维护协程补一条新的
			s.dropSlot(actual, conn)
			s.log.Warn("成员连接写不动，已摘除该槽", connLogAttrs(werr, conn, "slot", actual)...)
			return nil
		}
		s.log.Error("写隧道失败", connLogAttrs(werr, conn, "slot", actual, "bytes", len(batch))...)
		return fmt.Errorf("写隧道: %w", werr)
	}
	return nil
}

// dropSlot 摘掉写不动的成员连接。关闭放在锁外，避免持锁做 IO。
func (s *clientSession) dropSlot(k int, conn net.Conn) {
	s.mu.Lock()
	if k > 0 && k < len(s.slots) && s.slots[k] == conn {
		s.slots[k] = nil
	}
	s.mu.Unlock()
	_ = conn.Close()
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
