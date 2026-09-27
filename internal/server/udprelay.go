package server

import (
	"crypto/tls"
	"log/slog"
	"net"
	"sync"
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
	peer     *net.UDPAddr
	recv     *proto.Cipher // 解这个客户端发来的
	sendSeal *proto.Cipher // 加密发给这个客户端
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

// addSession 从一个已完成的 TLS 会话导出密钥，登记这个客户端的 UDP 端点。
//
// ip 由调用方从 TCP 连接的真实远端取，**不接受**客户端声称的 IP ——
// 否则任何人都能冒充这个客户端把隧道劫走。port 由控制通道上报。
func (m *udpMux) addSession(tlsState tls.ConnectionState, ip net.IP, port int) (*udpSession, error) {
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
		peer:     &net.UDPAddr{IP: ip, Port: port},
		recv:     recv,
		sendSeal: send,
	}
	m.mu.Lock()
	m.sessions[sess.peer.String()] = sess
	m.mu.Unlock()
	return sess, nil
}

func (m *udpMux) removeSession(sess *udpSession) {
	if sess == nil {
		return
	}
	m.mu.Lock()
	delete(m.sessions, sess.peer.String())
	m.mu.Unlock()
}

func (m *udpMux) lookup(from *net.UDPAddr) *udpSession {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessions[from.String()]
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
			// 重放/乱序是 UDP 的常态，不当错误刷日志
			continue
		}
		if _, err := m.tun.Write(plain); err != nil {
			m.log.Debug("UDP 包写网卡失败", "err", err)
		}
	}
}

// send 把一个 IP 包加密后发给该会话的客户端。
func (s *udpSession) send(m *udpMux, pkt []byte) error {
	_, err := m.conn.WriteToUDP(s.sendSeal.Seal(pkt), s.peer)
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
