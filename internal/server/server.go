package server

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
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/antapp-cc/antapp-link/internal/pki"
	"github.com/antapp-cc/antapp-link/internal/proto"
)

const (
	// 客户端每 10 秒发一次 PING，这里给六倍宽限，避免网络抖动就误踢。
	clientDeadAfter = 60 * time.Second
	// 与客户端 handshakeTimeout 同界放宽到 30 秒。
	//
	// 计时从 accept 之后才开始 —— 客户端还在 SYN 重传时这里根本没有这条连接，所以它管不到
	// 客户端的拨号超时，管的是接下来的 TLS 握手和读 HELLO 这两段。这两段上的超时取双方
	// 最小值：客户端愿意等 30 秒、这里 15 秒就掐，慢连接照样建不起来（客户端拿到的是
	// 「连接被重置」而不是超时）。高丢包跨境链路上客户端要靠多次重传才走得完这两段。
	handshakeTimeout = 30 * time.Second
	// 成员连接不参与心跳（只有控制连接有 PING/PONG），一整天没数据也是正常的。
	// 但只剩单方向可用的半死连接会让读循环永不返回、槽位被永久占死（客户端补拨
	// 一直拿到「槽位不可用」），所以给一个宽松的空闲上限：120s 不误杀安静连接，
	// 又把「永久」变成有界。
	//
	// 现客户端每 15 秒发一次成员 PING（8 倍余量）。没带成员心跳的旧客户端会在安静
	// 120 秒后被摘槽、接着自动补拨，升级期混跑时表现为周期性重拨，可以接受。
	memberIdleTimeout = 120 * time.Second
	readBufferSize    = 65535
	protoVersion      = 1

	// handshakeMaxConcurrent 是同时在做的外层 TLS 握手上限。relay 的 256 条上限
	// 管的是转发连接，隧道监听这条路径上没有任何限制 —— 半开握手洪峰会吃光 fd，
	// 而 accept 循环看起来还一切正常。
	//
	// 准入在派发处理协程之前完成：如果每个 accept 都先起一个 goroutine 再去排队，
	// 等待队列本身就没有数量上限，洪峰时 goroutine 和 fd 照样会短时间冲高。
	handshakeMaxConcurrent = 64
	// tunDropRefreshEvery 是重新执行 ip -s link 的间隔。每 5 秒 fork 一次没必要，
	// 丢包是趋势指标，30 秒的粒度足够，也避免在 fd 紧张时雪上加霜。
	tunDropRefreshEvery = 30 * time.Second
)

// handshakeLimiter 给外层 TLS 握手加一个有界闸门。
//
// 名额只覆盖「TLS 握手 + 读 HELLO」这一段（见 serveConn/handle），不覆盖之后的会话：
// 长连接一占就是几小时，占着握手槽会让 64 条会话之后的新连接全部被拒。
type handshakeLimiter struct {
	slots chan struct{}

	inFlight atomic.Int64
	peak     atomic.Int64
	refused  atomic.Uint64
}

func newHandshakeLimiter(max int) *handshakeLimiter {
	return &handshakeLimiter{slots: make(chan struct{}, max)}
}

// acquire 非阻塞地拿一个握手名额。拿不到返回 (nil,false)，调用方负责关掉连接：
// 这些连接还没完成 TLS、没有任何可信身份，排队只会把 fd 占得更久。
func (l *handshakeLimiter) acquire() (func(), bool) {
	select {
	case l.slots <- struct{}{}:
		return l.granted(), true
	default:
		l.refused.Add(1)
		return nil, false
	}
}

// granted 记一次占用并返回幂等的释放函数：重复调用释放会让上限悄悄变宽。
func (l *handshakeLimiter) granted() func() {
	n := l.inFlight.Add(1)
	for {
		peak := l.peak.Load()
		if n <= peak || l.peak.CompareAndSwap(peak, n) {
			break
		}
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			<-l.slots
			l.inFlight.Add(-1)
		})
	}
}

func (l *handshakeLimiter) snapshot() (inFlight, peak int, refused uint64) {
	return int(l.inFlight.Load()), int(l.peak.Load()), l.refused.Load()
}

func (l *handshakeLimiter) refusedCount() uint64 { return l.refused.Load() }

// newInstanceID 生成本次进程的随机 ID，用来确认状态是这一次启动写的。
func newInstanceID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

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
	joins   *joinGuard

	// 搬运链路的致命错误（缓冲 1、非阻塞投递）：Run 收到就退出，交给 systemd 重启。
	// 没有它，pumpTun 一死进程仍然"健康"、systemd 不会重启，而转发器还会继续往一张
	// 已经没人读的网卡上拨号。
	fatal chan error
	// 端口转发监听结果，nil 表示还没跑起来；status 据此报真实可用性
	relay atomic.Pointer[relayState]
	// 转发器的在途连接计数。这批端口对公网开放，扫描器会一直挂上来，
	// 光看 goroutine 数只能事后发现；把它写进状态文件才能当场看出异常。
	relayStat atomic.Pointer[relayStats]

	// udp 是 UDP 数据通道的多路复用器（mode=udp 时才有）。控制仍走 TCP，数据包走它。
	udp *udpMux

	hs         *handshakeLimiter
	instanceID string
	startedAt  time.Time
}

// relayState 是端口转发器的启动结果：就绪，或一句能被 status 展示的失败原因。
type relayState struct {
	ready bool
	err   string
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

	s := &Server{cfg: cfg, tun: tun, tlsCfg: tlsCfg, log: logger, joins: newJoinGuard(),
		fatal:      make(chan error, 1),
		hs:         newHandshakeLimiter(handshakeMaxConcurrent),
		instanceID: newInstanceID(),
		startedAt:  time.Now(),
	}

	// UDP 数据通道：与 TCP 共用同一个端口号（不同协议，不冲突）。控制连接仍走 TCP，
	// 数据包改走这里。设计见 docs/superpowers/specs/2026-10-17-udp-data-channel-design.md
	if cfg.Mode == string(pki.ModeUDP) {
		m, err := newUDPMux(cfg.Listen, tun, logger)
		if err != nil {
			return fmt.Errorf("UDP 数据通道监听 %s: %w", cfg.Listen, err)
		}
		defer m.close()
		s.udp = m
		logger.Info("UDP 数据通道已启用（控制仍走 TCP）", "listen", cfg.Listen)
	}

	logger.Info("隧道已就绪",
		"listen", cfg.Listen,
		"device", tun.Name(),
		"server_ip", cfg.Tunnel.ServerIP,
		"client_ip", cfg.Tunnel.ClientIP,
		"network", cfg.Tunnel.Network,
		"mtu", cfg.Tunnel.MTU,
		"forward_ports", fmt.Sprintf("%d-%d", cfg.ForwardPorts.Start, cfg.ForwardPorts.End))

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	go s.pumpTun(runCtx)

	// 端口转发必须先于 reportStatus 就位：reportStatus 第一轮就会写状态文件，那时
	// relay 若还是 nil，ForwardKnown 就是 false —— status 子命令会把这当成「状态文件
	// 来自旧版本」，而其实只是刚启动。
	// tunnelUp 传进去：节点机不在线时转发器直接关连接，不去拨一个没人接的地址
	stopRelay, relayStat, err := startPortRelay(runCtx, cfg, logger, func() bool { return s.currentSession() != nil })
	if err != nil {
		// 不走 connLogAttrs：它把 "address already in use" 翻成人话后会把原始 err
		// 吞掉，而这里的 err 带着失败的转发端口号，正是排障要看的东西。listen 字段
		// 要填转发端口段，别填隧道 TLS 端口。
		logger.Warn("端口转发监听启动失败，外部流量进不了隧道", "err", err,
			"forward_ports", fmt.Sprintf("%d-%d", cfg.ForwardPorts.Start, cfg.ForwardPorts.End))
		s.relay.Store(&relayState{err: err.Error()})
	} else {
		s.relay.Store(&relayState{ready: true})
		s.relayStat.Store(relayStat)
		defer stopRelay()
	}
	go s.reportStatus(runCtx)
	go func() {
		<-runCtx.Done()
		_ = ln.Close()
	}()

	// accept 挪进 goroutine，Run 才能同时盯住搬运链路的致命错误
	acceptErr := make(chan error, 1)
	go func() {
		var backoff time.Duration
		for {
			conn, err := ln.Accept()
			if err != nil {
				if runCtx.Err() != nil {
					acceptErr <- nil
					return
				}
				// fd / 内核缓冲耗尽是瞬时故障：进程直接退出会变成 systemd 三秒一次的
				// 重启循环，而重启只是把资源清掉，什么都没解决。持续退避重试，让在途
				// 连接自然回收；不设放弃路径 —— 放弃了就等于这个进程再也接不了连接。
				if isResourceExhausted(err) {
					if backoff == 0 {
						backoff = time.Second
					} else if backoff < 30*time.Second {
						backoff *= 2
					}
					s.log.Warn("accept 失败：资源不足（fd 或内核缓冲），稍后重试", "retry_in", backoff)
					select {
					case <-runCtx.Done():
						acceptErr <- nil
						return
					case <-time.After(backoff):
					}
					continue
				}
				acceptErr <- fmt.Errorf("accept: %w", err)
				return
			}
			backoff = 0
			s.serveConn(runCtx, conn)
		}
	}()

	select {
	case err := <-s.fatal:
		// 与 ctx 取消同时发生时按正常停止处理：pumpTun 投递前判了 ctx.Err()，但两个
		// case 同时就绪时 select 是随机选的，不判这一下会把正常关闭报成非 0 退出。
		if runCtx.Err() != nil {
			return nil
		}
		// 搬运停了、进程却"健康"时 systemd 不会重启，运维只会看到字节不涨；明确退出，
		// 让 systemd 拉起一个干净的进程（也顺带结束"往没人读的网卡拨号"那种空转）
		return err
	case err := <-acceptErr:
		return err
	}
}

// isResourceExhausted 判断 accept 失败是不是「资源暂时不够」（fd、内核缓冲、内存）。
//
// 这类错误重试就好；当成致命错误会让进程陷入 systemd 三秒一次的重启循环。
// ECONNABORTED 不在此列 —— Go 的 poll.FD.Accept 内部已经重试，不会浮上来。
func isResourceExhausted(err error) bool {
	for _, e := range []error{syscall.EMFILE, syscall.ENFILE, syscall.ENOBUFS, syscall.ENOMEM} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
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
			// 唯一的搬运 goroutine 一退出，下行就永久中断，而 accept / 心跳 / 会话都还在：
			// 进程看起来仍然健康、systemd 不会重启，转发器还会继续往没人读的网卡拨号。
			// 投给 Run 让它退出。缓冲 1 + default：没人收也不阻塞这里的收尾。
			select {
			case s.fatal <- fmt.Errorf("读网卡失败，下行搬运停止: %w", err):
			default:
			}
			return
		}
		if n <= 0 {
			continue
		}
		if n > proto.MaxPayload {
			// 帧头装不下。硬塞进去会让对端把这 4 字节 IP 头当帧头解析、整条流从此
			// 错位（连接还在、数据全垃圾），所以宁可丢这个包。正常 MTU ≤1500 且没开
			// GSO 的 tun 读不到这么大的包，这是给将来放宽 MTU 兜的底。
			s.log.Warn("网卡读到的包超过帧上限，已丢弃", "len", n, "max", proto.MaxPayload)
			continue
		}
		sess := s.currentSession()
		if sess == nil {
			continue
		}
		// UDP 数据通道：不走槽位、不做写合并 —— 一个 IP 包就是一个 UDP 包。
		// 丢包交给内层应用（TCP）重传，这是设计上的取舍，见设计文档。
		if u := sess.udpSession(); u != nil && s.udp != nil {
			if err := u.send(s.udp, buf[:n]); err != nil {
				s.log.Debug("UDP 下行发送失败", "err", err)
			}
			continue
		}
		k := sess.flow.Slot(buf[:n], sess.membersN(), sess.slotAlive)

		batch = proto.AppendHeader(batch[:0], proto.TypeIP, n)
		batch = append(batch, buf[:n]...)

		for len(batch) < proto.MaxBatchBytes-proto.HeaderSize-proto.MaxPayload {
			m, rerr := s.tun.TryRead(buf)
			if rerr != nil || m <= 0 {
				break
			}
			if m > proto.MaxPayload {
				s.log.Warn("网卡读到的包超过帧上限，已丢弃", "len", m, "max", proto.MaxPayload)
				break
			}
			kk := sess.flow.Slot(buf[:m], sess.membersN(), sess.slotAlive)
			if kk != k {
				sess.flushBatch(k, batch)
				batch = batch[:0]
				k = kk
				batch = proto.AppendHeader(batch, proto.TypeIP, m)
				batch = append(batch, buf[:m]...)
				continue
			}
			batch = proto.AppendHeader(batch, proto.TypeIP, m)
			batch = append(batch, buf[:m]...)
		}

		sess.flushBatch(k, batch)
		batch = batch[:0]
	}
}

// socketBufferBytes 是单条连接的收发缓冲目标值，和客户端同一套理由：
// 跨境链路带宽延迟积约 2.5 MB，内核默认缓冲装不下，窗口撑不满吞吐就上不去。
// 上限由安装脚本设置的 net.core.rmem_max / wmem_max（16 MB）兜着。
//
// 默认仍是 4 MB。运行优化报告第 5 项要求在隔离机器上做 A/B（固定 4 MB vs 交给系统
// 自动调节），所以留了环境变量：ANTAPP_SERVER_SOCKET_BUFFER=0 表示不显式设置。
// 拿到 A/B 数据之前不改默认值 —— 两端策略不一致不代表哪一边更快。
var socketBufferBytes = envServerSocketBufferBytes()

func envServerSocketBufferBytes() int {
	const fallback = 4 << 20
	v := strings.TrimSpace(os.Getenv("ANTAPP_SERVER_SOCKET_BUFFER"))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return fallback
	}
	return n
}

// tuneTCPBuffers 调大这条 TCP 连接的收发缓冲，对应 OpenVPN 的 --sndbuf / --rcvbuf。
// 调不上不是错误：回到系统默认值，功能不受影响。0 表示完全不设置。
func tuneTCPBuffers(conn net.Conn) {
	tcp, ok := conn.(*net.TCPConn)
	if !ok || socketBufferBytes <= 0 {
		return
	}
	_ = tcp.SetReadBuffer(socketBufferBytes)
	_ = tcp.SetWriteBuffer(socketBufferBytes)
}

// serveConn 在派生处理协程之前完成握手准入。
//
// 队列不设缓冲：名额满了就直接关连接。半开握手洪峰下，拒绝日志按次数采样 ——
// 每个连接一条 Warn 会把日志本身变成新的资源消耗。
func (s *Server) serveConn(ctx context.Context, conn net.Conn) bool {
	release, ok := s.hs.acquire()
	if !ok {
		if n := s.hs.refusedCount(); n == 1 || n%100 == 0 {
			inFlight, _, _ := s.hs.snapshot()
			s.log.Warn("握手并发已满，拒绝连接（防止半开握手吃光资源）",
				"max", cap(s.hs.slots), "in_flight", inFlight, "refused_total", n,
				"remote", conn.RemoteAddr())
		}
		_ = conn.Close()
		return false
	}
	go s.handle(ctx, conn, release)
	return true
}

func (s *Server) handle(ctx context.Context, raw net.Conn, releaseHandshake func()) {
	defer raw.Close()
	tuneTCPBuffers(raw)

	tlsConn, name, hello, ok := s.handshake(ctx, raw)
	// HELLO 验证完就还名额：后面的会话可能持续几小时，长连接占着握手槽会让
	// 后续连接的准入全部失败（64 条会话就够占满整个闸门）。
	releaseHandshake()
	if !ok {
		return
	}
	if hello.Member > 0 {
		s.handleMember(ctx, tlsConn, name, hello)
		return
	}
	s.handleControl(ctx, tlsConn, name, hello)
}

// handshake 做 TLS 握手并校验首帧 HELLO。ok=false 时调用方只需关闭连接。
func (s *Server) handshake(ctx context.Context, raw net.Conn) (*tls.Conn, string, helloPayload, bool) {
	tlsConn := tls.Server(raw, s.tlsCfg)
	_ = tlsConn.SetDeadline(time.Now().Add(handshakeTimeout))
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		s.log.Warn("TLS 握手失败（客户端证书不受信或版本不符）", connLogAttrs(err, raw)...)
		return nil, "", helloPayload{}, false
	}
	_ = tlsConn.SetDeadline(time.Time{})

	name := "unknown"
	if state := tlsConn.ConnectionState(); len(state.PeerCertificates) > 0 {
		name = state.PeerCertificates[0].Subject.CommonName
	}

	_ = tlsConn.SetReadDeadline(time.Now().Add(handshakeTimeout))
	typ, payload, err := proto.ReadFrame(tlsConn)
	if err != nil {
		s.log.Warn("读首帧失败", connLogAttrs(err, tlsConn, "client", name)...)
		return nil, "", helloPayload{}, false
	}
	if typ != proto.TypeHello {
		s.log.Warn("首帧不是 HELLO", "client", name, "type", typ.String())
		return nil, "", helloPayload{}, false
	}
	_ = tlsConn.SetReadDeadline(time.Time{})

	var hello helloPayload
	if err := json.Unmarshal(payload, &hello); err != nil {
		s.log.Warn("HELLO 解析失败", "client", name, "err", err)
		return nil, "", helloPayload{}, false
	}
	if hello.Version != protoVersion {
		// 明确告诉对端为什么连不上，否则老客户端只能看到连接被断开。
		_ = proto.WriteFrame(tlsConn, proto.TypeBye,
			[]byte(fmt.Sprintf("协议版本 %d 不受支持，本服务端为 %d", hello.Version, protoVersion)))
		return nil, "", helloPayload{}, false
	}
	return tlsConn, name, hello, true
}

// handleMember 处理成员连接：要求已有同证书、同 sid 的逻辑会话且槽位空闲。
// 任何不满足都只拒绝这一条连接，绝不影响现有会话。
func (s *Server) handleMember(ctx context.Context, tlsConn *tls.Conn, name string, hello helloPayload) {
	sess := s.currentSession()
	if sess == nil || sess.sid == "" || sess.sid != hello.Sid || sess.name != name {
		_ = proto.WriteFrame(tlsConn, proto.TypeBye, []byte("没有匹配的逻辑会话（控制连接未建立或 sid 不符）"))
		return
	}
	// 熔断：拒绝不等于结束会话，客户端退避后到窗口外边自然会重试成功
	if !s.joins.allow(sess.sid, time.Now()) {
		_ = proto.WriteFrame(tlsConn, proto.TypeBye, []byte("成员连接失败次数过多，已暂停加入"))
		s.log.Debug("成员加入被熔断拒绝", "client", name, "sid", sess.sid)
		return
	}
	if !sess.slotFree(hello.Member) {
		_ = proto.WriteFrame(tlsConn, proto.TypeBye, []byte("槽位不可用"))
		s.noteJoinFailure(sess)
		return
	}
	if err := proto.WriteFrame(tlsConn, proto.TypeMemberAck, nil); err != nil {
		return
	}
	s.log.Info("成员连接已加入", "client", name, "member", hello.Member)

	defer sess.leave(hello.Member, tlsConn)
	// 就绪信号：读循环已在本协程启动（下面的 for），发带槽位号的第二个 ACK
	// —— 客户端收到它才把流量切进本连接，消除「写进还没人读的连接」的黑洞
	if err := proto.WriteFrame(tlsConn, proto.TypeMemberAck,
		[]byte(fmt.Sprintf("{\"member\":%d}", hello.Member))); err != nil {
		return
	}
	if !sess.commitSlot(hello.Member, tlsConn) {
		_ = proto.WriteFrame(tlsConn, proto.TypeBye, []byte("槽位不可用"))
		s.noteJoinFailure(sess)
		return
	}
	s.log.Info("成员连接已就位", "client", name, "member", hello.Member,
		"live", sess.liveMembers())
	for {
		// 每读到一帧就续期。成员连接没有自己的心跳，安静是正常的；只有单方向可用的
		// 半死连接才会一直读不到帧，超时后必须断开这条，否则槽位被永久占死
		if err := tlsConn.SetReadDeadline(time.Now().Add(memberIdleTimeout)); err != nil {
			return
		}
		typ, payload, err := proto.ReadFrame(tlsConn)
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) || os.IsTimeout(err) {
				s.log.Warn("成员连接空闲超时（该方向已死），断开该槽等客户端补拨",
					"client", name, "member", hello.Member, "idle_timeout", memberIdleTimeout)
			} else {
				s.log.Info("成员连接断开", connLogAttrs(err, tlsConn,
					"client", name, "member", hello.Member)...)
			}
			return
		}
		if sl := sess.slotEntry(hello.Member); sl != nil {
			sl.touch()
		}
		switch typ {
		case proto.TypeIP:
			if sl := sess.slotEntry(hello.Member); sl != nil {
				sl.noteRx(len(payload))
			}
			if _, err := s.tun.Write(payload); err != nil {
				s.log.Error("写网卡失败", "err", err)
				return
			}
		case proto.TypePing:
			// 成员连接上的心跳应答：客户端定期发 PING 让这条连接两端一直有包
			// 往返，免得被跨境链路上的 NAT 当成空闲连接清掉状态。回 PONG 必须
			// 走 slotConn.write 而不是裸 proto.WriteFrame —— pumpTun 在另一个
			// goroutine 上往同一条连接写数据，两者要共用写锁，否则帧会交错。
			if sl := sess.slotEntry(hello.Member); sl != nil {
				if err := sl.write(proto.TypePong, payload); err != nil {
					return
				}
			}
		default:
			// 成员连接只搬数据；未知帧按兼容契约忽略
		}
	}
}

// 成员连接熔断：窗口内 JOIN 失败超阈值就拒绝该 sid 的后续加入。
const (
	joinWindow  = 60 * time.Second
	joinMaxFail = 20
)

// joinGuard 跟踪各 sid 的 JOIN 失败次数，防客户端 bug 打成拨接风暴。
// 计数只活在窗口内、随会话结束清掉——不会像全局 map 那样只涨不消。
type joinGuard struct {
	mu  sync.Mutex
	rec map[string]*joinRec
}

type joinRec struct {
	n     int
	since time.Time
}

func newJoinGuard() *joinGuard { return &joinGuard{rec: map[string]*joinRec{}} }

// allow 报告该 sid 现在是否还能尝试加入。
func (g *joinGuard) allow(sid string, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	r := g.rec[sid]
	if r == nil {
		return true
	}
	if now.Sub(r.since) > joinWindow {
		delete(g.rec, sid)
		return true
	}
	return r.n < joinMaxFail
}

// fail 记一次 JOIN 失败，返回是否刚刚达到阈值（阈值只告警一次）。
func (g *joinGuard) fail(sid string, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	r := g.rec[sid]
	if r == nil || now.Sub(r.since) > joinWindow {
		r = &joinRec{since: now}
		g.rec[sid] = r
	}
	r.n++
	return r.n == joinMaxFail
}

// failures 报告该 sid 当前窗口内的失败次数（状态展示用）。
func (g *joinGuard) failures(sid string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if r := g.rec[sid]; r != nil {
		return r.n
	}
	return 0
}

// forget 在会话结束时清掉该 sid 的计数。
func (g *joinGuard) forget(sid string) {
	g.mu.Lock()
	delete(g.rec, sid)
	g.mu.Unlock()
}

func (s *Server) noteJoinFailure(sess *session) {
	if s.joins.fail(sess.sid, time.Now()) {
		s.log.Warn("成员连接连续失败达到阈值，暂停该会话的成员加入",
			"client", sess.name, "failures", joinMaxFail, "window", joinWindow)
	}
}

// connBrokenReason 把连接断开的 error 翻成一句人话；认不出返回空串。
// 客户端侧有一份对应实现（Windows 的 syscall 文案也需要它）。
func connBrokenReason(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "use of closed network connection"):
		return "本端主动关闭（写超时或会话正在收尾）"
	case strings.Contains(msg, "connection reset"),
		strings.Contains(msg, "forcibly closed"),
		strings.Contains(msg, "connection abort"):
		return "对端强制断开（连接被重置）"
	case strings.Contains(msg, "did not properly respond after a period of time"),
		strings.Contains(msg, "host has failed to respond"),
		strings.Contains(msg, "i/o timeout"),
		strings.Contains(msg, "timed out"):
		return "对端一段时间没有响应（线路中断，或对端进程卡住）"
	case strings.Contains(msg, "eof"):
		return "对端正常关闭了连接"
	case strings.Contains(msg, "unreachable"):
		return "网络不可达"
	case strings.Contains(msg, "refused"):
		return "连接被拒绝（对端没在监听这个端口）"
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

// sameControlSession 判断新来的控制连接是不是当前会话的重复连接。
// 只有双方都带 sid 且相同才算重复：老客户端没有 sid，保持原来的踢旧接新行为。
func sameControlSession(cur *session, sid string) bool {
	return cur != nil && sid != "" && cur.sid == sid
}

// handleControl 是原有控制连接路径（member==0），多连接时是会话的"老大"。
func (s *Server) handleControl(ctx context.Context, tlsConn *tls.Conn, name string, hello helloPayload) {
	if sameControlSession(s.currentSession(), hello.Sid) {
		// 同会话的第二条控制连接：踢掉旧的会连累整条隧道，直接拒绝这一条
		_ = proto.WriteFrame(tlsConn, proto.TypeBye, []byte("同一会话的控制连接已存在"))
		s.log.Warn("拒绝重复的控制连接", "client", name, "sid", hello.Sid)
		return
	}

	approved := 1
	// 按客户端请求的成员数批准。
	//
	// 服务的 mode=udp 时**不能**在这里压回 1：模式是服务端配置，客户端可能还拿着
	// tcp 连接码（大量节点机就是这种）。压成 1 条的话，那些客户端的 4 条成员变成
	// 1 条，所有流量挤在单条 TCP 上 —— 在 16~20% 重传的跨境链路上慢得能感觉出来。
	//
	// UDP 客户端本来就不会多要：client/tunnel.go 里 UseUDP 时 wantMembers 直接置 1，
	// 所以这里放开不会白建任何连接。
	if hello.Members > 1 && s.cfg.Tunnel.MaxMembers > 1 {
		approved = hello.Members
		if approved > s.cfg.Tunnel.MaxMembers {
			approved = s.cfg.Tunnel.MaxMembers
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
		flow:        proto.NewFlowTable(),
		tlsConn:     tlsConn,
	}
	sess.slots = make([]*slotConn, approved)
	sess.slots[0] = newSlotConn(tlsConn)
	if err := sess.write(proto.TypeHelloAck, ack); err != nil {
		s.log.Warn("回 HELLO_ACK 失败", connLogAttrs(err, tlsConn, "client", name)...)
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
					s.log.Debug("读客户端失败", connLogAttrs(err, sess.slot(0).conn, "client", sess.name)...)
				}
				sess.close()
				return
			}
			sess.slot(0).touch()
			switch typ {
			case proto.TypeIP:
				if sl := sess.slotEntry(0); sl != nil {
					sl.noteRx(len(payload))
				}
				if _, err := s.tun.Write(payload); err != nil {
					s.log.Error("写网卡失败", "err", err)
					sess.close()
					return
				}
			case proto.TypeUDPPort:
				// 客户端上报自己的 UDP 数据口：从这里开始，数据包改走 UDP。
				if s.udp == nil {
					s.log.Warn("收到 UDP 端口上报，但服务端不是 udp 模式，已忽略",
						"client", sess.name)
					continue
				}
				if len(payload) < 2 {
					continue
				}
				port := int(binary.BigEndian.Uint16(payload))
				remote, ok := sess.slot(0).conn.RemoteAddr().(*net.TCPAddr)
				if !ok || sess.tlsConn == nil {
					s.log.Warn("登记 UDP 会话失败：拿不到 TCP 远端地址", "client", sess.name)
					continue
				}
				// IP 只认 TCP 连接的真实远端，端口由控制通道上报 —— 见 udprelay.go 的注释
				u, err := s.udp.addSession(sess.tlsConn.ConnectionState(), remote.IP, port)
				if err != nil {
					s.log.Warn("登记 UDP 会话失败", "client", sess.name, "err", err)
					continue
				}
				sess.mu.Lock()
				sess.udp = u
				sess.mu.Unlock()
				s.log.Info("客户端 UDP 数据通道已登记", "client", sess.name,
					"udp_port", port, "remote_ip", remote.IP.String())
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
			// 只看控制连接：成员连接上的数据帧不该给一条已经半死的控制连接续命
			// （没有 FIN 的那种半死连接，靠会话级 lastSeen 是永远发现不了的）。
			if idle := sess.slot(0).idleFor(); idle > clientDeadAfter {
				s.log.Warn("控制连接长时间没有任何帧，判定掉线", "client", sess.name, "idle", idle.Round(time.Second))
				return
			}
		}
	}
}

// udpSession 返回这个会话的 UDP 数据通道，没有则 nil。
func (s *session) udpSession() *udpSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.udp
}

// attach 接受新会话并踢掉旧的：客户机重装或换机时不该被一个僵死的旧连接卡住。
func (s *Server) attach(sess *session) {
	s.mu.Lock()
	old := s.current
	s.current = sess
	s.mu.Unlock()

	if old != nil {
		s.log.Info("新连接取代旧连接", "new", sess.name, "old", old.name)
		// BYE 丢到后台尽力发，主线程立刻 close：write 受 20 秒写超时约束，旧控制连接
		// 写缓冲满时会把「新会话接管」拖住最长 20 秒。close 会让那次 write 立即失败，
		// 所以实际语义是「能发就发，发不出去拉倒」—— 旧客户端看不到原因也会自己重连。
		go func() { _ = old.write(proto.TypeBye, []byte("已被新的连接取代")) }()
		old.close()
	}
}

func (s *Server) detach(sess *session) {
	s.mu.Lock()
	if s.current == sess {
		s.current = nil
	}
	// UDP 会话必须随控制连接一起移除：否则旧地址还能继续往隧道里注入数据。
	// 锁序固定为 s.mu → sess.mu，与 udpSession() 不嵌套，无死锁风险。
	var u *udpSession
	sess.mu.Lock()
	u, sess.udp = sess.udp, nil
	sess.mu.Unlock()
	s.mu.Unlock()
	if s.udp != nil && u != nil {
		s.udp.removeSession(u)
	}
	s.joins.forget(sess.sid)
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

	// 丢包读取有 2 秒预算，没必要每 5 秒跑一次外部命令；上一次的值在刷新间隔内复用。
	var (
		lastTunCheck time.Time
		tunRx, tunTx uint64
		tunKnown     bool
	)
	for {
		st := Status{
			Listen:     s.cfg.Listen,
			Device:     s.cfg.Tunnel.Device,
			ServerIP:   s.cfg.Tunnel.ServerIP,
			ClientIP:   s.cfg.Tunnel.ClientIP,
			Goroutines: runtime.NumGoroutine(),
			PID:        os.Getpid(),
			InstanceID: s.instanceID,
			StartedAt:  s.startedAt.Format(time.RFC3339),
		}
		if used, limit, err := readProcFDUsage("/proc"); err == nil {
			st.FDUsed, st.FDLimit, st.FDKnown = used, limit, true
		} else {
			// 读不到就写 known=false：显示成「0 个 fd」会让人以为一切正常
			s.log.Debug("读 fd 用量失败", "err", err)
		}
		if time.Since(lastTunCheck) >= tunDropRefreshEvery {
			rx, tx, err := readTunDrops(ctx, s.cfg.Tunnel.Device)
			lastTunCheck = time.Now()
			if err == nil {
				tunRx, tunTx, tunKnown = rx, tx, true
			} else {
				tunKnown = false
				s.log.Debug("读 TUN 丢包计数失败", "err", err)
			}
		}
		st.TUNRxDropped, st.TUNTxDropped, st.TUNDropsKnown = tunRx, tunTx, tunKnown
		if inFlight, peak, refused := s.hs.snapshot(); inFlight > 0 || peak > 0 || refused > 0 {
			st.HandshakeInFlight, st.HandshakePeak, st.HandshakeRefused = inFlight, peak, refused
		}
		if rs := s.relay.Load(); rs != nil {
			st.ForwardReady = rs.ready
			st.ForwardError = rs.err
			st.ForwardKnown = true
		}
		if rstat := s.relayStat.Load(); rstat != nil {
			active, total, rejected, idle := rstat.snapshot()
			st.ForwardActive = active
			st.ForwardTotal = total
			st.ForwardRejected = rejected
			st.ForwardSwept = idle
		}
		if sess := s.currentSession(); sess != nil {
			st.Client = sess.name
			st.ConnectedAt = sess.connectedAt.Format(time.RFC3339)
			st.Members = sess.membersN()
			st.LiveMembers = sess.liveMembers()
			st.Slots = sess.slotStats()
			st.JoinFailures = s.joins.failures(sess.sid)
			if u := sess.udpSession(); u != nil {
				st.UDPActive = true
				st.UDPRxBytes = u.rxBytes.Load()
				st.UDPTxBytes = u.txBytes.Load()
			}
			fillFlowStatus(&st, sess.flow)
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
	connectedAt time.Time

	mu    sync.Mutex
	slots []*slotConn // slots[0] 恒为控制连接；成员槽位从 1 开始
	// flow 决定下行包走哪个槽：已有流固定不变（保序），新流分给当前承载流数最少的槽。
	// 纯哈希做不到均衡 —— 一条大流会把某条成员连接塞到写超时被摘掉，别的槽却闲着。
	flow *proto.FlowTable

	// udp 是 UDP 数据通道（mode=udp 时才有）。非 nil 时下行数据不走槽位，直接走它。
	udp *udpSession

	// tlsConn 是控制连接本身。UDP 数据通道的密钥要从它的 TLS 会话导出（RFC 5705），
	// 所以必须留着它 —— 槽位里存的是 net.Conn 接口，类型断言太脆。
	tlsConn *tls.Conn
}

// slotConn 是逻辑会话里的一条物理连接。
//
// 数据批走独立写协程（out + close(out) 的 closed），控制帧仍用 write 同步写。
// 分两条路是因为调用方不同：write 只在各连接的读循环/握手路径里调，阻塞它只影响
// 那一条连接；而数据批来自 pumpTun —— 那是单个 goroutine 串行服务所有槽，在那里
// 同步写任何一条，拥塞时都会把其余槽（含控制连接的心跳）一起拖停。
type slotConn struct {
	conn    net.Conn
	writeMu sync.Mutex
	txBytes atomic.Uint64 // 写出的链路字节（含帧头）
	rxBytes atomic.Uint64 // 读入的链路字节（含帧头）

	out    chan []byte   // 待写数据批，队列满即丢，绝不阻塞 pumpTun
	closed chan struct{} // 关闭后写协程退出
	once   sync.Once
	drops  atomic.Uint64 // 因队列满被丢弃的批数，排障时看这个

	// lastSeen 是这条连接自己的活跃时刻。判掉线只能看控制连接（槽 0）的这个值：
	// 会话级的 lastSeen 会被成员连接一起刷新，于是「控制连接半死（没有 FIN）但成员
	// 还在收数据」时服务端永不判死，僵尸会话一直挂在 current 上、status 还显示运行中。
	lastSeen atomic.Int64
}

func (s *slotConn) touch() { s.lastSeen.Store(time.Now().UnixNano()) }

func (s *slotConn) idleFor() time.Duration {
	return time.Since(time.Unix(0, s.lastSeen.Load()))
}

// slotQueueLen 是每槽待写批的队列深度。批最大 MaxBatchBytes（约 20KB 量级），
// 8 批约百来 KB，够吸收一个写超时周期内的突发，又不会让延迟长到难忍。
const slotQueueLen = 8

func newSlotConn(conn net.Conn) *slotConn {
	sc := &slotConn{
		conn:   conn,
		out:    make(chan []byte, slotQueueLen),
		closed: make(chan struct{}),
	}
	sc.touch() // lastSeen 为零值会让 idleFor 算出天文数字，判死逻辑一上来就误杀
	go sc.writeLoop()
	return sc
}

// writeLoop 是每槽唯一的写协程，串行消费队列。写失败就关掉这条连接：读循环会随之
// 退出、槽位被 leave 回收，客户端按退避补拨，比留一条写不动的连接强。
func (s *slotConn) writeLoop() {
	for {
		select {
		case batch := <-s.out:
			if err := s.writeRaw(batch); err != nil {
				_ = s.conn.Close()
				return
			}
		case <-s.closed:
			return
		}
	}
}

// enqueue 把数据批交给写协程，绝不阻塞调用方 —— pumpTun 只有一个 goroutine，在这里
// 等任何一个槽都会拖停全部。队列满就丢这一批：那条 TCP 本来也在重传或重连，丢包
// 远好过全局停顿。
//
// batch 必须拷一份：调用方用的是可复用缓冲（发完就 batch[:0] 接着拼下一个包），
// 直接入队会让写协程读到被覆写的内容。
func (s *slotConn) enqueue(batch []byte) {
	if len(batch) == 0 {
		return
	}
	// 先看队列满没满再拷贝：恒定溢出时不必白花一次 16KB 的分配和拷贝
	if len(s.out) == cap(s.out) {
		s.drops.Add(1)
		return
	}
	cp := make([]byte, len(batch))
	copy(cp, batch)
	select {
	case s.out <- cp:
	case <-s.closed:
	default:
		s.drops.Add(1)
	}
}

// closeWrite 让写协程退出。由 session.close 调用。
func (s *slotConn) closeWrite() {
	s.once.Do(func() { close(s.closed) })
}

// slotWriteTimeout 限制单次写槽。pumpTun 是单 goroutine 串行往各槽写，
// 一条卡住的连接会把它后面的几条一起拖停。
//
// 20 秒而不是 5 秒：节点机同步区块时上行很满，而这条跨国链路实测有 30% 级别丢包，
// TCP 发送窗口会长时间处于满的状态 —— 5 秒不够把一批写完，成员连接就被反复误杀
// （日志里全是「本端主动关闭（写超时）」）。真拥塞时晚一点判死只是多等一会儿，
// 误杀的代价却是那条槽的流量全丢、还要重新拨号。
var slotWriteTimeout = 20 * time.Second

func (s *slotConn) write(t proto.Type, payload []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.conn.SetWriteDeadline(time.Now().Add(slotWriteTimeout)); err != nil {
		return err
	}
	if err := proto.WriteFrame(s.conn, t, payload); err != nil {
		_ = s.conn.Close() // 写不动就断开这一条，别让它拖住整条分发链路
		return err
	}
	s.txBytes.Add(uint64(len(payload) + proto.HeaderSize))
	return nil
}

// writeRaw 整批原样写出：批里每帧的帧头已由 AppendHeader 拼好，不能再走 WriteFrame
// （否则对端会把多出来的 4 字节帧头当成 IP 包内容，下行数据全被内核丢弃）。
func (s *slotConn) writeRaw(batch []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.conn.SetWriteDeadline(time.Now().Add(slotWriteTimeout)); err != nil {
		return err
	}
	if _, err := s.conn.Write(batch); err != nil {
		_ = s.conn.Close()
		return err
	}
	s.txBytes.Add(uint64(len(batch)))
	return nil
}

// noteRx 记一次读入，按链路上的字节算（含帧头）。
func (s *slotConn) noteRx(payloadLen int) {
	s.rxBytes.Add(uint64(payloadLen + proto.HeaderSize))
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

// slotEntry 返回槽位 k 的连接条目本身（不落回控制连接）；越界或空槽返回 nil。
func (s *session) slotEntry(k int) *slotConn {
	s.mu.Lock()
	defer s.mu.Unlock()
	if k < 0 || k >= len(s.slots) {
		return nil
	}
	return s.slots[k]
}

// slotAlive 报告槽位 k 此刻是否真的可用。FlowTable 靠它避开已断开的空槽 —— 分给空槽
// 的新流下行会被 flushBatch 整批丢掉，那条流就再也不通了，而且流表不会自我纠正。
func (s *session) slotAlive(k int) bool {
	return s.slotEntry(k) != nil
}

// slotStats 快照各槽的收发字节（status 展示用）。
func (s *session) slotStats() []SlotStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SlotStatus, 0, len(s.slots))
	for i, sl := range s.slots {
		st := SlotStatus{Slot: i}
		if sl != nil {
			st.RxBytes = sl.rxBytes.Load()
			st.TxBytes = sl.txBytes.Load()
			st.Drops = sl.drops.Load()
		}
		out = append(out, st)
	}
	return out
}

// slotFree 预检槽位是否可占（不占位）。
func (s *session) slotFree(k int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return k > 0 && k < len(s.slots) && s.slots[k] == nil
}

// commitSlot 把成员连接正式放进槽 k（读循环已启动后才调用）。
// 预检（slotFree）到提交之间被抢 → 返回 false。
//
// 会话已关闭也返回 false：close() 遍历槽位时这条连接还没进槽，之后更不会关它，
// 放进来就脱离了生命周期。检查与 close(s.closed) 同在锁内，才能堵住这个窗口。
func (s *session) commitSlot(k int, conn net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.closed:
		return false
	default:
	}
	if k <= 0 || k >= len(s.slots) || s.slots[k] != nil {
		return false
	}
	s.slots[k] = newSlotConn(conn)
	return true
}

// leave 把槽位摘除（成员连接断开时）。数据此后走 slotEntry → nil → 整批丢弃，
// 而不是落回控制连接（那会把心跳挤到排队）。
//
// 必须先 closeWrite 再置 nil：写协程不属于任何列表，槽位一被摘掉，session.close()
// 就再也遍历不到它 —— goroutine 和它那 8 格的队列会永久挂住，成员每断开/超时一次
// 漏一个（实测 20 次断开残留 19 个）。
func (s *session) leave(k int, conn net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if k > 0 && k < len(s.slots) && s.slots[k] != nil && s.slots[k].conn == conn {
		s.slots[k].closeWrite()
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

// flushBatch 把合并缓冲交给槽位 k 的写协程。
//
// 用 slotEntry 而不是 slot：slot 在槽为空时会回落到控制连接，而控制连接的职责是
// 心跳 —— 成员一断，本该发那条的数据就全涌进槽 0，把 PING/PONG 挤到排队，面板延迟
// 立刻从两百毫秒涨到上千；断两条时一半流量都挤过去，这也正是「有成员断开延迟就大」
// 的原因。槽空就把这批丢掉：那条链路的 TCP 本来也在重传或重连，等成员回来自会恢复。
// 控制帧（PING/PONG）仍然走 slot() 回落，它们必须发出去。
//
// 入队不阻塞：pumpTun 是单个 goroutine 串行服务所有槽，在这里同步写任何一条，拥塞
// 时都会把其余槽一起拖停（20 秒写超时意味着最坏 20 秒的全局停顿）。
func (s *session) flushBatch(k int, batch []byte) {
	if len(batch) == 0 {
		return
	}
	if sc := s.slotEntry(k); sc != nil {
		sc.enqueue(batch)
	}
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
		defer s.mu.Unlock()
		for _, sl := range s.slots {
			if sl != nil {
				sl.closeWrite()
				_ = sl.conn.Close()
			}
		}
		// 在锁内关闭：commitSlot 也在锁内查它，否则刚提交的连接会晚于收尾、没人再关
		close(s.closed)
	})
}
