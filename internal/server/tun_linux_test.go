//go:build linux

package server

import (
	"os"
	"testing"
	"time"
)

// TryRead 必须真的非阻塞。/dev/net/tun 的 fd 是阻塞模式（OpenTUN 没设 O_NONBLOCK），
// 空队列上 read 会挂住；一旦挂住，pumpTun 的收割循环就永久卡死 —— 连接看起来
// 全在（控制帧走另一条循环），但服务端再也不往下发 IP 包，下行彻底中断。
//
// 用空管道复现同样的 fd 语义：阻塞 fd 上没有数据可读。
func TestTryReadDoesNotBlockOnEmptyTUN(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()

	tun := &TUN{file: r, name: "test-tun"}
	done := make(chan int, 1)
	go func() {
		n, _ := tun.TryRead(make([]byte, 2048))
		done <- n
	}()

	select {
	case n := <-done:
		if n != 0 {
			t.Errorf("无包时应返回 0 字节，实际 %d", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("TryRead 在无包时阻塞了：pumpTun 会卡死在这里，服务端下行永久中断")
	}
}
