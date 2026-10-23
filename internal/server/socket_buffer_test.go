package server

import "testing"

// 默认必须保持 4 MiB：A/B 还没做，不能顺手改生产默认值。
func TestServerSocketBufferDefault(t *testing.T) {
	t.Setenv("ANTAPP_SERVER_SOCKET_BUFFER", "")
	if got := envServerSocketBufferBytes(); got != 4<<20 {
		t.Fatalf("默认应是 4 MiB，实际 %d", got)
	}
}

// 0 表示不显式设置（交给系统自动调节），非法值回落到默认 —— 拼错一个数字
// 不该让服务端悄悄用上 0 字节缓冲。
func TestServerSocketBufferEnv(t *testing.T) {
	cases := map[string]int{
		"0":         0,
		"1048576":   1 << 20,
		" 2097152 ": 2 << 20,
		"garbage":   4 << 20,
		"-1":        4 << 20,
	}
	for v, want := range cases {
		t.Setenv("ANTAPP_SERVER_SOCKET_BUFFER", v)
		if got := envServerSocketBufferBytes(); got != want {
			t.Errorf("%q: 期望 %d，实际 %d", v, want, got)
		}
	}
}
