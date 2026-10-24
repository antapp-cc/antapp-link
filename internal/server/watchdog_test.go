package server

import (
	"testing"
	"time"
)

func TestWatchdogStuck(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		beat time.Time
		want bool
	}{
		{"零值不触发（启动初期还没上报过）", time.Time{}, false},
		{"心跳新鲜", now.Add(-5 * time.Second), false},
		{"刚好超过阈值", now.Add(-statusWatchdogTimeout - time.Second), true},
		{"心跳停了很久", now.Add(-10 * time.Minute), true},
	}
	for _, c := range cases {
		if got := watchdogStuck(c.beat, now, statusWatchdogTimeout); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}
