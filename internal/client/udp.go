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

// probe 确认 UDP 双向都通：发一个保活包，等服务端的回包刷新 lastRx。
func (c *udpChannel) probe(ctx context.Context, timeout time.Duration) error {
	start := c.lastRx.Load()
	if err := c.send(nil); err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c.lastRx.Load() != start {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.closed:
			return errors.New("UDP 通道已关闭")
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("%s 内没收到服务端回包（UDP 端口可能没放行）", timeout)
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
				c.close()
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

func (c *udpChannel) close() {
	c.once.Do(func() {
		close(c.closed)
		_ = c.conn.Close()
	})
}
