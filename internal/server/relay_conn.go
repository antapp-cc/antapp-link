package server

import (
	"net"
	"sync/atomic"
	"time"
)

// 端口转发的几个阈值。用 var 而不是 const：测试要把空闲超时压到毫秒级，
// 否则验证"静默连接会被清掉"这一个用例就得跑十分钟。
var (
	// relayIdleTimeout 是转发连接允许的最长静默时间。
	//
	// 公网扫描器连上端口后通常既不发送也不关闭，而搬运协程会一直阻塞在 Read 上 ——
	// 一条这样的连接白占 2 个协程和 2 个 fd。实测 31402 上同时挂着 25~36 条，
	// 服务端 goroutine 从 28 涨到 138。真正的 Pi 节点连接不会静默这么久
	// （共识流和块同步一直在走），所以这个阈值只杀噪声。
	relayIdleTimeout = 10 * time.Minute

	// relaySweepEvery 是清扫空闲连接的间隔。
	// 比 relayIdleTimeout 小一个数量级：被静默的连接最多多活一个清扫周期。
	relaySweepEvery = time.Minute

	// relayMaxActive 是同时在途的转发连接上限。
	//
	// 正常 Pi 节点只占几条；到这个数量一定是外部扫描或攻击。与其让它们把
	// fd 和 goroutine 吃光，不如直接拒绝新的 —— 真节点会自己重试。
	relayMaxActive = 256
)

// relayConn 包一层 net.Conn，记录最后一次有字节流动的时间。
//
// 只包外层（accept 来的那条）就够：它的 Read 覆盖「外部→节点机」方向，
// Write 覆盖「节点机→外部」方向，两个方向任一有流量都会刷新时间戳。
type relayConn struct {
	net.Conn
	lastActive atomic.Int64 // unix nano
}

func newRelayConn(c net.Conn) *relayConn {
	rc := &relayConn{Conn: c}
	rc.touch()
	return rc
}

func (c *relayConn) touch() { c.lastActive.Store(time.Now().UnixNano()) }

// idleFor 返回这条连接已经静默多久。
func (c *relayConn) idleFor(now time.Time) time.Duration {
	return now.Sub(time.Unix(0, c.lastActive.Load()))
}

func (c *relayConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.touch()
	}
	return n, err
}

func (c *relayConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 {
		c.touch()
	}
	return n, err
}

// CloseWrite 转发半关闭。
//
// 必须显式转发：relayConn 嵌的是 net.Conn 接口，而 CloseWrite 不在该接口的方法集里，
// 嵌进来的接口不会自动带上它。不转发的话 handleRelayConn 里的 closeWrite() 类型断言
// 失败、退回整条 Close —— 服务端还没发完的应答被截断，客户端拿到空响应。这正是
// 那段代码注释里说要避免的情况，套一层 conn 就把这个保证丢了。
func (c *relayConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return c.Close()
}

// relayStats 是转发器的运行计数，写进状态文件供 status 展示。
//
// 存在的理由：这次的「goroutine 从 28 涨到 138」是靠手工数 ss 才发现的。
// 如果 status 里一直显示在途连接数，异常增长当场就能看出来。
type relayStats struct {
	active   atomic.Int64
	total    atomic.Int64
	rejected atomic.Int64
	swept    atomic.Int64
}

func (s *relayStats) onAccept()   { s.total.Add(1) }
func (s *relayStats) onActive()   { s.active.Add(1) }
func (s *relayStats) onClosed()   { s.active.Add(-1) }
func (s *relayStats) onRejected() { s.rejected.Add(1) }
func (s *relayStats) setSwept(n int64) {
	s.swept.Store(n)
}

// snapshot 一次读出四个数，顺序：在途、累计、被拒、静默超时。
func (s *relayStats) snapshot() (active, total, rejected, swept int) {
	return int(s.active.Load()), int(s.total.Load()),
		int(s.rejected.Load()), int(s.swept.Load())
}
