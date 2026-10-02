package server

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
)

// 断开日志不摆英文原文：认得出原因就只给中文原因 + 本端/对端地址，
// 只有认不出时才退回原始 error 当线索。
func TestConnLogAttrsServer(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	toMap := func(attrs []any) map[string]any {
		m := map[string]any{}
		for i := 0; i+1 < len(attrs); i += 2 {
			m[fmt.Sprint(attrs[i])] = attrs[i+1]
		}
		return m
	}

	m := toMap(connLogAttrs(
		errors.New("read tcp 1.1.1.1:1->2.2.2.2:2: read: connection reset by peer"), a, "member", 1))
	if got := m["reason"]; got != "对端强制断开（连接被重置）" {
		t.Errorf("reason = %v", got)
	}
	if _, ok := m["err"]; ok {
		t.Error("认得出原因时不该再带英文 err")
	}
	if m["local"] == nil || m["remote"] == nil {
		t.Error("应带上本端/对端地址，否则不知道是哪条连接")
	}
	if m["member"] != 1 {
		t.Error("调用方传进来的字段要保留")
	}

	m = toMap(connLogAttrs(errors.New("something entirely unexpected"), a))
	if _, ok := m["err"]; !ok {
		t.Error("认不出原因时必须附原始 err，否则排障没有线索")
	}
}

// 服务端日志里同样是原始英文 error，配一句人话原因才好排障。
// 这份映射与客户端那边的 connBrokenReason 对应（那边走过红→绿）。
func TestConnBrokenReasonServer(t *testing.T) {
	cases := []struct{ name, err, want string }{
		{
			name: "对端 RST",
			err:  "read tcp 103.142.86.145:62233->221.11.94.143:19763: read: connection reset by peer",
			want: "对端强制断开",
		},
		{
			name: "本端主动关闭（写超时后我们关的）",
			err:  "read tcp 103.142.86.145:62233->221.11.94.143:19763: use of closed network connection",
			want: "本端主动关闭",
		},
		{
			name: "对端正常关闭",
			err:  "read tcp 103.142.86.145:62233->221.11.94.143:19763: EOF",
			want: "对端正常关闭",
		},
		{
			name: "读超时",
			err:  "read tcp 103.142.86.145:62233->221.11.94.143:19763: i/o timeout",
			want: "对端一段时间没有响应",
		},
		{
			name: "端口被占用",
			err:  "listen tcp 0.0.0.0:31400: bind: address already in use",
			want: "端口已被占用",
		},
		{
			name: "TLS 没带客户端证书",
			err:  "tls: client didn't provide a certificate",
			want: "没有提供证书",
		},
		{
			name: "证书不被信任",
			err:  "x509: certificate signed by unknown authority",
			want: "不被信任",
		},
		{
			name: "写不进去（对端已断）",
			err:  "write tcp 1.2.3.4:5->6.7.8.9:10: write: broken pipe",
			want: "对端已经断开",
		},
		{
			name: "认不出来就别编",
			err:  "totally unexpected failure",
			want: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := connBrokenReason(errors.New(c.err))
			if c.want == "" {
				if got != "" {
					t.Errorf("认不出的错误不该编原因，实际 %q", got)
				}
				return
			}
			if !strings.Contains(got, c.want) {
				t.Errorf("原因 %q 应包含 %q", got, c.want)
			}
		})
	}
}
