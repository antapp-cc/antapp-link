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
	pingInterval = 10 * time.Second
	// deadAfter 必须显著大于 slotWriteTimeout：单连接模式（members=1）下数据也走槽 0，
	// 一个数据批最长能占住那条连接的写锁 20 秒，心跳只能排在它后面。60 秒的判死线留了
	// 三倍余量，连续两个慢批也不会把好好的会话自己判死重连。
	deadAfter = 60 * time.Second
	// handshakeTimeout 同时管 dial、TLS 握手、等 HELLO_ACK 三段，取的是三者的公共上限。
	//
	// 从 15 秒放宽到 30 秒，依据是高丢包跨境链路的实测：RTT 250ms、丢包 16% 时这三段
	// 每一步都得靠 TCP 重传才过得去，累计轻易超过 15 秒。晚高峰一小时内 9 次连接尝试、
	// 0 次建成会话，报错是 dial i/o timeout —— 不是对端拒绝，是自己放弃太早。
	//
	// 客户端跑在 Windows 上，SYN 重传节奏由 Windows 内核决定（约 3 秒起步、二十来秒
	// 放弃），Linux 的 tcp_syn_retries 管不到它 —— 服务端那台才是 Linux，两者的参数
	// 不能混起来当依据。
	//
	// 服务端 server.handshakeTimeout 必须同步放宽：在 accept 之后的 TLS 握手与 HELLO 段，
	// 超时取双方最小值，一方先到就会把本来能建成的连接掐掉（客户端看到的是「连接被重置」
	// 而不是超时）。
	handshakeTimeout = 30 * time.Second
	// memberHandshakeTimeout 是成员连接一次建连（dial + TLS + 两个 ACK）的总预算。
	//
	// 不复用 handshakeTimeout：成员断了会退避重拨，它属于「补齐」而不是「首次建立」，
	// 三段各 30 秒意味着一轮失败最坏拖 90 秒，违背「成员断开后 30 秒内补齐」的设计目标
	// （docs/superpowers/specs/2026-10-01-multi-tcp-concurrency.md）。用一个总预算把
	// 三段合计封住，长尾才有界。
	//
	// 取 20 秒而不是更短：这三段在丢包链路上每段都要重传（间隔 1s/2s/4s 递增），12 秒在
	// 严重丢包时可能一次都建不起来；20 秒仍留在一「30 秒内补齐」的单次边界之内。
	memberHandshakeTimeout = 20 * time.Second
	readBufferSize         = 65535
	protoVersion           = 1

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
//
// 指数退避，10 秒起步、封顶 3 分钟。起点刻意不短：链路抖动往往几秒内就恢复，
// 立刻重拨只会在对端刚断开、TCP 还没完全清理时反复失败，日志看着像在疯狂重连；
// 等一下再试成功率更高，也不会给云服添无谓的连接压力。
func Backoff(attempt int) time.Duration {
	const (
		base = 10 * time.Second
		cap  = 3 * time.Minute
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

	// wake 在网络出口迁移时被 watcher 触发：把重连从「最长 3 分钟退避」变成
	// 「网络一恢复立刻试」。带缓冲，连续多次通知合并成一次。
	wake chan struct{}

	// Trace 是联调用的顺序追踪（nil = 关闭，零开销）。它记录每个包的槽位，
	// 用来断言「同一内层流始终走同一条外层连接」。
	Trace *OrderTracer

	// pktCh 是网卡读循环与「当前会话」之间的包管道。
	//
	// 读循环只有一条、由 Tunnel 持有：Wintun 的 Read 是阻塞的，如果每个会话各起一条，
	// 会话结束时那条 goroutine 会一直卡在 Read 里，直到下一个包到来才退出 —— 反复重连
	// 就积累一堆旧 reader，而且它们还会和新会话抢同一个网卡上的包（表现为重连后偶发
	// 首包丢失）。把读循环提到这一层，会话怎么重拨都只有这一条。
	pktCh chan []byte

	// readErr 承接读循环的致命错误。
	//
	// 没有它的话，设备读失败只是让 readPump 自己 return：控制连接照常收发心跳，
	// 面板显示在线、延迟也正常，但上行早就停止搬运了 —— 直到别的方向偶然报错才重连。
	// 带缓冲 1：读循环挂掉后塞一次就退出，不会阻塞。
	readErr chan error

	// pktDrops 是「上行队列满、网卡读到的包被丢弃」的次数。
	// 正常情况下它一直是 0；涨起来说明下游处理跟不上，是"越用越慢/没网"的第一现场。
	pktDrops atomic.Int64

	// udpCh 指向当前会话的 UDP 数据通道（UDP 模式才有），会话结束清空。
	//
	// 存在的理由：界面要能区分「控制连接在、数据面已经不通」。UDP 模式下控制走 TCP，
	// 光看 Stats.Connected 会一直显示「已连接」，而业务包早就发不出去了。
	udpCh atomic.Pointer[udpChannel]
}

func NewTunnel(inv pki.Invite, dev Device, logger *slog.Logger) *Tunnel {
	if logger == nil {
		logger = slog.Default()
	}
	return &Tunnel{inv: inv, dev: dev, log: logger, wake: make(chan struct{}, 1),
		pktCh: make(chan []byte, 64), readErr: make(chan error, 1)}
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
		// 其余交给统一映射：Windows 的 syscall 长文案（如 wsarecv 的超时整句）
		// 只有它认得出，认出来就不必在这行再摆一串英文原文
		return slog.LevelWarn, connBrokenReason(err)
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
		return "本端关闭"
	case strings.Contains(msg, "did not properly respond after a period of time"),
		strings.Contains(msg, "host has failed to respond"),
		strings.Contains(msg, "i/o timeout"),
		strings.Contains(msg, "timed out"):
		return "对端无响应"
	case strings.Contains(msg, "forcibly closed"),
		strings.Contains(msg, "connection reset"),
		strings.Contains(msg, "connection abort"):
		return "对端重置"
	case strings.Contains(msg, "eof"):
		return "正常关闭"
	case strings.Contains(msg, "unreachable"):
		return "网络不可达"
	case strings.Contains(msg, "refused"):
		return "连接被拒绝"
	case strings.Contains(msg, "address already in use"):
		return "端口占用"
	case strings.Contains(msg, "didn't provide a certificate"),
		strings.Contains(msg, "certificate required"):
		return "对端无证书"
	case strings.Contains(msg, "bad certificate"),
		strings.Contains(msg, "unknown authority"):
		return "证书不受信"
	case strings.Contains(msg, "certificate has expired"):
		return "证书已过期"
	case strings.Contains(msg, "broken pipe"):
		return "对端已断开"
	case strings.Contains(msg, "no route to host"):
		return "无路由"
	default:
		return ""
	}
}

// connLogAttrs 组装断开日志的字段，认不出原因时才附上原始 error（它是英文）。
func connLogAttrs(err error, conn net.Conn, attrs ...any) []any {
	reason := connBrokenReason(err)
	out := append([]any{}, attrs...)
	if reason == "" {
		// 认不出来也给它一句中文，别让界面上直接出现英文原文。
		// 原始错误仍附在后面：真排查的人需要它，普通用户看前面的中文就够了。
		reason = "原因不明"
	}
	out = append(out, "reason", reason)
	if conn != nil {
		out = append(out, "local", conn.LocalAddr(), "remote", conn.RemoteAddr())
	}
	if connBrokenReason(err) == "" {
		out = append(out, "err", err)
	}
	return out
}

// Run 保持隧道可用，断线自动重连，直到 ctx 结束。
//
// 网卡与路由在整个过程中保持不动：隧道断掉时靠「原默认路由仍在、只是 metric 更高」
// 自动回落到本地线路，不会整机断网。
func (t *Tunnel) Run(ctx context.Context) error {
	// 内部 ctx：读循环挂掉时取消它，把正在跑的会话一起叫停。
	//
	// 不让 readPump 自己 return 就完事的原因：控制连接还在收发心跳，面板显示在线、
	// 延迟也正常，而上行早就停止搬运了 —— 用户看到的是「一连上但什么都打不开」。
	runCtx, cancelRead := context.WithCancel(ctx)
	defer cancelRead()

	// 唯一的网卡读循环：整个 Tunnel 生命周期只起这一次
	go t.readPump(runCtx, cancelRead)

	attempt := 0
	for {
		// 读循环挂掉要优先处理，不能让「控制连接还在线」掩盖上行停摆
		if rerr := t.takeReadError(); rerr != nil {
			if t.RebuildDevice != nil {
				t.log.Warn("网卡读取失败（上行搬运已停），自动重建设备并重连", "err", rerr)
				go t.RebuildDevice()
				return nil
			}
			t.log.Error("网卡读取失败且无法重建设备，隧道退出", "err", rerr)
			return rerr
		}
		if ctx.Err() != nil {
			return nil
		}
		connected, err := t.session(runCtx)
		if ctx.Err() != nil {
			return nil
		}
		if connected {
			// 成功握手过就重置退避，避免一次偶发断线让后续重连一直等 3 分钟
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
		case <-runCtx.Done():
			// 读循环挂了：回到循环顶部走错误处理，不要在这里当成一次普通断线
			continue
		case <-t.wake:
			// 网络出口刚迁移完（watcher 通知），立刻重试而不是干等退避计时
		case <-time.After(Backoff(attempt)):
		}
		if attempt < 10 {
			attempt++
		}
	}
}

// UDPState 报告 UDP 数据面的状态。
//
// enabled=false 表示这条隧道走的是 TCP 成员连接（healthy 无意义）。
// enabled=true 时 healthy 反映 UDP 心跳是否还在 udpDeadAfter 内 —— 控制连接再好，
// 只要它 false，业务包就已经发不出去了，界面不能继续报「已连接」。
func (t *Tunnel) UDPState() (enabled, healthy bool) {
	u := t.udpCh.Load()
	if u == nil {
		return false, false
	}
	if u.isClosed() {
		return true, false
	}
	idle := time.Since(time.Unix(0, u.lastRx.Load()))
	return true, idle < udpDeadAfter
}

// takeReadError 非阻塞取读循环的致命错误（没有就返回 nil）。
func (t *Tunnel) takeReadError() error {
	select {
	case err := <-t.readErr:
		return err
	default:
		return nil
	}
}

// readPump 是全局唯一的网卡读循环。
//
// 它不属于任何会话：读到包就塞进 pktCh，由当前会话取走。这样会话怎么重拨都不会
// 积累旧 reader，也不会有旧会话抢走新会话的首包。读失败要上报 —— 设备坏了，重拨
// TCP 救不了，得靠 App 的完整重连（RebuildDevice）。
func (t *Tunnel) readPump(ctx context.Context, onFatal context.CancelFunc) {
	buf := make([]byte, readBufferSize)
	report := func(err error) {
		// 带缓冲 1 且只塞一次：读循环随后就退出，不会再写
		select {
		case t.readErr <- err:
		default:
		}
		// 叫停会话循环，让 Run 立刻看到这条错误（否则它会一直等到连接出问题）
		onFatal()
	}
	for {
		n, err := t.dev.Read(buf)
		if err != nil {
			if ctx.Err() != nil {
				return // 外层取消：正常收尾，不当作故障上报
			}
			// 我们自己的 Close 是正常收尾（比如用户断开），不触发重连
			if errors.Is(err, ErrDeviceClosed) {
				return
			}
			report(err)
			return
		}
		if n <= 0 {
			continue
		}
		if n > proto.MaxPayload {
			t.log.Warn("网卡读到的包超过帧上限，已丢弃", "len", n, "max", proto.MaxPayload)
			continue
		}
		pkt := make([]byte, n)
		copy(pkt, buf[:n])
		// 非阻塞投递：pktCh 满了宁可丢这个包，也不能把网卡读循环卡住。
		//
		// 阻塞的话，下游一旦慢下来（写超时、调度抖动、高流量积累），readPump 就停在
		// 这里不再读网卡 —— Wintun 的 ring 很快被内核填满，之后上行发包全部失败，
		// 表现就是「用着用着突然没网、断了重连才恢复」。IP 层本就尽力而为，丢一个包
		// 让内层 TCP 重传，比整条上行停摆好得多。
		select {
		case t.pktCh <- pkt:
		case <-ctx.Done():
			return
		default:
			if n := t.pktDrops.Add(1); n == 1 || n%1000 == 0 {
				t.log.Warn("上行队列已满，丢弃网卡读到的包（内层 TCP 会重传）", "drops", n)
			}
		}
	}
}

// session 建立一次完整会话并在其上搬运数据，返回是否成功握过手。
func (t *Tunnel) session(ctx context.Context) (bool, error) {
	tlsCfg, err := pki.ClientTLSConfig(t.inv)
	if err != nil {
		return false, err
	}

	// dial、TLS 握手、等 HELLO_ACK 三段共用一个总预算：三段各给 30 秒的话最坏会拖到 90 秒，
	// 而重连节奏（keepConnected 的 30 秒一轮）是按「一轮多久」安排的。
	// 不能只靠 defer cancel —— 下面要跑 sess.run 长循环，预算必须在它之前就释放。
	hctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()

	dialer := &net.Dialer{}
	raw, err := dialer.DialContext(hctx, "tcp", t.inv.Server)
	if err != nil {
		return false, fmt.Errorf("连接 %s: %w", t.inv.Server, err)
	}
	conn := tls.Client(raw, tlsCfg)

	if err := conn.HandshakeContext(hctx); err != nil {
		raw.Close()
		return false, fmt.Errorf("TLS 握手（证书或网络问题）: %w", err)
	}
	defer conn.Close()

	// 多连接并发数据面：sid 是本次逻辑会话的团队编号，成员连接靠它认亲。
	// members 请求来自连接码（0/1=单连接）；服务端在 ACK 里批准实际值。
	sid := newSessionID()
	// UDP 模式只要一条控制连接：数据包全走 UDP，成员连接没有意义
	wantMembers := effectiveMembers(t.inv.Members)
	if t.inv.UseUDP() {
		wantMembers = 1
	}

	hello, err := json.Marshal(map[string]any{
		"version": protoVersion, "client": t.inv.Name,
		"sid": sid, "members": wantMembers, "member": 0,
	})
	if err != nil {
		return false, err
	}
	if dl, ok := hctx.Deadline(); ok {
		_ = conn.SetWriteDeadline(dl)
	}
	if err := proto.WriteFrame(conn, proto.TypeHello, hello); err != nil {
		return false, fmt.Errorf("发 HELLO: %w", err)
	}
	_ = conn.SetWriteDeadline(time.Time{})

	if dl, ok := hctx.Deadline(); ok {
		_ = conn.SetReadDeadline(dl)
	}
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
	// 一条说完建连结果。sid / gateway / mtu 不再往界面写：sid 是 32 位 hex，
	// 用户看不懂，排查时有日志文件里的 Debug 明细；gateway 和 mtu 由服务端配置决定，
	// 出问题时 `antapp-linkd status` 更权威。
	connLine := fmt.Sprintf("已连接 %s", t.inv.Server)
	if ack.TunnelIP != "" {
		connLine += "  隧道IP=" + ack.TunnelIP
	}
	if ack.Members > 1 {
		connLine += fmt.Sprintf("  %d 条并行连接", ack.Members)
	}
	t.log.Info(connLine)
	t.Stats.Connected.Store(true)
	defer t.Stats.Connected.Store(false)

	sess := newClientSession(t, conn, sid, tlsCfg, ack)
	defer sess.closeAll()
	sess.trace = t.Trace

	// UDP 模式：数据包改走 UDP。控制连接继续跑心跳与保活，并负责把本地 UDP 端口报给
	// 服务端（服务端按 TCP 远端 IP + 这个端口认人，不接受 UDP 包里自称的地址）。
	// 启动失败不算致命 —— 退回 TCP 数据通道照样能用，只是慢。
	if t.inv.UseUDP() {
		u, uerr := startUDPChannel(ctx, t.inv, conn.ConnectionState(), sess.dev, t.log, sess.stats,
			func(port int) error {
				var p [2]byte
				binary.BigEndian.PutUint16(p[:], uint16(port))
				return sess.write(proto.TypeUDPPort, p[:])
			})
		if uerr != nil {
			// UDP 模式就是要走 UDP：通道建不起来就明确失败，不偷偷退回 TCP。
			//
			// 退回 TCP 会把「UDP 端口没放行」伪装成「一切正常」—— 用户以为在用 UDP，
			// 实际跑的是 TCP，既违背「纯 UDP、不自动切换」的设计，也让故障无法定位。
			// 报错后连接失败，用户能直接看到「UDP 通道不可用」，自己决定放行端口，
			// 或换一张 TCP 连接码。
			return true, fmt.Errorf("UDP 数据通道不可用（%s 模式下数据只走 UDP，不回退 TCP）: %w",
				t.inv.Mode, uerr)
		}
		sess.udp = u
		// 让界面能查数据面状态；会话结束时清掉，别让下一个会话看到旧通道
		t.udpCh.Store(u)
		defer func() {
			t.udpCh.Store(nil)
			u.close()
		}()
		t.log.Info("UDP 数据通道已启用（控制仍走 TCP）",
			"local_port", u.conn.LocalAddr().(*net.UDPAddr).Port)
	}

	cancel() // 建连结束：后面的读循环与保活只受外层 ctx 约束
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
	if t.inv.UseUDP() {
		// 服务端可能是老版本或没按 udp 模式回 ack，这里强制单连接：UDP 模式下建成员
		// 连接纯属浪费，而且数据本就不该再分到槽位上。
		n = 1
	}
	sess := &clientSession{
		tunnel: t, dev: t.dev, log: t.log, stats: &t.Stats,
		sid: sid, members: n, tlsCfg: tlsCfg, closed: make(chan struct{}),
		slots:  make([]net.Conn, n),
		writes: make([]sync.Mutex, n),
		flow:   proto.NewFlowTable(),
	}
	sess.slots[0] = conn
	return sess
}

type clientSession struct {
	tunnel *Tunnel
	dev    Device
	log    *slog.Logger
	stats  *Stats
	// ctrlSeen 只由控制连接（槽 0）的收帧刷新。判死必须看会话命脉那一条连接：
	// 成员连接的下行数据不刷新它，否则控制连接单向半开时，只要成员槽还有流量
	// 就永远不判死，哈希到槽 0 的内层流全部黑洞。
	ctrlSeen atomic.Int64

	sid     string // 逻辑会话 ID
	members int    // 生效连接数（含控制连接）
	tlsCfg  *tls.Config
	trace   *OrderTracer    // 联调顺序追踪（nil = 关闭）
	ctx     context.Context // 会话上下文（run 里赋值），收尾时用来判断写失败是否预期内

	// memberDrops 是本会话成员连接被摘掉的次数（含主动摘除与对端断开）。
	// 日志里带上它，用户一眼能看出「这一晚掉了几次」，不用数滚动条。
	memberDrops atomic.Int32

	// emptySlotDrops 是「所有成员槽都不可用、上行批次被迫丢弃」的批次数。
	// 正常情况下它一直是 0；涨起来就说明成员全断了在等补拨，业务正在黑洞。
	emptySlotDrops atomic.Int64

	// closed 是本会话的关闭信号：run 返回后由 closeAll 关闭，让还阻塞在设备读上、
	// 或正要往旧连接写包的 goroutine 能自己退出。设备跨会话复用、不能靠关设备来收尾。
	closed    chan struct{}
	closeOnce sync.Once

	mu     sync.Mutex
	slots  []net.Conn   // slots[0]=控制连接；成员槽位从 1 开始，nil=空
	writes []sync.Mutex // 每槽一把写锁；成员槽空时流量落回控制连接，锁也跟着落回
	// flow 决定上行包走哪个槽：已有流固定不变（保序），新流分给当前承载流数最少的槽。
	// 纯哈希做不到均衡，一条大流会把某条成员连接塞到写超时被摘掉。
	flow *proto.FlowTable

	// udp 是 UDP 数据通道（mode=udp 且启动成功时才有）。非 nil 时上行数据不走槽位。
	udp *udpChannel
}

// sessionEnded 报告会话是否已进入收尾（此时连接会随会话一起关，写失败是预期内的）。
func (s *clientSession) sessionEnded() bool {
	return s.ctx != nil && s.ctx.Err() != nil
}

// closeAll 关掉会话的全部连接，把阻塞在 ReadFrame 上的读循环放出来。
//
// 槽 0 只关不清空：writeSlot/slot 在槽 0 为 nil 时返回的是 nil 接口，随后的
// SetWriteDeadline 或 ReadFrame 会 panic；留一个已关闭的连接，读写一样立刻失败。
func (s *clientSession) closeAll() {
	s.closeOnce.Do(func() {
		close(s.closed)

		s.mu.Lock()
		members := make([]net.Conn, 0, len(s.slots))
		for i := 1; i < len(s.slots); i++ {
			if s.slots[i] != nil {
				members = append(members, s.slots[i])
				s.slots[i] = nil
			}
		}
		ctrl := s.slots[0]
		s.mu.Unlock()

		// 关闭放在锁外：Close 可能阻塞，不能压着槽表
		for _, c := range members {
			_ = c.Close()
		}
		if ctrl != nil {
			_ = ctrl.Close()
		}
	})
}

// isClosed 报告会话是否已收尾。
func (s *clientSession) isClosed() bool {
	select {
	case <-s.closed:
		return true
	default:
		return false
	}
}

// slotWriteTimeout 限制单次写连接。卡住的连接必须能摘掉，否则会拖停整条分发链路。
//
// 20 秒而不是 5 秒：上行很满、链路实测有 30% 级别丢包时，TCP 发送窗口会长时间处于
// 满的状态，5 秒不够把一批写完，成员连接会被反复误杀。
var slotWriteTimeout = 20 * time.Second

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

// writeSlotData 与 writeSlot 一样取槽，但空槽返回 nil 而不落回控制连接。
// 数据批必须走这条：成员断一条，本该发它的包若灌进跑心跳的槽 0，PING/PONG 就要排队，
// 面板延迟立刻从两百毫秒涨到上千（服务端那份 flushBatch 踩过同一个坑）。
// k == 0 仍然合法 —— 单连接模式下所有流量本来就在控制连接上。
func (s *clientSession) writeSlotData(k int) (int, net.Conn, *sync.Mutex) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if k < 0 || k >= len(s.slots) || s.slots[k] == nil {
		return 0, nil, nil
	}
	return k, s.slots[k], &s.writes[k]
}

// slotAlive 报告槽位 k 此刻是否有连接。FlowTable 靠它避开空槽 —— 分给空槽的新流下行
// 会被 flushBatch 整批丢掉，那条流就再也不通了，而且流表不会自我纠正。
func (s *clientSession) slotAlive(k int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return k > 0 && k < len(s.slots) && s.slots[k] != nil
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

// pingMember 往指定成员槽写一个心跳帧保活。
//
// 持该槽的写锁：同一条连接上还有数据批在写（flushBatch），裸写会把帧撕开。
func (s *clientSession) pingMember(k int) error {
	actual, conn, mu := s.writeSlot(k)
	if actual != k {
		// 槽已空，流量落回控制连接，没什么可保活的
		return nil
	}
	mu.Lock()
	defer mu.Unlock()
	if err := conn.SetWriteDeadline(time.Now().Add(slotWriteTimeout)); err != nil {
		return err
	}
	ts := make([]byte, 8)
	binary.BigEndian.PutUint64(ts, uint64(time.Now().UnixNano()))
	return proto.WriteFrame(conn, proto.TypePing, ts)
}

// memberHeartbeat 给一条成员连接保活，直到会话结束、这条连接写不动，或它的槽位
// 已经被摘掉/换掉。
//
// 服务端会回 PONG（老服务端会忽略，但上行有包就已经能刷新 NAT 映射了）。
func (s *clientSession) memberHeartbeat(ctx context.Context, k int, conn net.Conn) {
	ticker := time.NewTicker(memberPingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.closed:
			return
		case <-ticker.C:
			// 槽位已经不是自己了（被摘除、或维护协程换了新连接）：退出。
			// 不检查的话这条协程会一直空转到会话结束 —— pingMember 在空槽上返回
			// nil，循环因此永不退出，反复掉线就积累一堆死心跳。
			if s.slotConn(k) != conn {
				return
			}
			if err := s.pingMember(k); err != nil {
				// 写失败不在这里收尾：读循环会摘槽、维护协程会补拨
				return
			}
		}
	}
}

var (
	// memberPingInterval 是成员连接的心跳间隔。
	//
	// 成员连接上只有数据帧、没有控制帧：流量稀疏时它会长时间没有包往返，
	// 跨境链路的 NAT 会把这种连接的状态清掉，客户端随后读到 i/o timeout、
	// 摘槽重拨（实测 3 条数据连接时每 1~2 分钟一次）。定期发一个小帧保活，
	// 比事后重拨便宜得多。取 15s 是为了短于常见 NAT 超时（30s 起）。
	//
	// 是 var 而不是 const 只为让测试能临时调小它；生产路径没人写它。
	memberPingInterval = 15 * time.Second
)

// slotConn 返回槽位 k 上此刻的连接；空槽或越界返回 nil（不像 slot 那样兜底到控制连接）。
func (s *clientSession) slotConn(k int) net.Conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	if k < 0 || k >= len(s.slots) {
		return nil
	}
	return s.slots[k]
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
	// 建连这三段共用一个总预算（见 memberHandshakeTimeout）。进入读循环前必须让它失效，
	// 并且保活与关闭只认外层 ctx —— 否则预算一到，会把一条已经正常工作的成员连接关掉。
	hctx, cancelBudget := context.WithTimeout(ctx, memberHandshakeTimeout)
	defer cancelBudget() // 兜底：任何提前 return 都要把预算放掉

	dialer := &net.Dialer{}
	raw, err := dialer.DialContext(hctx, "tcp", t.inv.Server)
	if err != nil {
		return fmt.Errorf("成员连接 %d 拨号: %w", k, err)
	}
	conn := tls.Client(raw, sess.tlsCfg)
	// 握手失败、被拒、读循环退出、ctx 结束都收口到这一处：不关连接就会漏 fd，
	// 读循环也会永远卡在 ReadFrame 上
	defer conn.Close()
	defer closeOnDone(ctx, conn)()

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
	// HELLO 的写也要吃总预算：高丢包/零窗口下 write 能长时间阻塞，而 hctx 到期并不会
	// 关掉连接（closeOnDone 监听的是外层 ctx），只靠它封不住这一段。
	if dl, ok := hctx.Deadline(); ok {
		_ = conn.SetWriteDeadline(dl)
	}
	if err := proto.WriteFrame(conn, proto.TypeHello, hello); err != nil {
		return err
	}
	_ = conn.SetWriteDeadline(time.Time{})
	// 等两个 ACK 也吃同一个总预算：用 hctx 的截止时刻，而不是再起一段独立超时
	if dl, ok := hctx.Deadline(); ok {
		_ = conn.SetReadDeadline(dl)
	}
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
	// 建连结束：立刻把预算放掉，后面的读循环与保活只受外层 ctx 约束
	cancelBudget()
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
	// 只在整个会话凑齐时打一条。三条「成员连接已就位 member=1/2/3」对用户没有意义，
	// 他关心的是「几条都连上了没有」。
	if live := sess.liveCount(); live >= sess.members {
		t.log.Info(fmt.Sprintf("%d 条连接全部就位", live))
	}

	// 这条连接要自己保活（理由见 memberPingInterval）：会话收尾或写不动时退出
	go sess.memberHeartbeat(ctx, k, conn)

	// 读循环：成员连接上来的包写网卡；断开摘槽（该槽流量落回控制连接）
	for {
		typ, payload, err := proto.ReadFrame(conn)
		if err != nil {
			sess.mu.Lock()
			if sess.slots[k] == conn {
				sess.slots[k] = nil
			}
			sess.mu.Unlock()
			reason := connBrokenReason(err)
			if reason == "本端关闭" {
				// dropSlot 主动摘掉的连接：写那侧已经 Warn 过，这里只留 Debug，
				// 否则同一件事会打两条（一条信息一条警告），读起来像两个故障。
				t.log.Debug("成员连接已摘除", "member", k)
			} else {
				n := sess.memberDrops.Add(1)
				t.log.Warn("成员连接断开", "member", k, "reason", reason,
					"drops", n, "live", sess.liveCount())
			}
			return err
		}
		switch typ {
		case proto.TypeIP:
			sess.trace.Record("rx", k, payload)
			if _, err := sess.dev.Write(payload); err != nil {
				// 这条读循环马上要退出，槽位必须跟着摘掉：留着的话分发仍会哈希到它，
				// 包只会写进这条再没人读的连接（和上面读错误路径同一个理由）。
				sess.mu.Lock()
				if sess.slots[k] == conn {
					sess.slots[k] = nil
				}
				sess.mu.Unlock()
				return err
			}
			sess.stats.RxBytes.Add(uint64(len(payload)))
		case proto.TypeBye:
			// 服务端拒绝了这条成员连接，或正在关它：立刻摘槽，别继续往里写
			sess.mu.Lock()
			if sess.slots[k] == conn {
				sess.slots[k] = nil
			}
			sess.mu.Unlock()
			t.log.Warn("成员连接被服务端关闭", "member", k, "reason", string(payload))
			return fmt.Errorf("成员连接 %d 被服务端关闭: %s", k, string(payload))
		case proto.TypePong:
			// 心跳得到应答，说明这条连接还活着；不需要别的动作
		default:
			// 成员连接只搬数据，其他帧忽略
		}
	}
}

// memberStagger 是成员连接首次拨号之间的间隔。
//
// 刻意不并发建：几条 TLS 在几十毫秒内挤向同一个 IP:端口是很好认的行为特征，
// 正常客户端不会这样。错开也不影响可用性 —— 成员没就位时数据走控制连接。
const memberStagger = 400 * time.Millisecond

// memberRetryDelay 是补拨成员连接前的等待。
//
// 首次（含断开后的第一次补拨）不等待：单条成员连接断掉时数据还能走其他槽，越快补回
// 越好 —— 让它干等十秒没有任何好处，只会让多连接长时间退化成少连接、吞吐直接掉。
// 只有连续失败才退避，而且用比会话级重连短得多的档位（1s 起、封顶 15s）：这里退避的
// 代价是吞吐，不像整条隧道断开那样需要给对端留恢复时间。
func memberRetryDelay(attempt int) time.Duration {
	if attempt <= 0 {
		return 0
	}
	if attempt > 5 {
		attempt = 5
	}
	d := time.Second << (attempt - 1) // 1s 2s 4s 8s 16s
	if d > 15*time.Second {
		return 15 * time.Second
	}
	return d
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
			// 只在首次建连时错开；断开后的补拨走退避 —— 那是恢复路径，不快不行。
			if k > 1 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(memberStagger * time.Duration(k-1)):
				}
			}
			attempt := 0
			for {
				// 首次与断开后的第一次补拨都不等待（memberRetryDelay(0) == 0）；
				// 只有连续失败才退避。
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
					// 前几次留 Debug：刚建连时成员还在建，失败一两次是正常的。
					// 但持续补不上就必须 Warn —— 那种状态下控制连接还活着、面板显示
					// 「已连接」，而上行数据全被 emptySlotDrops 丢掉，用户只觉得
					// 「突然没网」，日志里却一条线索都没有。
					if attempt < 3 {
						t.log.Debug("成员连接补拨失败", connLogAttrs(err, nil,
							"member", k, "retry_in", memberRetryDelay(attempt+1))...)
					} else if attempt == 3 || attempt%10 == 0 {
						t.log.Warn("成员连接一直补不上，上行数据正在被丢弃",
							connLogAttrs(err, nil, "member", k, "attempts", attempt+1,
								"live", sess.liveCount(), "target", sess.members)...)
					}
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
	s.touchCtrl()

	errCh := make(chan error, 3)
	go func() { errCh <- s.pumpFromTunnel(ctx) }()
	go func() { errCh <- s.pumpFromDevice(ctx) }()
	go func() { errCh <- s.heartbeat(ctx) }()
	go s.watchDataPath(ctx)
	if s.members > 1 {
		// 不单独打「多连接模式：目标 N 条，启动成员维护」—— 上面「已连接 … N 条并行连接」
		// 已经说过了，用户不需要知道内部有个维护协程。
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

// touchCtrl 记下控制连接最近一次收到帧的时间（心跳判死的唯一依据）。
func (s *clientSession) touchCtrl() { s.ctrlSeen.Store(time.Now().UnixNano()) }

// watchDataPath 定期检查「数据面还在不在动」。
//
// 控制连接活着不代表数据面通：控制走 slot 0、业务数据走成员连接，成员全挂时面板
// 照样显示「已连接」，而所有上行都被 emptySlotDrops 丢掉 —— 用户看到的就是「用着
// 用着突然没网，断开重连才好」，日志里却一条线索都没有（成员补拨失败只记 Debug）。
//
// 这里只观察、不改行为，把两种可疑状态写进日志：
//   - 有上行却长时间收不到任何下行（下行那条路断了）
//   - 成员连接持续不足（上行正在被丢）
func (s *clientSession) watchDataPath(ctx context.Context) {
	const sample = 30 * time.Second
	ticker := time.NewTicker(sample)
	defer ticker.Stop()

	var lastTx, lastRx uint64
	var noRxSamples, lowMemberSamples int

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tx, rx := s.stats.TxBytes.Load(), s.stats.RxBytes.Load()
			live := s.liveCount()

			// TxBytes 只统计业务包（心跳走 TypePing 不计入），所以「有上行没下行」
			// 是一个干净的信号：用户在发包，但对端一个字节都没回来。
			if tx > lastTx && rx == lastRx {
				noRxSamples++
			} else {
				noRxSamples = 0
			}

			if s.members > 1 && live < s.members {
				lowMemberSamples++
			} else {
				lowMemberSamples = 0
			}

			// 连续两个采样（60 秒）才报，避免短暂波动吵人；之后每 5 分钟提醒一次
			if noRxSamples == 2 || (noRxSamples > 2 && noRxSamples%10 == 0) {
				s.log.Warn("数据面单向：有上行但一直收不到下行",
					"members", fmt.Sprintf("%d/%d", live, s.members),
					"empty_slot_drops", s.emptySlotDrops.Load(),
					"member_drops", s.memberDrops.Load(),
					"tx_mb", tx/1024/1024, "rx_mb", rx/1024/1024)
			}
			if lowMemberSamples == 2 || (lowMemberSamples > 2 && lowMemberSamples%20 == 0) {
				s.log.Warn("成员连接持续不足，上行数据正在被丢弃",
					"members", fmt.Sprintf("%d/%d", live, s.members),
					"empty_slot_drops", s.emptySlotDrops.Load(),
					"member_drops", s.memberDrops.Load())
			}

			lastTx, lastRx = tx, rx
		}
	}
}

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
		s.touchCtrl()
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

// pumpFromDevice：把网卡上出现的包（内核要发往外网的包）送进隧道。
//
// 包来自 Tunnel 的全局读循环（pktCh），这里不再直接碰设备 —— 会话重拨时这条
// goroutine 能被 ctx 立刻叫醒，不会像以前那样卡在阻塞的 dev.Read 上、非要等下一个
// 包到来才退出（那一等就积累旧 reader，还会抢新会话的包）。
//
// 写合并：首包选定槽位后，非阻塞地从 pktCh 继续收割同槽的现成包，拼成单缓冲一次
// 写出——外层 TLS 记录数量减半。收不到就停，绝不等待。
func (s *clientSession) pumpFromDevice(ctx context.Context) error {
	batch := make([]byte, 0, proto.MaxBatchBytes)
	for {
		if ctx.Err() != nil {
			return nil
		}
		var pkt []byte
		select {
		case <-ctx.Done():
			return nil
		case pkt = <-s.tunnel.pktCh:
		}
		if s.isClosed() {
			return nil
		}
		pktLen := len(pkt)
		// UDP 数据通道：一个 IP 包就是一个 UDP 包，不分槽、不做写合并。
		// 丢了就丢了 —— 内层应用自己的 TCP 会重传。
		if s.udp != nil {
			s.trace.Record("tx", 0, pkt)
			s.stats.TxBytes.Add(uint64(pktLen))
			if err := s.udp.send(pkt); err != nil {
				if s.udp.isClosed() {
					// keepalive 已经判定这条 UDP 通道失效并关掉了它。结束会话让 Run
					// 重建 —— 重建出来的仍是 UDP 通道，不切 TCP。
					return fmt.Errorf("UDP 数据通道已失效（准备重建）: %w", err)
				}
				s.log.Debug("UDP 上行发送失败", "err", err)
			}
			continue
		}
		k := s.flow.Slot(pkt, s.members, s.slotAlive)
		s.trace.Record("tx", k, pkt)
		s.stats.TxBytes.Add(uint64(pktLen))

		batch = proto.AppendHeader(batch[:0], proto.TypeIP, pktLen)
		batch = append(batch, pkt...)

		// 收割同槽的现成包：不等待、批量不超上限
		for len(batch) < proto.MaxBatchBytes-proto.HeaderSize-proto.MaxPayload {
			var more []byte
			select {
			case more = <-s.tunnel.pktCh:
			default:
			}
			if more == nil {
				break
			}
			kk := s.flow.Slot(more, s.members, s.slotAlive)
			s.trace.Record("tx", kk, more)
			if kk != k {
				if err := flushBatch(s, k, batch); err != nil {
					return err
				}
				k = kk
				batch = batch[:0]
			}
			batch = proto.AppendHeader(batch, proto.TypeIP, len(more))
			batch = append(batch, more...)
			s.stats.TxBytes.Add(uint64(len(more)))
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
	actual, conn, mu := s.writeSlotData(k)
	if conn == nil {
		// 槽空（成员断了或正在补）：丢掉这批，等成员回来。绝不能落回控制连接 ——
		// 那是心跳通道，数据挤上去面板延迟立刻上千（实测过）。
		//
		// 前 200 批只进 Debug：隧道刚建立时成员连接还在建（实测 1~2 秒），这段时间
		// 丢几批是正常的，内层 TCP 会重传。打成 Warn 的话每次重连都亮一个 ▲，
		// 用户会以为出了故障（已经有人这么问过）。
		//
		// 持续丢到 200 批以上才 Warn —— 那说明成员是真的补不上，不是启动瞬间。
		n := s.emptySlotDrops.Add(1)
		if n == 200 || (n > 200 && n%1000 == 0) {
			s.log.Warn("成员槽长时间不可用，上行数据持续丢弃（内层 TCP 会重传）",
				"slot", k, "dropped_batches", n)
		}
		return nil
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
			// 一条成员连接出问题不该重启整条隧道：摘掉它，让维护协程补一条新的。
			// dropSlot 会关连接、读循环随后也会失败，但那边按 reason=="本端关闭"
			// 降级成 Debug，所以断开这件事只在这里 Warn 一次。
			s.dropSlot(actual, conn)
			n := s.memberDrops.Add(1)
			s.log.Warn("成员连接写不动，已摘除", "slot", actual,
				"reason", connBrokenReason(werr), "drops", n)
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
			if idle := time.Since(time.Unix(0, s.ctrlSeen.Load())); idle > deadAfter {
				return fmt.Errorf("控制连接超过 %s 没收到任何帧，判定链路已死", deadAfter)
			}
			if err := send(); err != nil {
				return fmt.Errorf("发 PING: %w", err)
			}
		}
	}
}
