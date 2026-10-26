package client

import (
	"strings"
	"testing"
)

// 界面日志区是单行模式，超宽的内容控件仍然会自己折行（续行顶格压在日期列上）。
// fitLogLine 保证写进缓冲区的那一行一定放得下 —— 否则又回到「内容占用日期时间」。
func TestFitLogLineCapsWidth(t *testing.T) {
	displayUnits := func(s string) int {
		n := 0
		for _, r := range s {
			if r > 127 {
				n += 2
			} else {
				n++
			}
		}
		return n
	}

	cases := []struct {
		name string
		in   string
	}{
		{"短行不动", "10-20 12:01:51 . 已连接"},
		{"中文超长", "10-20 12:01:40 ! 会话结束  原因=" + strings.Repeat("很长的中文原因", 20)},
		{"英文超长", "10-20 12:01:40 ! " + strings.Repeat("abcdefghij", 30)},
		{"中英混排超长", "10-20 12:01:40 ! " + strings.Repeat("连接abc", 30)},
		{"刚好边界", "10-20 12:01:40 ! 成员连接断开  成员=1  原因=正常关闭  累计丢弃=1"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := fitLogLine(c.in)
			if strings.ContainsAny(got, "\r\n") {
				t.Fatalf("结果里还有换行符: %q", got)
			}
			if displayUnits(got) > logLineMaxUnits {
				t.Fatalf("截断后仍然超宽：%d > %d\n%q", displayUnits(got), logLineMaxUnits, got)
			}
			// 没被截断的必须原样保留
			if displayUnits(c.in) <= logLineMaxUnits && got != c.in {
				t.Fatalf("短行不该被改动：\n原: %q\n新: %q", c.in, got)
			}
			// 被截断的应当以 … 收尾
			if displayUnits(c.in) > logLineMaxUnits && !strings.HasSuffix(got, "…") {
				t.Fatalf("截断应以 … 结尾: %q", got)
			}
		})
	}
}
