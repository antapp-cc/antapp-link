package client

import (
	"strings"
	"testing"
)

// 所有往界面日志区写的文本都必须经过 wrapToMessageColumn。
//
// 界面里日志区的自动折行已经被关掉（EM_SETTARGETDEVICE），如果哪个调用点绕过了
// 这里直接塞原始文本，超宽内容就会被控件截断或者自己折出顶格的续行 —— 后者会把
// 内容压在日期时间那一列上，正是用户反复提的问题。
func TestWrapKeepsContinuationIndented(t *testing.T) {
	lines := []string{
		// 长短不一、中英混排、带标点和不带标点的都要覆盖
		"10-20 10:27:40  ▲  连接失败  错误=连接服务端失败：连接被拒绝",
		"10-20 10:19:24  ·  隧道已建立 服务端=103.142.86.145:62233 隧道IP=10.10.0.2 网关=10.10.0.1 mtu=1400 会话号=3bd96649477c30176443555dac6a407c want_members=4 ack_members=4",
		"10-20 10:19:50  ·  等待重连：还剩 30 秒",
		// 一整串没有空格和标点的长内容（比如 hash）：找不到断点只能硬切，
		// 但续行仍然必须缩进。
		"10-20 10:19:24  ·  " + strings.Repeat("a", 300),
		// 中文长串同理
		"10-20 10:19:24  ·  " + strings.Repeat("中", 120),
	}

	for _, line := range lines {
		got := wrapToMessageColumn(line)
		for i, part := range strings.Split(got, "\n") {
			if i == 0 {
				continue // 第一行本来就以时间戳开头
			}
			if !strings.HasPrefix(part, continuationIndent) {
				t.Fatalf("续行没有缩进到消息列，会压在日期时间上：\n原行: %s\n续行: %q", line, part)
			}
			if displayUnits(part) > maxLineUnits {
				t.Fatalf("续行缩进后仍超出宽度上限 %d（实际 %d）：\n%q",
					maxLineUnits, displayUnits(part), part)
			}
		}
	}
}

// 短消息不该被折开：一行能说完就不要多出一行。
func TestWrapLeavesShortLinesAlone(t *testing.T) {
	line := "10-20 10:31:13  ▲  连接失败  错误=连接服务端失败：连接被拒绝"
	if got := wrapToMessageColumn(line); got != line {
		t.Fatalf("短消息不应折行：\n输入: %s\n输出: %s", line, got)
	}
}

// 折行点应该落在空格或中文标点后面，不能把 IP 或 key=value 切两半。
func TestWrapBreaksAtReadableBoundaries(t *testing.T) {
	line := "10-20 10:19:27  ▲  连接失败  错误=连接服务端失败，未改动网络：连接 103.142.86.145:62233 失败：连接被拒绝"
	first := strings.Split(wrapToMessageColumn(line), "\n")[0]

	// 第一行不能以半个 IP 结尾
	if strings.HasSuffix(first, "103.1") || strings.HasSuffix(first, "103.142") {
		t.Fatalf("把 IP 切开了：%q", first)
	}
	// 第一行不能以 key= 结尾（值被甩到下一行）
	if strings.HasSuffix(first, "=") {
		t.Fatalf("把 key=value 切开了：%q", first)
	}
}

// displayUnits 与 wrapToMessageColumn 内部同一套宽度估算：ASCII=1，非 ASCII=2。
func displayUnits(s string) int {
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
