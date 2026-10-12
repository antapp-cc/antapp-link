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

// Slot 返回该 IP 包应走的槽位号，用内层五元组的纯哈希决定。0 是控制连接，只在
// 单连接模式与畸形包兜底时出现。
//
// 数据刻意**不**落在槽 0：槽 0 是控制连接，PING/PONG 与它共用同一条 TCP。
// 数据挤进去时心跳的写会排在大批数据后面，撞上写超时（20s）就是整个会话重连
// （实测每 1~2 分钟一次，日志表现为「成员连接断开」与「会话结束」同一毫秒出现）。
// 所以 n>1 时值域是 1..n-1；n<=1（单连接模式）没有成员槽可用，只能退回 0。
//
// 纯哈希不感知负载：一条大流撞上哪条连接就塞满哪条（实测 4 条下出现过
// 1.4MB / 1.4MB / 58KB / 0）。发送端要均衡请用 FlowTable；这个函数留着是为了
// 兜底畸形包、以及给 FlowTable 在流表满时退让。
//
// 规则（两端必须一致，勿改）：
//   - 非 IPv4 / 畸形包 → 0（控制连接兜底，绝不 panic）；
//   - IP 分片（MF 置位或 frag_off≠0）→ 以 (src, dst, protocol, ip_id) 哈希，
//     同一报文的各分片同槽；
//   - TCP/UDP → 以规范化端点对（字节序小者在前）+ protocol 哈希，方向无关；
//   - 其他协议 → 以 (src, dst, protocol) 哈希。
//
// 哈希：FNV-1a 32 位；结果对 n-1 取模后加 1。
func Slot(payload []byte, n int) int {
	if n <= 1 || len(payload) < 20 || payload[0]>>4 != 4 {
		return 0
	}
	key, ok := flowKeyOf(payload)
	if !ok {
		return 0
	}
	h := fnv1a{}
	// 1 + (哈希 % (n-1))：值域 1..n-1，把槽 0 留给控制连接
	return 1 + int(h.sum(key.b[:key.len])%uint32(n-1))
}

// flowKeyOf 把 IP 包规范化成可比较的流标识：同一内层流的两个方向算出同一个 key。
// 第二个返回值为 false 表示包太短或不是 IPv4 —— 调用方退回控制连接或纯哈希。
func flowKeyOf(payload []byte) (flowKey, bool) {
	var k flowKey
	if len(payload) < 20 || payload[0]>>4 != 4 {
		return k, false
	}

	protocol := payload[9]
	srcIP := payload[12:16]
	dstIP := payload[16:20]

	fragOff := binary.BigEndian.Uint16(payload[6:8]) & 0x1FFF
	moreFrag := payload[6]&0x20 != 0

	switch {
	case fragOff != 0 || moreFrag:
		// 分片：传输层头只有首片带，用 ip_id 把同一报文的各片绑在一起
		n := copy(k.b[0:4], srcIP)
		n += copy(k.b[n:n+4], dstIP)
		k.b[n] = protocol
		n++
		binary.BigEndian.PutUint16(k.b[n:n+2], binary.BigEndian.Uint16(payload[4:6]))
		k.len = uint8(n + 2)

	case protocol == 6 || protocol == 17:
		if len(payload) < 24 {
			return k, false
		}
		sport := payload[20:22]
		dport := payload[22:24]
		// 规范化端点对：字节序小者在前 → 方向无关
		n := 0
		if endpointLess(srcIP, sport, dstIP, dport) {
			n += copy(k.b[n:n+4], srcIP)
			n += copy(k.b[n:n+2], sport)
			n += copy(k.b[n:n+4], dstIP)
			n += copy(k.b[n:n+2], dport)
		} else {
			n += copy(k.b[n:n+4], dstIP)
			n += copy(k.b[n:n+2], dport)
			n += copy(k.b[n:n+4], srcIP)
			n += copy(k.b[n:n+2], sport)
		}
		k.b[n] = protocol
		k.len = uint8(n + 1)

	default:
		n := copy(k.b[0:4], srcIP)
		n += copy(k.b[n:n+4], dstIP)
		k.b[n] = protocol
		k.len = uint8(n + 1)
	}
	return k, true
}

// endpointLess 按字节序比较两个 (ip, port) 端点，用于把端点对规范化成方向无关。
func endpointLess(ipA, portA, ipB, portB []byte) bool {
	for i := 0; i < 4; i++ {
		if ipA[i] != ipB[i] {
			return ipA[i] < ipB[i]
		}
	}
	for i := 0; i < 2; i++ {
		if portA[i] != portB[i] {
			return portA[i] < portB[i]
		}
	}
	return true
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
