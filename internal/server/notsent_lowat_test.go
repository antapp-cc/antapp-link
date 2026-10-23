package server

import "testing"

// 默认 128 KB；0 表示不设置；非法值回落默认 —— 拼错数字不该让拥塞重新变成排长队。
func TestServerNotsentLowatEnv(t *testing.T) {
	cases := map[string]int{
		"":        128 << 10,
		"0":       0,
		"16384":   16 << 10,
		" 65536 ": 64 << 10,
		"garbage": 128 << 10,
		"-1":      128 << 10,
	}
	for v, want := range cases {
		t.Setenv("ANTAPP_SERVER_NOTSENT_LOWAT", v)
		if got := envServerNotsentLowat(); got != want {
			t.Errorf("%q: 期望 %d，实际 %d", v, want, got)
		}
	}
}
