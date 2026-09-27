package client

import (
	"os"
	"os/signal"
	"syscall"
)

// WaitForInterrupt 阻塞到收到 Ctrl+C 或终止信号，供前台模式使用。
func WaitForInterrupt() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	<-ch
}
