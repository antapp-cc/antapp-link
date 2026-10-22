package client

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/antapp-cc/antapp-link/internal/pki"
	"github.com/antapp-cc/antapp-link/internal/proto"
)

// UDP 数据通道的保活参数。
//
// 家宽 NAT 的空洞通常 30 秒~2 分钟就关，15 秒留足余量；45 秒收不到任何回包就判可疑。
const (
	udpKeepaliveInterval = 15 * time.Second
	udpDeadAfter         = 45 * time.Second
)

// udpChannel 是客户端侧的 UDP 数据通道。
//
// 控制连接仍走 TLS over TCP（认证、心跳、上报端口），这里只搬数据包：一个 IP 包就是
// 一个 UDP 包，不重传、不排序 —— 丢包交给内层应用自己重传。这是设计上的取舍：
// 隧道传的就是 IP 包，内层 TCP 已经在做可靠性，再叠一层会变成"两层各管各的重传"，
// 那正是之前 13.6 秒延迟的来源。
//
// 密钥从控制连接的 TLS 会话导出，与服务端导出的一致（RFC 5705）。
type udpChannel struct {
	conn     *net.UDPConn
	recv     *proto.Cipher // 解服务端发来的
	sendSeal *proto.Cipher // 加密发给服务端的
	log      *slog.Logger
	stats    *Stats

	// sendMu 必须把 Seal 和 Write 一起锁住：Cipher.Seal 会改 sendSeq 和 nonce，
	// 而数据面（pumpFromDevice）和 keepalive 是两个 goroutine 在并发调用。
	// 只锁 Write 不够 —— 序号重复/nonce 覆盖会让对端 GCM 解密失败，表现为随机丢包。
	sendMu sync.Mutex

	lastRx atomic.Int64 // UnixNano，判活用
	closed chan struct{}
	once   sync.Once
	// failErr 只在被动失效时写入；读取方必须先等到 closed 关闭（见 failCause），
	// 所以靠 channel 的 happens-before 就够，不需要再加一把锁。
	failErr error
}

// startUDPChannel 建 UDP socket、导出密钥、把本地端口报给服务端，然后起收发循环。
func startUDPChannel(ctx context.Context, inv pki.Invite, state tls.ConnectionState,
	dev Device, log *slog.Logger, stats *Stats, report func(port int) error) (*udpChannel, error) {

	c2sKey, err := state.ExportKeyingMaterial(proto.UDPLabelC2S, nil, 32)
	if err != nil {
		return nil, fmt.Errorf("导出 UDP 发送密钥: %w", err)
	}
	s2cKey, err := state.ExportKeyingMaterial(proto.UDPLabelS2C, nil, 32)
	if err != nil {
		return nil, fmt.Errorf("导出 UDP 接收密钥: %w", err)
	}
	sendSeal, err := proto.NewCipher(c2sKey, 1)
	if err != nil {
		return nil, err
	}
	recv, err := proto.NewCipher(s2cKey, 2)
	if err != nil {
		return nil, err
	}

	// 服务端的 UDP 口与控制连接同端口（不同协议，不冲突）
	raddr, err := net.ResolveUDPAddr("udp", inv.Server)
	if err != nil {
		return nil, fmt.Errorf("解析服务端 UDP 地址 %s: %w", inv.Server, err)
	}
	conn, err := net.DialUDP("udp", nil, raddr)
	if err != nil {
		return nil, fmt.Errorf("建 UDP 连接: %w", err)
	}

	// 端口必须通过控制通道上报：服务端按 (TCP 远端 IP, 这个端口) 认人，
	// 不接受 UDP 包里自称的地址，否则谁发个包都能往隧道里灌数据。
	local, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		_ = conn.Close()
		return nil, errors.New("拿不到 UDP 本地端口")
	}
	if err := report(local.Port); err != nil {
		_ = conn.Close()
		return nil, err
	}

	c := &udpChannel{conn: conn, recv: recv, sendSeal: sendSeal, log: log, stats: stats,
		closed: make(chan struct{})}
	c.lastRx.Store(time.Now().UnixNano())
	go c.recvLoop(ctx, dev)
	go c.keepalive(ctx)

	// 验证 UDP 双向真的通。
	//
	// 只上报端口是控制面的事：端口从 TCP 报上去了，UDP 包却可能被防火墙 / 运营商
	// 悄悄丢掉（这个坑真踩过 —— NAT 改了源端口那次就是「端口报上去了、包全丢」）。
	// 不验证的话，界面会显示「UDP 数据通道已启用」而业务全不通，用户只会看到
	// 「连上了但打不开网页」，比直接退回 TCP 难排查得多。
	//
	// 发一个保活包，等服务端回一个空的加密包（readLoop 收到空负载会回 ACK）。
	// 收不到就判定这条通道不可用，并且不会退回 TCP —— 调用方拿到错误后直接让这次连接失败。
	// UDP 模式下偷偷改用 TCP 会把「端口没放行」伪装成一切正常，让故障无法定位。
	if perr := c.probe(ctx, 5*time.Second); perr != nil {
		c.close()
		return nil, fmt.Errorf("UDP 通道验证失败: %w", perr)
	}
	return c, nil
}

// udpProbeRetryAt 是首次探测后重发认证空包的时刻（相对首次发送）。
//
// 只发一次的实测问题：家宽 NAT 映射刚建立、或服务端还没处理完 UDPPort 登记时，
// 第一个包就丢了，客户端却立刻判整条 UDP 通道不可用。keepalive 是 15 秒一轮，
// 覆盖不到这 5 秒的探测窗口 —— 一次普通丢包就够让这次连接失败。
//
// 250ms/750ms/1500ms 三档：前两档覆盖「包到了、服务端还没认人」这个窗口，最后一档
// 再扛一次路径丢包。总预算仍是 5 秒，端口真没放行时不会拖得更久。
//
// var 而不是 const：测试把节奏压到毫秒级。
var udpProbeRetryAt = []time.Duration{
	250 * time.Millisecond,
	750 * time.Millisecond,
	1500 * time.Millisecond,
}

// probe 确认 UDP 双向都通：在总预算内发几次认证空包，等服务端的加密回包。
//
// 只有通过 GCM 认证的回包才算数 —— lastRx 只在 recvLoop 解包成功后刷新，
// 任何明文/伪造的 UDP 输入都刷不亮它。否则「端口放行了但密钥不对」会被当成健康，
// 业务包却全被对端丢掉。
func (c *udpChannel) probe(ctx context.Context, timeout time.Duration) error {
	baseline := c.lastRx.Load()
	start := time.Now()
	deadline := start.Add(timeout)

	if err := c.send(nil); err != nil {
		return err
	}
	next := 0
	for {
		if c.lastRx.Load() != baseline {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%s 内没收到服务端回包（UDP 端口可能没放行）", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.closed:
			return errors.New("UDP 通道已关闭")
		case <-time.After(10 * time.Millisecond):
		}
		if next < len(udpProbeRetryAt) && time.Since(start) >= udpProbeRetryAt[next] {
			next++
			if err := c.send(nil); err != nil {
				return err
			}
		}
	}
}

// send 把一个 IP 包加密后发出去。并发安全：见 sendMu 的注释。
func (c *udpChannel) send(pkt []byte) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	_, err := c.conn.Write(c.sendSeal.Seal(pkt))
	return err
}

// recvLoop 收服务端发来的包，解密后写网卡。
func (c *udpChannel) recvLoop(ctx context.Context, dev Device) {
	buf := make([]byte, proto.MaxUDPPacket)
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.closed:
			return
		default:
		}
		// 必须带读超时：UDP 没数据时 Read 会永久阻塞，ctx 取消也放不出来
		_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, err := c.conn.Read(buf)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			case <-c.closed:
				return
			default:
			}
			if errors.Is(err, os.ErrDeadlineExceeded) {
				continue
			}
			c.log.Debug("UDP 读失败", "err", err)
			continue
		}
		plain, err := c.recv.Open(buf[:n])
		if err != nil {
			// 重放、乱序、保活空包都是常态，不当错误刷日志
			continue
		}
		c.lastRx.Store(time.Now().UnixNano())
		if c.stats != nil {
			c.stats.RxBytes.Add(uint64(len(plain)))
		}
		if len(plain) > 0 && dev != nil {
			if _, err := dev.Write(plain); err != nil {
				c.log.Debug("UDP 包写网卡失败", "err", err)
			}
		}
	}
}

// keepalive 定期发空包：既保活 NAT，也让服务端有东西可回（回包同样刷新 NAT 状态）。
func (c *udpChannel) keepalive(ctx context.Context) {
	t := time.NewTicker(udpKeepaliveInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.closed:
			return
		case <-t.C:
			if idle := time.Since(time.Unix(0, c.lastRx.Load())); idle > udpDeadAfter {
				// 连续多个保活周期收不到回包：这条 UDP 路径已经没用了（NAT 映射被清、
				// 路由变了、或者运营商开始封）。关掉它并让上层结束会话，重连时会重新
				// 建一条 UDP 通道 —— 仍然是 UDP，不切 TCP。
				//
				// 原来这里只打一条 warn 就继续跑：控制连接还好好的，面板显示在线，
				// 业务包却一直往一个死掉的 UDP 路径里灌。
				c.log.Warn("UDP 数据通道连续多个周期收不到回包，关闭并重建",
					"idle", idle.Round(time.Second))
				c.fail(fmt.Errorf("UDP 数据通道连续 %s 收不到回包",
					idle.Round(time.Second)))
				return
			}
			_ = c.send(nil)
		}
	}
}

// isClosed 报告通道是否已被关闭（keepalive 判定失效，或会话收尾）。
func (c *udpChannel) isClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

func (c *udpChannel) close() { c.fail(nil) }

// fail 关闭通道并记下被动失效的原因。原因只为区分「故障」和「会话主动收尾」：
// 后者调用 close()，会话监听方看到后什么都不做，绝不能借机重连。
func (c *udpChannel) fail(cause error) {
	c.once.Do(func() {
		c.failErr = cause
		close(c.closed)
		_ = c.conn.Close()
	})
}

// failCause 返回被动失效的原因；主动收尾或尚未关闭返回 nil。
// 必须在 closed 已关闭之后调用（写入发生在 close(closed) 之前）。
func (c *udpChannel) failCause() error {
	select {
	case <-c.closed:
		return c.failErr
	default:
		return nil
	}
}

// watchUDP 盯着当前 UDP 通道的失效信号，让会话不等业务包就结束并重建。
//
// 以前只有用户恰好发包、pumpFromDevice 的 send 失败才会结束会话：空闲的隧道
// 控制连接心跳正常、面板显示「已连接」，数据面早就断了，直到用户下次上网才暴露。
// keepalive 已经判定通道没救并关掉它，这里直接接住那个信号。
//
// 只认有原因的关闭：会话收尾时 session 的 defer 也会 close()，那不该触发重连。
func (s *clientSession) watchUDP(ctx context.Context, errCh chan<- error) {
	u := s.udp
	if u == nil {
		return
	}
	select {
	case <-ctx.Done():
		return
	case <-u.closed:
	}
	// 收尾顺序是「run 先 cancel 再关通道」，但两个信号可能同时就绪；
	// 再查一次 ctx，免得把主动收尾当成故障报上去。
	select {
	case <-ctx.Done():
		return
	default:
	}
	if cause := u.failCause(); cause != nil {
		// run 可能已经因别的错误返回，errCh 没人收；非阻塞投递避免协程卡住
		select {
		case errCh <- cause:
		default:
		}
	}
}
