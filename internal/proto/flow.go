package proto

import (
	"sync"
)

// FlowTable 把内层流固定到某个槽位，供发送端选槽。
//
// 为什么需要它：Slot() 是纯哈希，不感知负载。一条大流（比如节点机同步区块）撞上
// 哪条成员连接，就把那条的写队列塞满，直到 slotWriteTimeout 超时被摘掉重拨，而小流
// 所在的槽闲着 —— 实测 4 条连接下出现过 1.4MB / 1.4MB / 58KB / 0 的分布。
//
// 为什么只改发送端就够：接收端是「从哪条连接读到包」就知道槽位的（连接即槽），
// 帧头里没有、也不需要槽号。所以新旧两端混跑都成立，protoVersion 不必动。
//
// 唯一必须守住的约束：同一内层流的所有包永远走同一个槽 —— 内层 TCP 绝不能乱序。
// 因此映射一旦建立就不再改动（成员数变化的例外见 Slot）。
type FlowTable struct {
	mu    sync.Mutex
	m     map[flowKey]int
	max   int
	lastN int
}

// maxFlows 是流表容量上限。每条约 40 字节，两万条约 800KB，对两端都不算负担。
const maxFlows = 20000

// flowKey 是规范化的流标识，可比较。长度随协议类型不同（9~13 字节），用 len 区分。
type flowKey struct {
	b   [13]byte
	len uint8
}

func NewFlowTable() *FlowTable {
	return &FlowTable{m: make(map[flowKey]int, 256), max: maxFlows}
}

// Slot 返回该包应走的槽位（1..n-1）。
//
// 已有映射直接复用；新流分给当前承载流数最少的槽 —— 比单纯轮询更能适应成员增减，
// 代价是 O(表大小)，而只有新流会走到这里。
func (t *FlowTable) Slot(payload []byte, n int) int {
	if n <= 1 {
		return 0
	}
	key, ok := flowKeyOf(payload)
	if !ok {
		// 畸形包/短包：沿用纯哈希兜底，绝不 panic
		return Slot(payload, n)
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	// 成员数变了，槽位的含义跟着变，旧映射可能指到已不存在的槽。清掉重建：此时那些
	// 流的包本来就已经在丢（成员断开时槽空即丢），不存在「已经有序发出去又换槽」的问题。
	if t.lastN != n {
		t.m = make(map[flowKey]int, 256)
		t.lastN = n
	}

	if s, ok := t.m[key]; ok {
		return s
	}
	// 表满：新流不插入，退回哈希。老流的映射绝不能动 —— 那会让内层 TCP 乱序。
	if len(t.m) >= t.max {
		return Slot(payload, n)
	}

	counts := make([]int, n)
	for _, s := range t.m {
		if s > 0 && s < n {
			counts[s]++
		}
	}
	best := 1
	for i := 2; i < n; i++ {
		if counts[i] < counts[best] {
			best = i
		}
	}
	t.m[key] = best
	return best
}
