package client

import "testing"

func TestNlaRunning(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want bool
	}{
		{"运行中（英文系统）", "SERVICE_NAME: nlasvc\n        STATE              : 4  RUNNING\n", true},
		{"运行中（中文系统）", "SERVICE_NAME: nlasvc\n        状态              : 4  RUNNING\n", true},
		{"已停止", "SERVICE_NAME: nlasvc\n        STATE              : 1  STOPPED\n", false},
		{"空输出", "", false},
		{"只有错误信息", "错误: 服务未安装", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := nlaRunning(c.out); got != c.want {
				t.Fatalf("nlaRunning(%q) = %v，want %v", c.out, got, c.want)
			}
		})
	}
}
