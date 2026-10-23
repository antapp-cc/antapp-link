package server

import "testing"

// 默认 0：交给内核自动调节（安装脚本已把 tcp_rmem/tcp_wmem 上限设到 16 MB），
// 和客户端一致。显式写死 4 MB 会关掉自动调节、把窗口天花板压低。
func TestServerSocketBufferDefault(t *testing.T) {
	t.Setenv("ANTAPP_SERVER_SOCKET_BUFFER", "")
	if got := envServerSocketBufferBytes(); got != 0 {
		t.Fatalf("默认应是 0（系统自动调节），实际 %d", got)
	}
}

// 显式值照用（A/B 或回退到 4 MB）；非法值回落到默认 0，也就是自动调节 ——
// 拼错一个数字不该让服务端用上某个奇怪的固定缓冲。
func TestServerSocketBufferEnv(t *testing.T) {
	cases := map[string]int{
		"0":         0,
		"1048576":   1 << 20,
		" 2097152 ": 2 << 20,
		"4194304":   4 << 20,
		"garbage":   0,
		"-1":        0,
	}
	for v, want := range cases {
		t.Setenv("ANTAPP_SERVER_SOCKET_BUFFER", v)
		if got := envServerSocketBufferBytes(); got != want {
			t.Errorf("%q: 期望 %d，实际 %d", v, want, got)
		}
	}
}
