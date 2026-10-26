package client

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// 界面日志区现在是「强行单行」模式（控件不让折行，见 ui_windows.go）。
// 所以任何一条记录都必须落成一行 —— 消息里带换行、属性值里带换行都不行，
// 否则界面里会出现第二条没有时间戳前缀的行，正好压在日期时间那一列上。
func TestHandleEmitsSingleLine(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		attr slog.Attr
	}{
		{"消息带换行", "第一行\n第二行", slog.String("err", "正常")},
		{"属性带换行", "正常", slog.String("err", "底层错误\n还有一行")},
		{"两者都带", "头\n尾", slog.String("原因", "中间\r\n换行")},
		{"多段换行", "a\nb\nc\n", slog.String("x", "y\ny")},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			buf := NewLogBuffer(10)
			h := &teeHandler{w: io.Discard, buf: buf}

			r := slog.NewRecord(time.Now(), slog.LevelWarn, c.msg, 0)
			r.AddAttrs(c.attr)
			if err := h.Handle(context.Background(), r); err != nil {
				t.Fatalf("Handle: %v", err)
			}

			lines, _ := buf.Since(0)
			if len(lines) != 1 {
				t.Fatalf("一条记录应只产生一行，实际 %d 行: %q", len(lines), lines)
			}
			if strings.ContainsAny(lines[0], "\r\n") {
				t.Fatalf("行内仍有换行符，界面会折出顶格的续行: %q", lines[0])
			}
		})
	}
}
