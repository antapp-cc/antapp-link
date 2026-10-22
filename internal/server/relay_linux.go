//go:build linux

package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"
)

// startPortRelay 在云服本机监听转发端口段的每个端口，收到连接立即应答，
// 再拨号经隧道转给节点机（10.10.0.2:同端口）。
//
// 在本机监听并立即应答：外部连接的握手在云服完成，延迟只算到云服为止；
// 之后连接经隧道转给节点机。代价：节点机看到的连接来源是服务端隧道地址
// （10.10.0.1）。节点机离线 / 对应端口无服务时，拨号失败、连接立即关闭。
//
// tunnelUp 报告隧道链路是否还在。节点机不在线时不拨号：拨号的目标地址在 tun 上，
// 没人读网卡就是空转 SYN 重传，外部连接还要挂在云服等满拨号超时（实测节点机离线
// 期间云服堆着 84 条 SYN-SENT、每秒几十个白跑的包）。
//
// 返回的 *relayStats 让 reportStatus 把在途连接数写进状态文件：这批端口对公网开放，
// 互联网扫描器会一直连上来且不发送数据，只看 goroutine 数才知道出了什么事。
func startPortRelay(ctx context.Context, cfg Config, log *slog.Logger, tunnelUp func() bool) (func(), *relayStats, error) {
	var (
		listeners []net.Listener
		active    = map[*relayConn]struct{}{}
		st        = &relayStats{}
		mu        sync.Mutex
		stopped   bool
	)
	stop := func() {
		mu.Lock()
		defer mu.Unlock()
		if stopped {
			return
		}
		stopped = true
		for _, ln := range listeners {
			_ = ln.Close()
		}
		// 在途连接的 io.Copy 不感知 ctx，只能把连接关掉让它们返回：否则 Run 退出后
		// 这些搬运协程还挂着（把 Run 当库复用时就是每次泄漏一批）。
		for c := range active {
			_ = c.Close()
		}
	}
	track := func(c *relayConn) bool {
		mu.Lock()
		defer mu.Unlock()
		if stopped {
			return false
		}
		active[c] = struct{}{}
		return true
	}
	untrack := func(c *relayConn) {
		mu.Lock()
		delete(active, c)
		mu.Unlock()
		st.onClosed()
	}
	go func() {
		<-ctx.Done()
		stop()
	}()

	// 清扫静默连接。没有它，一条「连上但不发数据」的连接会永久占着两个搬运协程：
	// handleRelayConn 的 <-done; <-done 没有任何超时，而扫描器正是连上就不动。
	go func() {
		t := time.NewTicker(relaySweepEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				now := time.Now()
				// 先在锁内挑出目标，再出锁关闭：Close 会唤醒阻塞中的 io.Copy，
				// 那条协程的 defer untrack 回来抢同一把锁，持锁关闭就自己和自己死锁。
				mu.Lock()
				var victims []*relayConn
				for rc := range active {
					if rc.idleFor(now) > relayIdleTimeout {
						victims = append(victims, rc)
					}
				}
				stillIdle := int64(len(victims))
				mu.Unlock()

				for _, rc := range victims {
					_ = rc.Close()
				}
				st.setIdle(stillIdle)
				if stillIdle > 0 {
					log.Info("清理静默的转发连接",
						"count", stillIdle, "idle_timeout", relayIdleTimeout)
				}
			}
		}
	}()

	clientAddr := cfg.Tunnel.ClientIP
	for port := cfg.ForwardPorts.Start; port <= cfg.ForwardPorts.End; port++ {
		port := port
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err != nil {
			stop()
			return nil, nil, fmt.Errorf("监听转发端口 %d: %w", port, err)
		}
		mu.Lock()
		listeners = append(listeners, ln)
		mu.Unlock()
		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return // 监听已关闭
				}
				st.onAccept()
				if tunnelUp != nil && !tunnelUp() {
					// 隧道不可用就别拨号：外部立刻拿到连接关闭，不必挂到超时
					_ = conn.Close()
					continue
				}
				mu.Lock()
				full := len(active) >= relayMaxActive
				mu.Unlock()
				if full {
					// 这些端口对公网开放，扫描器能把连接数推到很高。拒绝比让它
					// 把 fd 和协程吃光好 —— 真节点会自己重试，晚几秒不影响共识。
					st.onRejected()
					_ = conn.Close()
					continue
				}
				rc := newRelayConn(conn)
				target := net.JoinHostPort(clientAddr, fmt.Sprint(port))
				go func() {
					if !track(rc) {
						_ = rc.Close() // 已经在收尾了，别再搬
						return
					}
					st.onActive()
					defer untrack(rc)
					handleRelayConn(rc, target, log)
				}()
			}
		}()
	}
	return stop, st, nil
}

// handleRelayConn 双向搬运一条转发连接。任一方向出错即整条关闭。
func handleRelayConn(conn net.Conn, target string, log *slog.Logger) {
	defer conn.Close()
	// 6 秒：目标是隧道内的跨境直连地址，Linux 的 SYN 重传是 1s/3s/7s，而这条链路
	// 实测有 30% 级别丢包 —— 3 秒只容得下第二个 SYN，约 9% 的转发连接会被误判失败。
	// 「节点机端口没服务」的快速路径靠对端回 RST（秒回），不依赖缩短这个超时。
	upstream, err := net.DialTimeout("tcp", target, 6*time.Second)
	if err != nil {
		log.Debug("转发连接拨号节点机失败", connLogAttrs(err, conn, "target", target)...)
		return
	}
	defer upstream.Close()

	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, conn)
		// 客户端只是关掉了写方向（请求发完了），要等应答。这里把半关闭传给
		// upstream，而不是整条关掉 —— 整条关会把服务端还没发完的应答截断，
		// 客户端拿到空应答（复审第三轮问题六用本地 TCP 对照复现过）。
		closeWrite(upstream)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(conn, upstream)
		closeWrite(conn)
		done <- struct{}{}
	}()
	<-done
	<-done
}

// closeWrite 只关写方向，让另一方向还能继续收。
// 不是 *net.TCPConn 时退回整条关闭（至少不会挂住）。
func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}
