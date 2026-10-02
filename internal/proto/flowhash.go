package proto

import (
	"encoding/binary"
)

// TypeMemberAck 是服务端对成员连接握手的确认（多连接并发数据面，见
// docs/superpowers/specs/2026-10-01-multi-tcp-ai-spec.md）。载荷为空。
// 老版本收到此类型会按未知类型忽略——帧协议的前向兼容契约。
const TypeMemberAck Type = 0x08

// MaxBatchBytes 是写合并的单批上限：一条 TLS 记录最多装 ~11 帧（MTU 1400），
// 超过就先发出去，避免单次 Write 撑出超过路径 MTU 的外层大包。
const MaxBatchBytes = 16384

// AppendHeader 向 dst 追加 4 字节帧头，返回扩展后的切片。
// 供写合并路径拼装 [hdr|payload][hdr|payload]… 单缓冲；WriteFrame 语义不变。
func AppendHeader(dst []byte, t Type, n int) []byte {
	if n > MaxPayload {
		// 与 WriteFrame 的超长检查保持同一契约；调用方保证不越界，这里兜底。
		return dst
	}
	var hdr [HeaderSize]byte
	hdr[0] = byte(t)
	hdr[1] = byte(n >> 16)
	hdr[2] = byte(n >> 8)
	hdr[3] = byte(n)
	return append(dst, hdr[:]...)
}

// Slot 返回该 IP 包应走的槽位号（0 = 控制连接）。
//
// 多连接并发数据面的流路由：同一内层流（规范化五元组）的所有包永远返回同一
// 槽位——内层 TCP 绝不能看到乱序，这是方案的正确性基石。
//
// 规则（两端必须一致，勿改）：
//   - 非 IPv4 / 畸形包 → 0（控制连接兜底，绝不 panic）；
//   - IP 分片（MF 置位或 frag_off≠0）→ 以 (src, dst, protocol, ip_id) 哈希，
//     同一报文的各分片同槽；
//   - TCP/UDP → 以规范化端点对（字节序小者在前）+ protocol 哈希，方向无关；
//   - 其他协议 → 以 (src, dst, protocol) 哈希。
//
// 哈希：FNV-1a 32 位；结果对 n 取模。n <= 1 时恒返 0。
func Slot(payload []byte, n int) int {
	if n <= 1 || len(payload) < 20 || payload[0]>>4 != 4 {
		return 0
	}

	protocol := payload[9]
	srcIP := payload[12:16]
	dstIP := payload[16:20]

	h := fnv1a{}
	var input []byte

	fragOff := binary.BigEndian.Uint16(payload[6:8]) & 0x1FFF
	moreFrag := payload[6]&0x20 != 0
	if fragOff != 0 || moreFrag {
		// 分片：传输层头不完整/只有首片有，用 ip_id 绑定同一报文的各片
		input = append(input, srcIP...)
		input = append(input, dstIP...)
		input = append(input, protocol)
		input = append(input, payload[4:6]...)
	} else if protocol == 6 || protocol == 17 {
		if len(payload) < 24 {
			return 0
		}
		sport := payload[20:22]
		dport := payload[22:24]
		// 规范化端点对：字节序比较，小者在前 → 方向无关
		a := append(append([]byte{}, srcIP...), sport...)
		b := append(append([]byte{}, dstIP...), dport...)
		if lexicographic(a, b) {
			input = append(input, a...)
			input = append(input, b...)
		} else {
			input = append(input, b...)
			input = append(input, a...)
		}
		input = append(input, protocol)
	} else {
		input = append(input, srcIP...)
		input = append(input, dstIP...)
		input = append(input, protocol)
	}

	return int(h.sum(input) % uint32(n))
}

type fnv1a struct{ v uint32 }

func (h *fnv1a) sum(b []byte) uint32 {
	h.v = 2166136261
	for _, c := range b {
		h.v ^= uint32(c)
		h.v *= 16777619
	}
	return h.v
}

func lexicographic(a, b []byte) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return true
}
