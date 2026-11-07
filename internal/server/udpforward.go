package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// UDP 端口转发：把转发端口段上的 UDP 流量经隧道送到节点机同端口。
//
// TCP 的转发在 relay_linux.go 里；UDP 单独一套（UDP socket 不能和 TCP 共用监听）。
// 隧道本身仍是 TCP（TLS），UDP 包进 TUN 后由节点机内核路由到本机服务 —— 客户端
// 不需要为 UDP 做任何改动。
//
// 取舍：
//   - 每个「外部来源 IP:端口」一条会话，各自 DialUDP 到节点机同端口；UDP 无连接，
//     复用同一个 sock 会让不同来源的回包串味。
//   - 会话 60 秒无包回收，防止公网扫描把会话表撑爆（TCP 转发那边有同款清扫）。
//   - 节点机不在线时直接丢弃，不去 Dial 一个没人接的地址。
var (
	udpForwardIdleTimeout = 60 * time.Second
	udpForwardSweepEvery  = 15 * time.Second
	udpForwardMaxSessions = 256
)

// udpForwardCounters 是 UDP 转发的累计计数（多端口共用），写进状态文件供 status 展示。
var udpForwardCounters struct {
	rxPackets, rxBytes, txPackets, txBytes atomic.Uint64
}

// udpForwardSnapshot 返回 UDP 转发累计收发包数与字节数；一次都没转发过时 ok=false。
func udpForwardSnapshot() (packets, bytes uint64, ok bool) {
	rxP, txP := udpForwardCounters.rxPackets.Load(), udpForwardCounters.txPackets.Load()
	if rxP+txP == 0 {
		return 0, 0, false
	}
	return rxP + txP, udpForwardCounters.rxBytes.Load() + udpForwardCounters.txBytes.Load(), true
}

type udpForward struct {
	listen   *net.UDPConn
	target   *net.UDPAddr
	port     int
	log      *slog.Logger
	tunnelUp func() bool

	mu       sync.Mutex
	sessions map[string]*udpForwardSession
	closed   bool
}

type udpForwardSession struct {
	conn *net.UDPConn
	peer *net.UDPAddr
	last atomic.Int64 // unix nano
}

// startUDPForward 在转发端口段的每个端口上监听 UDP。
// 个别端口监听失败只记日志继续（和 TCP 转发一样的兜底），全部失败才返回错误。
func startUDPForward(ctx context.Context, cfg Config, log *slog.Logger, tunnelUp func() bool) (func(), error) {
	if cfg.ForwardPorts.End < cfg.ForwardPorts.Start {
		return func() {}, nil
	}
	clientIP := net.ParseIP(cfg.Tunnel.ClientIP)
	if clientIP == nil {
		return func() {}, errors.New("隧道客户端地址无效")
	}
	var forwards []*udpForward
	var firstErr error
	for port := cfg.ForwardPorts.Start; port <= cfg.ForwardPorts.End; port++ {
		f, err := newUDPForward(port, port, clientIP, log, tunnelUp)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			log.Warn("UDP 转发端口监听失败", "port", port, "err", err)
			continue
		}
		forwards = append(forwards, f)
	}
	if len(forwards) == 0 {
		return func() {}, firstErr
	}

	var wg sync.WaitGroup
	for _, f := range forwards {
		wg.Add(1)
		go func(f *udpForward) {
			defer wg.Done()
			f.run(ctx)
		}(f)
	}
	stop := func() {
		for _, f := range forwards {
			f.close()
		}
		wg.Wait()
	}
	return stop, nil
}

func newUDPForward(listenPort, targetPort int, targetIP net.IP, log *slog.Logger, tunnelUp func() bool) (*udpForward, error) {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{Port: listenPort})
	if err != nil {
		return nil, err
	}
	return &udpForward{
		listen:   conn,
		target:   &net.UDPAddr{IP: targetIP, Port: targetPort},
		port:     listenPort,
		log:      log,
		tunnelUp: tunnelUp,
		sessions: make(map[string]*udpForwardSession),
	}, nil
}

func (f *udpForward) run(ctx context.Context) {
	go func() {
		<-ctx.Done()
		f.close()
	}()
	go f.sweepLoop(ctx)

	// 转发的是节点机原生 UDP 数据报，上限 65507；不能用隧道包上限（1600）
	// 当读缓冲，否则大包会被截断。
	buf := make([]byte, 65535)
	for {
		n, from, err := f.listen.ReadFromUDP(buf)
		if err != nil {
			f.mu.Lock()
			closed := f.closed
			f.mu.Unlock()
			if !closed {
				f.log.Debug("UDP 转发读结束", "port", f.port, "err", err)
			}
			return
		}
		if n <= 0 {
			continue
		}
		if f.tunnelUp != nil && !f.tunnelUp() {
			continue
		}
		f.dispatch(from, buf[:n])
	}
}

// dispatch 找到（或建立）外部来源对应的会话，把包写给节点机。
// 会话的 Dial 放在这里同步做：包到达时 conn 必须已经可用，否则第一批包会丢。
func (f *udpForward) dispatch(from *net.UDPAddr, pkt []byte) {
	key := from.String()
	f.mu.Lock()
	sess := f.sessions[key]
	if sess == nil {
		if len(f.sessions) >= udpForwardMaxSessions {
			f.mu.Unlock()
			return
		}
		conn, err := net.DialUDP("udp", nil, f.target)
		if err != nil {
			f.mu.Unlock()
			f.log.Debug("UDP 转发拨节点机失败", "port", f.port, "err", err)
			return
		}
		sess = &udpForwardSession{conn: conn, peer: from}
		sess.touch()
		f.sessions[key] = sess
		f.mu.Unlock()
		go f.sessionReadLoop(key, sess)
	} else {
		f.mu.Unlock()
	}
	sess.touch()
	if _, err := sess.conn.Write(pkt); err != nil {
		f.log.Debug("UDP 转发写节点机失败", "port", f.port, "err", err)
	} else {
		udpForwardCounters.rxPackets.Add(1)
		udpForwardCounters.rxBytes.Add(uint64(len(pkt)))
	}
}

// udpForwardBufCount 是同时可用的会话读缓冲上限。每块 64KB，512 块约 32MB 常驻，
// 这就是 UDP 转发的内存峰值上界：并发会话超过它时宁可丢回包，也不无限分配。
const udpForwardBufCount = 512

var (
	udpForwardBufPoolOnce sync.Once
	udpForwardBufPool     chan *[]byte
)

// udpForwardBufs 惰性预填缓冲池。服务端没启用 UDP 转发时不会为此分配 32MB。
func udpForwardBufs() chan *[]byte {
	udpForwardBufPoolOnce.Do(func() {
		udpForwardBufPool = make(chan *[]byte, udpForwardBufCount)
		for i := 0; i < udpForwardBufCount; i++ {
			b := make([]byte, 65535)
			udpForwardBufPool <- &b
		}
	})
	return udpForwardBufPool
}

func getUDPForwardBuf() (*[]byte, bool) {
	select {
	case b := <-udpForwardBufs():
		return b, true
	default:
		return nil, false
	}
}

func putUDPForwardBuf(b *[]byte) {
	select {
	case udpForwardBufs() <- b:
	default:
	}
}

// sessionReadLoop 把节点机的回包转发回外部来源；连接出错即回收会话。
func (f *udpForward) sessionReadLoop(key string, sess *udpForwardSession) {
	bufp, ok := getUDPForwardBuf()
	if !ok {
		// 并发会话已到缓冲上限：拒绝为这条会话服务，避免内存无界增长。
		f.removeSession(key, sess)
		return
	}
	defer putUDPForwardBuf(bufp)
	buf := *bufp
	for {
		_ = sess.conn.SetReadDeadline(time.Now().Add(udpForwardIdleTimeout))
		n, err := sess.conn.Read(buf)
		if err != nil {
			f.removeSession(key, sess)
			return
		}
		if n <= 0 {
			continue
		}
		sess.touch()
		if _, err := f.listen.WriteToUDP(buf[:n], sess.peer); err != nil {
			f.removeSession(key, sess)
			return
		}
		udpForwardCounters.txPackets.Add(1)
		udpForwardCounters.txBytes.Add(uint64(n))
	}
}

func (f *udpForward) sweepLoop(ctx context.Context) {
	t := time.NewTicker(udpForwardSweepEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			now := time.Now().UnixNano()
			f.mu.Lock()
			for key, sess := range f.sessions {
				if now-sess.last.Load() > int64(udpForwardIdleTimeout) {
					delete(f.sessions, key)
					sess.conn.Close()
				}
			}
			f.mu.Unlock()
		}
	}
}

func (f *udpForward) removeSession(key string, sess *udpForwardSession) {
	f.mu.Lock()
	if f.sessions[key] == sess {
		delete(f.sessions, key)
	}
	f.mu.Unlock()
	_ = sess.conn.Close()
}

func (f *udpForward) close() {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	f.closed = true
	sessions := make([]*udpForwardSession, 0, len(f.sessions))
	for _, s := range f.sessions {
		sessions = append(sessions, s)
	}
	f.sessions = make(map[string]*udpForwardSession)
	f.mu.Unlock()
	for _, s := range sessions {
		_ = s.conn.Close()
	}
	_ = f.listen.Close()
}

func (s *udpForwardSession) touch() { s.last.Store(time.Now().UnixNano()) }
