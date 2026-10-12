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
func startPortRelay(ctx context.Context, cfg Config, log *slog.Logger, tunnelUp func() bool) (func(), error) {
	var (
		listeners []net.Listener
		active    = map[net.Conn]struct{}{}
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
	track := func(c net.Conn) bool {
		mu.Lock()
		defer mu.Unlock()
		if stopped {
			return false
		}
		active[c] = struct{}{}
		return true
	}
	untrack := func(c net.Conn) {
		mu.Lock()
		delete(active, c)
		mu.Unlock()
	}
	go func() {
		<-ctx.Done()
		stop()
	}()

	clientAddr := cfg.Tunnel.ClientIP
	for port := cfg.ForwardPorts.Start; port <= cfg.ForwardPorts.End; port++ {
		port := port
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err != nil {
			stop()
			return nil, fmt.Errorf("监听转发端口 %d: %w", port, err)
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
				if tunnelUp != nil && !tunnelUp() {
					// 隧道不可用就别拨号：外部立刻拿到连接关闭，不必挂到超时
					_ = conn.Close()
					continue
				}
				target := net.JoinHostPort(clientAddr, fmt.Sprint(port))
				go func() {
					if !track(conn) {
						_ = conn.Close() // 已经在收尾了，别再搬
						return
					}
					defer untrack(conn)
					handleRelayConn(conn, target, log)
				}()
			}
		}()
	}
	return stop, nil
}

// handleRelayConn 双向搬运一条转发连接。任一方向出错即整条关闭。
func handleRelayConn(conn net.Conn, target string, log *slog.Logger) {
	defer conn.Close()
	// 3 秒而不是 10 秒：节点机进程活着、那个端口却没服务时，外部连接要白挂满超时。
	// 目标是隧道内的直连地址（跨境 RTT 两百毫秒量级），3 秒已经非常宽裕。
	upstream, err := net.DialTimeout("tcp", target, 3*time.Second)
	if err != nil {
		log.Debug("转发连接拨号节点机失败", connLogAttrs(err, conn, "target", target)...)
		return
	}
	defer upstream.Close()

	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, conn)
		_ = upstream.Close()
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(conn, upstream)
		_ = conn.Close()
		done <- struct{}{}
	}()
	<-done
	<-done
}
