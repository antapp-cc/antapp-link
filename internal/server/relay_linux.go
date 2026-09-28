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
// 为什么用本机监听转发而不用内核 DNAT：DNAT 的 TCP 握手必须走到节点机
// 才完成，外部检查器的延迟会把整条隧道往返（实测 ~194ms）算进去，显示
// 翻倍（~400ms）；本机先应答后，检查器只量到自己到云服为止，与 rinetd
// 时代同口径。代价：节点机看到的连接来源是服务端隧道地址（10.10.0.1）。
//
// 节点机离线 / 对应端口无服务时，拨号失败、连接立即关闭 —— 检查器的
// TCP 连接已经建立（显示 OPEN），不会误报故障。
func startPortRelay(ctx context.Context, cfg Config, log *slog.Logger) (func(), error) {
	var (
		listeners []net.Listener
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
				target := net.JoinHostPort(clientAddr, fmt.Sprint(port))
				go handleRelayConn(conn, target, log)
			}
		}()
	}
	return stop, nil
}

// handleRelayConn 双向搬运一条转发连接。任一方向出错即整条关闭。
func handleRelayConn(conn net.Conn, target string, log *slog.Logger) {
	defer conn.Close()
	upstream, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		log.Debug("转发连接拨号节点机失败", "target", target, "err", err)
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
