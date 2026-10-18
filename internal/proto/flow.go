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
//
// 已知限制：表只增不减，到 maxFlows 之后新流一律退回纯哈希。老条目不能随便淘汰 ——
// 淘汰谁就等于让那条流换槽、内层 TCP 乱序；要按最后活跃时间淘汰，也得先能确定那条流
// 已经结束。所以在内层短连接极多的长会话里，均衡能力会逐渐退化。另外新流选槽是
// O(表大小) 且持全局锁，端口扫描式的负载下会成为热点。都属于后续工作。
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
// 已有映射直接复用；新流分给「活着且承载流数最少」的槽。alive 报告某槽此刻是否真的
// 可用（成员连接在、能写），传 nil 视为全部可用。
//
// alive 不能省：把新流分给一个已经断开的空槽，那条流的下行会被 flushBatch 整批丢掉
// （槽空即丢，这是为了让数据不倒进心跳通道），表现为「部分新连接完全不通」，而且流表
// 不会自我纠正 —— 只要那条流还在，它就一直往死槽发。
func (t *FlowTable) Slot(payload []byte, n int, alive func(int) bool) int {
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

	// 成员数变了（正常一个会话内不会变，这是防御）：映射里指向已不存在槽的条目必须
	// 处理，否则会越界。但**绝不能整表清空** —— 那会让还在有序传输的流换槽，内层 TCP
	// 直接乱序。只删落在被移除槽上的那些：它们的包本来就已经发不出去（槽没了）。
	if t.lastN != n {
		for k, s := range t.m {
			if s >= n {
				delete(t.m, k)
			}
		}
		t.lastN = n
	}

	if s, ok := t.m[key]; ok {
		// 映射指向的槽可能已经死了（成员断开还没补上）。原来这里直接返回它，那条流
		// 就持续往死槽发、被 flushBatch 整批丢掉，直到那个槽补拨回来 —— 实测能黑到
		// 几十秒，用户看到的是「显示已连接、延迟在跳，但这条流就是不动」。
		//
		// 迁移一次：只迁一次（迁完映射立刻更新），内层 TCP 的乱序只发生在这一刻，
		// 比持续黑洞好得多。这也是本表唯一允许改映射的情形。
		if alive == nil || alive(s) {
			return s
		}
		delete(t.m, key)
		// 落到下面重新选槽
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
	best := 0
	for i := 1; i < n; i++ {
		if alive != nil && !alive(i) {
			continue
		}
		if best == 0 || counts[i] < counts[best] {
			best = i
		}
	}
	if best == 0 {
		// 一个活槽都没有（成员全断了）。
		//
		// 这里**不**回退到控制连接 0：槽 0 是心跳通道，数据挤上去面板延迟立刻上千
		// （实测过）。宁可让这条流暂时发不出去、等成员补拨（`maintainMembers` 在
		// 后台补），内层 TCP 自己会重传。调用方要为空槽批次计数，别像以前那样静默。
		return Slot(payload, n)
	}
	t.m[key] = best
	return best
}
