package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/antapp-cc/antapp-link/internal/pki"
)

func TestBackoffSequence(t *testing.T) {
	// 起点 10 秒、封顶 3 分钟：链路抖动往往几秒才恢复，1 秒起步会在对端刚清理连接时
	// 反复失败，日志看着像在疯狂重连。
	want := []time.Duration{
		10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second,
		160 * time.Second, 3 * time.Minute, 3 * time.Minute,
	}
	for i, w := range want {
		if got := Backoff(i); got != w {
			t.Errorf("Backoff(%d) = %v, want %v", i, got, w)
		}
	}
	if got := Backoff(1000); got != 3*time.Minute {
		t.Errorf("退避必须封顶在 3 分钟，实际 %v", got)
	}
	if got := Backoff(-5); got != 10*time.Second {
		t.Errorf("负数次数应按首次处理，实际 %v", got)
	}
}

func testInvite(t *testing.T) pki.Invite {
	t.Helper()
	pkiDir := filepath.Join(t.TempDir(), "pki")
	if err := pki.Init(pkiDir); err != nil {
		t.Fatal(err)
	}
	inv, err := pki.Issue(pkiDir, "pinode-01", "103.143.11.34:62233", pki.TunnelParams{
		TunnelIP: "10.10.0.2",
		Gateway:  "10.10.0.1",
		Prefix:   24,
		MTU:      1400,
		DNS:      []string{"8.8.8.8"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

func TestLoadInviteFromSingleLineCode(t *testing.T) {
	inv := testInvite(t)
	code, err := inv.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := LoadInvite(code)
	if err != nil {
		t.Fatalf("LoadInvite(单行码): %v", err)
	}
	if got.Name != "pinode-01" || got.Server != "103.143.11.34:62233" {
		t.Errorf("解析结果不对: %+v", got.Name)
	}
}

func TestLoadInviteFromJSONFile(t *testing.T) {
	inv := testInvite(t)
	raw, err := json.MarshalIndent(inv, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "antapp-node-pinode-01.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadInvite(path)
	if err != nil {
		t.Fatalf("LoadInvite(JSON 文件): %v", err)
	}
	if got.TunnelIP != "10.10.0.2" {
		t.Errorf("TunnelIP = %q", got.TunnelIP)
	}
}

// 用户常把单行码存成 txt 再传过来，这条路径也得走通。
func TestLoadInviteFromTextFileHoldingCode(t *testing.T) {
	inv := testInvite(t)
	code, err := inv.Encode()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "code.txt")
	if err := os.WriteFile(path, []byte(code+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadInvite(path); err != nil {
		t.Fatalf("LoadInvite(文本文件里的单行码): %v", err)
	}
	// 首尾带空白的粘贴内容也要能用
	if _, err := LoadInvite("  " + code + "  "); err != nil {
		t.Fatalf("LoadInvite(带空白): %v", err)
	}
}

func TestLoadInviteErrors(t *testing.T) {
	if _, err := LoadInvite(""); err == nil {
		t.Error("空输入必须报错")
	}
	if _, err := LoadInvite(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Error("不存在的文件必须报错")
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadInvite(bad); err == nil {
		t.Error("垃圾内容必须报错")
	}
}

// 单条成员连接断掉要尽快补回来：数据还能走其他槽，让它干等只会把多连接拖成少连接、
// 吞吐直接掉。只有连续失败才退避，而且档位要短 —— 长退避是留给整条隧道断开用的。
func TestMemberRetryDelay(t *testing.T) {
	if got := memberRetryDelay(0); got != 0 {
		t.Errorf("成员补拨不该等待，实际 %v", got)
	}
	if got := memberRetryDelay(1); got != time.Second {
		t.Errorf("首次失败后退避 1 秒，实际 %v", got)
	}
	if got := memberRetryDelay(9); got != 15*time.Second {
		t.Errorf("成员补拨退避应封顶 15 秒，实际 %v", got)
	}
	// 起步阶段必须明显短于会话级起点：成员补拨退避的代价是吞吐，只有整条隧道断开
	// 才需要给对端留恢复时间。（封顶值可以接近会话起点，所以只比前几档）
	for i := 1; i <= 3; i++ {
		if memberRetryDelay(i) >= Backoff(0) {
			t.Errorf("第 %d 次成员补拨退避 %v 不该达到会话级起点 %v", i, memberRetryDelay(i), Backoff(0))
		}
	}
}

func TestAckParsesServerHandshake(t *testing.T) {
	raw := []byte(`{"tunnel_ip":"10.10.0.2","gateway":"10.10.0.1","prefix":24,"mtu":1400,"mss":1360,"dns":["8.8.8.8"]}`)
	var ack Ack
	if err := json.Unmarshal(raw, &ack); err != nil {
		t.Fatal(err)
	}
	if ack.TunnelIP != "10.10.0.2" || ack.Gateway != "10.10.0.1" || ack.MTU != 1400 || ack.MSS != 1360 {
		t.Errorf("HELLO_ACK 解析结果不对: %+v", ack)
	}
	if len(ack.DNS) != 1 || ack.DNS[0] != "8.8.8.8" {
		t.Errorf("DNS = %v", ack.DNS)
	}
}

// 断开日志里不该出现英文原文：认得出原因时只给中文原因 + 本端/对端地址；
// 只有认不出原因时才退回原始 error —— 排障总得有线索。
func TestConnLogAttrsPrefersChineseReason(t *testing.T) {
	server, peer := net.Pipe()
	defer server.Close()
	defer peer.Close()

	toMap := func(attrs []any) map[string]any {
		m := map[string]any{}
		for i := 0; i+1 < len(attrs); i += 2 {
			m[fmt.Sprint(attrs[i])] = attrs[i+1]
		}
		return m
	}

	m := toMap(connLogAttrs(
		errors.New("read tcp 1.2.3.4:1->5.6.7.8:2: read: connection reset by peer"), server, "member", 2))
	if got := m["reason"]; got != "对端强制断开（连接被重置）" {
		t.Errorf("reason = %v", got)
	}
	if _, ok := m["err"]; ok {
		t.Error("认得出原因时不该再带英文 err")
	}
	if m["local"] == nil || m["remote"] == nil {
		t.Error("应带上本端/对端地址，否则不知道是哪条连接")
	}
	if m["member"] != 2 {
		t.Error("调用方传进来的字段要保留")
	}

	m = toMap(connLogAttrs(errors.New("something entirely unexpected"), server))
	if _, ok := m["err"]; !ok {
		t.Error("认不出原因时必须附原始 err，否则排障没有线索")
	}
}

// 连接断开的原始 error 在 Windows 上是一长串英文（且不含 timeout 这类关键词），
// 日志里得配一句人话原因，排障时一眼就知道是哪种断法。
func TestConnBrokenReason(t *testing.T) {
	cases := []struct {
		name string
		err  string
		want string
	}{
		{
			name: "Windows 的对端无响应（用户实际遇到的那条）",
			err: "read tcp 192.168.5.15:60451->103.142.86.145:62233: wsarecv: A connection attempt " +
				"failed because the connected party did not properly respond after a period of time, " +
				"or established connection failed because connected host has failed to respond.",
			want: "对端一段时间没有响应",
		},
		{
			name: "Windows 的 RST",
			err:  "read tcp 192.168.5.15:1->1.2.3.4:5: wsarecv: An existing connection was forcibly closed by the remote host.",
			want: "对端强制断开",
		},
		{
			name: "Linux 的 RST",
			err:  "read tcp 10.0.0.1:1->1.2.3.4:5: read: connection reset by peer",
			want: "对端强制断开",
		},
		{
			name: "本端主动关闭（写超时后我们关的）",
			err:  "read tcp 10.0.0.1:1->1.2.3.4:5: use of closed network connection",
			want: "本端主动关闭",
		},
		{
			name: "对端正常关闭",
			err:  "read tcp 10.0.0.1:1->1.2.3.4:5: EOF",
			want: "对端正常关闭",
		},
		{
			name: "网络不可达",
			err:  "dial tcp 1.2.3.4:5: connect: network is unreachable",
			want: "网络不可达",
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
			err:  "some entirely unexpected failure",
			want: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := connBrokenReason(fmt.Errorf("%s", c.err))
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

// 会话结束的原因归类：EOF（服务端干净关闭）降为信息级并写明常见原因，
// 网络类异常保持警告级。这行日志是排障的第一入口，级别和原因不能含糊。
func TestSessionEndInfo(t *testing.T) {
	lvl, reason := sessionEndInfo(fmt.Errorf("读隧道: EOF"))
	if lvl != slog.LevelInfo || reason == "" {
		t.Errorf("EOF 应为信息级且带原因，实际 lvl=%v reason=%q", lvl, reason)
	}
	lvl, reason = sessionEndInfo(fmt.Errorf("连接 1.2.3.4: i/o timeout"))
	if lvl != slog.LevelWarn || !strings.Contains(reason, "超时") {
		t.Errorf("超时应为警告级且带原因，实际 lvl=%v reason=%q", lvl, reason)
	}
	if _, reason := sessionEndInfo(fmt.Errorf("其它未知错误")); reason != "" {
		t.Errorf("未知错误不该编造原因，实际 %q", reason)
	}
}
