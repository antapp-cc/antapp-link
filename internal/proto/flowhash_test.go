package proto

import (
	"encoding/binary"
	"math/rand"
	"testing"
)

// mkIPv4 构造一个最小合法 IPv4 头 + 传输层前 4 字节。
func mkIPv4(src, dst [4]byte, protocol byte, sport, dport uint16, fragOff uint16, mf bool) []byte {
	b := make([]byte, 24)
	b[0] = 0x40 // v4
	binary.BigEndian.PutUint16(b[2:4], 24)
	binary.BigEndian.PutUint16(b[4:6], 0x1234) // ip_id
	b[8] = 64                                  // ttl
	b[9] = protocol
	copy(b[12:16], src[:])
	copy(b[16:20], dst[:])
	// 先写 fragOff 再叠 MF 位：MF 在 b[6] 高位，PutUint16 会覆盖它
	binary.BigEndian.PutUint16(b[6:8], fragOff)
	if mf {
		b[6] |= 0x20
	}
	binary.BigEndian.PutUint16(b[20:22], sport)
	binary.BigEndian.PutUint16(b[22:24], dport)
	return b
}

func TestSlotDirectionSymmetry(t *testing.T) {
	// src/dst 互换后必须同槽（方向无关）
	a := mkIPv4([4]byte{1, 2, 3, 4}, [4]byte{8, 8, 8, 8}, 6, 1234, 443, 0, false)
	b := mkIPv4([4]byte{8, 8, 8, 8}, [4]byte{1, 2, 3, 4}, 6, 443, 1234, 0, false)
	for n := 2; n <= 4; n++ {
		sa, sb := Slot(a, n), Slot(b, n)
		if sa != sb {
			t.Fatalf("n=%d 方向不对称: %d != %d", n, sa, sb)
		}
	}
}

func TestSlotFragmentConsistency(t *testing.T) {
	// 同一报文的两个分片（同 ip_id，frag_off 不同）必须同槽
	src, dst := [4]byte{10, 0, 0, 2}, [4]byte{93, 184, 216, 34}
	f1 := mkIPv4(src, dst, 6, 5555, 80, 0, true)     // 首片
	f2 := mkIPv4(src, dst, 6, 5555, 80, 1480, false) // 后续片（MF=0 但 offset≠0）
	for n := 2; n <= 4; n++ {
		if Slot(f1, n) != Slot(f2, n) {
			t.Fatalf("n=%d 分片不同槽", n)
		}
	}
}

func TestSlotStability(t *testing.T) {
	// 同一包重复调用必须同槽（确定性）
	b := mkIPv4([4]byte{1, 1, 1, 1}, [4]byte{2, 2, 2, 2}, 17, 100, 200, 0, false)
	first := Slot(b, 4)
	for i := 0; i < 100; i++ {
		if Slot(b, 4) != first {
			t.Fatal("Slot 不确定")
		}
	}
}

func TestSlotDifferentFlowsSpread(t *testing.T) {
	// 大样本下 4 槽都应被命中（分布性烟雾测试，不卡方）
	seen := map[int]bool{}
	for i := 0; i < 4000; i++ {
		sport := uint16(rand.Intn(65536))
		b := mkIPv4([4]byte{10, 0, 0, 2}, [4]byte{93, 184, 216, 34}, 6, sport, 443, 0, false)
		seen[Slot(b, 4)] = true
	}
	if len(seen) != 4 {
		t.Fatalf("4000 个流只覆盖 %d/4 槽", len(seen))
	}
}

func TestSlotEdgeCases(t *testing.T) {
	valid := mkIPv4([4]byte{1, 2, 3, 4}, [4]byte{5, 6, 7, 8}, 6, 1, 2, 0, false)

	if Slot(valid, 1) != 0 {
		t.Fatal("n=1 必须恒返 0")
	}
	if Slot(valid, 0) != 0 || Slot(valid, -3) != 0 {
		t.Fatal("n<=0 必须返 0")
	}
	// 非 IPv4
	v6 := make([]byte, 40)
	v6[0] = 0x60
	if Slot(v6, 4) != 0 {
		t.Fatal("IPv6 包必须返 0")
	}
	// 过短
	if Slot(valid[:8], 4) != 0 {
		t.Fatal("短包必须返 0")
	}
	// 畸形：声称 TCP 但头不完整（len=22 只够 IP 头）
	if Slot(valid[:22], 4) != 0 {
		t.Fatal("传输层头不完整必须返 0")
	}
	// 其他协议（ICMP=1）仍应给出哈希槽（不 panic）
	icmp := mkIPv4([4]byte{1, 2, 3, 4}, [4]byte{5, 6, 7, 8}, 1, 0, 0, 0, false)
	Slot(icmp, 4)
}

func TestAppendHeaderAndReadBack(t *testing.T) {
	// AppendHeader 拼出的帧必须能被 ReadFrame 读回（写合并的往返契约）
	payloads := [][]byte{nil, []byte("x"), make([]byte, MaxPayload)}
	var batch []byte
	for _, p := range payloads {
		batch = AppendHeader(batch, Type(0x03), len(p))
		batch = append(batch, p...)
	}

	rd := &sliceReader{batch}
	for i, want := range payloads {
		_ = want
		typ, got, err := ReadFrame(rd)
		if err != nil {
			t.Fatalf("帧 %d 读取失败: %v", i, err)
		}
		if typ != Type(0x03) {
			t.Fatalf("帧 %d 类型错: %v", i, typ)
		}
		if len(got) != len(want) {
			t.Fatalf("帧 %d 长度错: %d != %d", i, len(got), len(want))
		}
	}
}

func TestAppendHeaderOversize(t *testing.T) {
	// 超长载荷：AppendHeader 兜底不加头（调用方不可能走到，防御性契约）
	before := []byte("keep")
	got := AppendHeader(before, TypeIP, MaxPayload+1)
	if string(got) != "keep" {
		t.Fatal("超长载荷必须不加头")
	}
}

func TestWriteFrameUnchanged(t *testing.T) {
	// 禁止事项：WriteFrame 行为不得改变——回归锚
	var buf sliceBuffer
	if err := WriteFrame(&buf, TypePing, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	want := []byte{0x04, 0x00, 0x00, 0x05, 'h', 'e', 'l', 'l', 'o'}
	if string(buf.data) != string(want) {
		t.Fatalf("WriteFrame 字节流变化: % x", buf.data)
	}
}

type sliceReader struct{ b []byte }

func (r *sliceReader) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, ioEOF{}
	}
	n := copy(p, r.b)
	r.b = r.b[n:]
	return n, nil
}

type ioEOF struct{}

func (ioEOF) Error() string { return "EOF" }

type sliceBuffer struct{ data []byte }

func (b *sliceBuffer) Write(p []byte) (int, error) {
	b.data = append(b.data, p...)
	return len(p), nil
}
