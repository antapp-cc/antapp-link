package client

import (
	"encoding/binary"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mkTCP 造一个最小 IPv4 + TCP 头（够解析五元组和 seq）。
func mkTCP(src, dst string, sport, dport int, seq uint32) []byte {
	pkt := make([]byte, 40)
	pkt[0] = 0x45
	binary.BigEndian.PutUint16(pkt[2:4], 40)
	pkt[9] = 6
	copy(pkt[12:16], net.ParseIP(src).To4())
	copy(pkt[16:20], net.ParseIP(dst).To4())
	binary.BigEndian.PutUint16(pkt[20:22], uint16(sport))
	binary.BigEndian.PutUint16(pkt[22:24], uint16(dport))
	binary.BigEndian.PutUint32(pkt[24:28], seq)
	return pkt
}

func TestOrderTracerRecordsSlotAndFlow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.log")
	tr, err := NewOrderTracer(path)
	if err != nil {
		t.Fatal(err)
	}
	tr.Record("tx", 2, mkTCP("10.10.0.2", "1.1.1.1", 40000, 443, 1234))
	tr.Record("rx", 0, mkTCP("1.1.1.1", "10.10.0.2", 443, 40000, 5678))
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 3 { // 一行表头 + 两行记录
		t.Fatalf("期望 3 行（含表头），实际 %d 行：%v", len(lines), lines)
	}
	if !strings.HasPrefix(lines[0], "#") {
		t.Errorf("第一行应是表头，实际 %q", lines[0])
	}
	if !strings.Contains(lines[1], " tx 2 10.10.0.2 40000 1.1.1.1 443 6 1234 40") {
		t.Errorf("上行记录不对：%q", lines[1])
	}
	if !strings.Contains(lines[2], " rx 0 1.1.1.1 443 10.10.0.2 40000 6 5678 40") {
		t.Errorf("下行记录不对：%q", lines[2])
	}
}

// 非 IPv4 / 太短的包不该写进去，也不该 panic。
func TestOrderTracerSkipsNonIPv4(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.log")
	tr, err := NewOrderTracer(path)
	if err != nil {
		t.Fatal(err)
	}
	tr.Record("tx", 1, []byte{0x60, 0x00, 0x00}) // IPv6 头
	tr.Record("tx", 1, []byte{0x45, 0x00})       // 截断
	tr.Record("tx", 1, mkTCP("10.0.0.1", "10.0.0.2", 1, 2, 3))
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}

	raw, _ := os.ReadFile(path)
	if got := strings.Count(string(raw), "\n"); got != 2 { // 表头 + 唯一一条有效记录
		t.Errorf("表头 + 1 条有效记录 = 2 行，实际 %d 行", got)
	}
}

// 默认关闭：nil tracer 必须是安全的空操作，数据面才不会为联调付出代价。
func TestOrderTracerNilIsNoop(t *testing.T) {
	var tr *OrderTracer
	tr.Record("tx", 0, mkTCP("10.0.0.1", "10.0.0.2", 1, 2, 3))
	if err := tr.Close(); err != nil {
		t.Errorf("nil tracer 的 Close 应为 nil，实际 %v", err)
	}
}
