package server

import (
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/antapp-cc/antapp-link/internal/proto"
)

// udpMux 是服务端唯一的 UDP 数据口。
//
// 为什么是一个口而不是每会话一个：UDP socket 只能绑一次同一个地址。
// 而密钥必须每会话一份（各自从自己的 TLS 会话导出），所以这里按来源地址
// 把包分派到对应的会话上。
//
// 分派依据是「控制通道上报过的端点」—— 不认识的来源直接丢，
// 否则谁发个包都能往隧道里灌数据。
type udpMux struct {
	conn *net.UDPConn
	tun  *TUN
	log  *slog.Logger

	mu       sync.RWMutex
	sessions map[string]*udpSession
	closed   bool
}

// udpSession 是一个客户端在 UDP 侧的全部状态。
type udpSession struct {
	// peer 是客户端真实的 UDP 端点。IP 由 TCP 连接确定，端口靠收到包时学习 ——
	// 客户端上报的端口会被家宽 NAT 改写，在服务端看到的往往不是同一个值。
	peer     atomic.Pointer[net.UDPAddr]
	recv     *proto.Cipher // 解这个客户端发来的
	sendSeal *proto.Cipher // 加密发给这个客户端

	// owner 是登记这条通道的会话。同一个 IP 快速重连时，旧会话读协程的迟到登记
	// 不能覆盖新会话的条目 —— 否则 map 被顶掉后新会话的上行全部找不到人。
	owner *session

	// sendMu 保护 sendSeal：Cipher.Seal 会改 sendSeq 和共享的 nonce。
	// 下行数据（pumpTun）和保活回包（readLoop）是两个 goroutine 在并发调 send，
	// 不串行化就会序号重复、nonce 复用 —— 后者是密码学错误，不只是丢包。
	sendMu sync.Mutex

	// rxBytes/txBytes 是这条 UDP 通道实际收发的链路字节（含认证开销）。UDP 模式
	// 下行不走槽位，没有 SlotStatus 可看，这两个数是判断「数据到底通没通」的依据。
	rxBytes atomic.Uint64
	txBytes atomic.Uint64
}

// sessionAlive 报告会话是否还活着（closed 只在收尾时关闭一次）。
func sessionAlive(s *session) bool {
	if s == nil {
		return false
	}
	select {
	case <-s.closed:
		return false
	default:
		return true
	}
}

// allowUDPReplace 报告同 IP 已有的条目能否被新会话顶掉。
//
// 只有「旧条目没有主人」或「旧主人已经收尾」才允许覆盖。同 IP 快速重连时新会话
// 先登记、旧会话读协程的迟到登记后到：不拦的话旧条目会顶掉新条目，旧会话 detach
// 再按守卫删掉它，map 就空了 —— 新会话下行照发、上行全被当来路不明丢弃。
func allowUDPReplace(cur *udpSession, next *session) bool {
	if cur == nil {
		return true
	}
	if cur.owner == nil {
		return true
	}
	if cur.owner == next {
		return true
	}
	return !sessionAlive(cur.owner)
}

func newUDPMux(listen string, tun *TUN, log *slog.Logger) (*udpMux, error) {
	addr, err := net.ResolveUDPAddr("udp", listen)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return nil, err
	}
	m := &udpMux{
		conn: conn, tun: tun, log: log,
		sessions: make(map[string]*udpSession),
	}
	go m.readLoop()
	return m, nil
}

// addSession 从一个已完成的 TLS 会话导出密钥，登记这个客户端的 UDP 身份。
//
// ip 由调用方从 TCP 连接的真实远端取，**不接受**客户端声称的 IP ——
// 否则任何人都能冒充这个客户端把隧道劫走。
//
// 端口参数只用于日志：家宽 NAT 会把客户端自报的源端口改掉，服务端真正能用的端口
// 必须从收到的第一个包里学习（见 lookup）。按 IP 收敛没有歧义 —— 服务端同一时刻
// 只服务一个客户端。
func (m *udpMux) addSession(owner *session, tlsState tls.ConnectionState, ip net.IP, port int) (*udpSession, error) {
	// 登记前先自查：自己这个会话是不是已经在收尾了。
	//
	// 交错 A（泄漏）里 detach 早就跑过（那时 sess.udp 还是 nil，不会回收），迟到的
	// 登记只会在 map 里留一条没人删的条目；交错 B 里新旧会话交替时旧会话已被
	// old.close() 踢掉。两种情况下 owner 的 closed 都已关闭 —— 直接拒绝最省事，
	// 也不依赖 map 里此刻有没有条目。
	if !sessionAlive(owner) {
		return nil, errors.New("会话已收尾，放弃登记 UDP 数据通道")
	}
	c2sKey, err := tlsState.ExportKeyingMaterial(proto.UDPLabelC2S, nil, 32)
	if err != nil {
		return nil, err
	}
	s2cKey, err := tlsState.ExportKeyingMaterial(proto.UDPLabelS2C, nil, 32)
	if err != nil {
		return nil, err
	}
	recv, err := proto.NewCipher(c2sKey, 1)
	if err != nil {
		return nil, err
	}
	send, err := proto.NewCipher(s2cKey, 2)
	if err != nil {
		return nil, err
	}

	sess := &udpSession{
		recv:     recv,
		sendSeal: send,
		owner:    owner,
	}
	sess.peer.Store(&net.UDPAddr{IP: ip, Port: port})
	m.mu.Lock()
	if !allowUDPReplace(m.sessions[ip.String()], owner) {
		m.mu.Unlock()
		return nil, errors.New("该 IP 已有更新的 UDP 会话，拒绝覆盖")
	}
	m.sessions[ip.String()] = sess
	m.mu.Unlock()
	return sess, nil
}

// removeSession 从索引里摘掉一个会话。
//
// 必须确认 map 里存的仍然是同一个对象：重连时旧会话可能晚于新会话才 detach，
// 无条件 delete 会把新会话的映射一起删掉，新会话的下行就永久不通了。
func (m *udpMux) removeSession(sess *udpSession) {
	if sess == nil {
		return
	}
	p := sess.peer.Load()
	if p == nil {
		return
	}
	key := p.IP.String()
	m.mu.Lock()
	if cur, ok := m.sessions[key]; ok && cur == sess {
		delete(m.sessions, key)
	}
	m.mu.Unlock()
}

// dropOtherSessions 摘掉同 IP 下不属于 keep 的条目。
//
// 调用方（registerUDP）已经确认 keep 是当前会话：同 IP 的其他条目都属于已被取代的
// 旧会话。不先摘掉的话，addSession 的 allowUDPReplace 会看到「旧 owner 还活着」
// 而拒绝新会话的登记，随后旧会话 detach 删掉旧条目，map 就空了 —— 新会话上行全丢。
func (m *udpMux) dropOtherSessions(ip net.IP, keep *session) {
	key := ip.String()
	m.mu.Lock()
	if cur, ok := m.sessions[key]; ok && cur.owner != keep {
		delete(m.sessions, key)
	}
	m.mu.Unlock()
}

// lookup 只按来源 IP 找候选会话，**不改** peer —— 必须等包通过 GCM 认证后才允许
// 更新端口（见 confirmPeer），否则伪造包就能把下行数据导向错误的地方。
func (m *udpMux) lookup(from *net.UDPAddr) *udpSession {
	m.mu.RLock()
	sess := m.sessions[from.IP.String()]
	m.mu.RUnlock()
	return sess
}

// confirmPeer 在包通过认证后刷新真实源端口：NAT 的空洞可能因设备重连而换端口。
func (m *udpMux) confirmPeer(sess *udpSession, from *net.UDPAddr) {
	sess.peer.Store(&net.UDPAddr{IP: from.IP, Port: from.Port})
}

func (m *udpMux) readLoop() {
	buf := make([]byte, proto.MaxUDPPacket)
	for {
		n, from, err := m.conn.ReadFromUDP(buf)
		if err != nil {
			m.mu.RLock()
			closed := m.closed
			m.mu.RUnlock()
			if !closed {
				m.log.Debug("UDP 读结束", "err", err)
			}
			return
		}
		if n <= 0 {
			continue
		}

		sess := m.lookup(from)
		if sess == nil {
			m.log.Debug("丢弃来路不明的 UDP 包", "from", from.String())
			continue
		}

		plain, err := sess.recv.Open(buf[:n])
		if err != nil {
			// 重放/乱序/伪造都是 UDP 的常态，不当错误刷日志。
			// 注意这里**不更新** peer —— 未认证的包不能影响下行目标。
			continue
		}
		// 认证通过了，才承认这个来源端口
		m.confirmPeer(sess, from)
		sess.rxBytes.Add(uint64(len(plain)))
		if len(plain) == 0 {
			// 空负载 = 客户端的保活包。回一个空的加密包，让它的 lastRx 能刷新 ——
			// 客户端只看"我发出去了"判断不了下行还通不通。
			if err := sess.send(m, nil); err != nil {
				m.log.Debug("UDP 保活回包失败", "err", err)
			}
			continue
		}
		if _, err := m.tun.Write(plain); err != nil {
			m.log.Debug("UDP 包写网卡失败", "err", err)
		}
	}
}

// send 把一个 IP 包加密后发给该会话的客户端。并发安全：见 sendMu 的注释。
func (s *udpSession) send(m *udpMux, pkt []byte) error {
	peer := s.peer.Load()
	if peer == nil {
		return nil
	}
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	n, err := m.conn.WriteToUDP(s.sendSeal.Seal(pkt), peer)
	if err == nil {
		s.txBytes.Add(uint64(n))
	}
	return err
}

func (m *udpMux) close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	m.mu.Unlock()

	_ = m.conn.SetReadDeadline(time.Now())
	_ = m.conn.Close()
}
