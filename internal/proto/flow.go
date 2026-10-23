package proto

import (
	"sync"
	"time"
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
// 已知限制：TCP 靠 FIN/RST 即时回收、UDP 靠空闲期；异常断开且没发 FIN/RST 的流只能等
// 空闲兜底。表满时新流一律退回纯哈希（老条目不能随便淘汰 —— 淘汰谁就等于让那条流换槽、
// 内层 TCP 乱序）。新流选槽仍持全局锁，高并发建流时是串行的，属于后续工作。
// flowEntry 是一条内层流的选槽映射。
//
// lastSeen 只用于回收：TCP 看到 FIN/RST 会立即删映射，这个时间只是异常断开
// （没发 FIN/RST 就消失）的兜底；UDP 没有结束标记，只能靠它判空闲。
type flowEntry struct {
	slot     int
	lastSeen int64 // UnixNano
	proto    uint8
}

// 回收期限。TCP 的短连接大多有 FIN/RST，空闲兜底取长一点，免得误删正在
// 长跑但暂时安静的流；UDP 没有结束标记，只能靠空闲期，取得保守一些。
const (
	tcpFlowIdle   = 5 * time.Minute
	udpFlowIdle   = 5 * time.Minute
	sweepInterval = time.Second
)

type FlowTable struct {
	mu    sync.Mutex
	m     map[flowKey]flowEntry
	max   int
	lastN int
	// fallbacks 是表满后新流退回哈希的次数。只增不减，和 Size 一起看才能判断
	// 「均衡能力退化了多少」：占用贴顶并且回退一直涨，说明这个会话的流表已经没用了。
	fallbacks uint64
	// reclaimed 是因 FIN/RST 或空闲期被回收的映射数。回收生效时它应该持续上涨，
	// 而 Size 稳定在活跃流数量级。
	reclaimed uint64
	// lastSweep 是上一次全表清扫的时间，用来给满表时的兜底回收节流。
	lastSweep int64
	// counts[s] 是映射到槽 s 的流数。之所以要单独记：新流分配原先是遍历整张映射
	// 数一遍，表接近上限后每条新流都是 O(表大小)，累计 O(N²)。计数只随单条映射
	// 增减而变，选槽就只剩 O(槽数)；计数一旦漂移，某个槽会永远选不到，所以每条
	// 改映射的路径都必须同步维护它。
	counts []int
	// clock 是时间源，测试注入用；nil 表示 time.Now。
	clock func() time.Time
}

// maxFlows 是流表容量上限。每条 entry 约 24 字节，三万条约 1MB 级（含 map 开销），
// 对两端都不算负担。上限不是"永不淘汰"的借口：FIN/RST 和空闲期回收才是控制占用的
// 手段，这个数只是兜底。
const maxFlows = 30000

// flowKey 是规范化的流标识，可比较。长度随协议类型不同（9~13 字节），用 len 区分。
type flowKey struct {
	b   [13]byte
	len uint8
}

func NewFlowTable() *FlowTable {
	return &FlowTable{m: make(map[flowKey]flowEntry, 256), max: maxFlows}
}

func (t *FlowTable) now() time.Time {
	if t.clock != nil {
		return t.clock()
	}
	return time.Now()
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
	key, proto, flags, ok := flowKeyMeta(payload)
	if !ok {
		// 畸形包/短包：沿用纯哈希兜底，绝不 panic
		return Slot(payload, n)
	}
	now := t.now()

	t.mu.Lock()
	defer t.mu.Unlock()

	// 成员数变了（正常一个会话内不会变，这是防御）：映射里指向已不存在槽的条目必须
	// 处理，否则会越界。但**绝不能整表清空** —— 那会让还在有序传输的流换槽，内层 TCP
	// 直接乱序。只删落在被移除槽上的那些：它们的包本来就已经发不出去（槽没了）。
	//
	// 计数只在槽集合变化时按映射重建：这一步仍是 O(表大小)，但成员补拨很少发生；平时
	// 每次分配只碰单条计数，不再扫全表。
	if t.lastN != n {
		for k, e := range t.m {
			if e.slot >= n {
				delete(t.m, k)
			}
		}
		t.lastN = n
		t.counts = growCounts(t.counts, n)
		clear(t.counts)
		for _, e := range t.m {
			if e.slot > 0 && e.slot < n {
				t.counts[e.slot]++
			}
		}
	} else if len(t.counts) < n {
		// 零值或手工构造的表可能没走过上面的重建，补齐长度即可，计数全零也是对的。
		t.counts = growCounts(t.counts, n)
	}

	if e, ok := t.m[key]; ok {
		// TCP FIN/RST：连接正在结束。本包仍走原槽（不制造这一次乱序），随后删掉
		// 映射，让后续同 key 的包（如果有）重新分配。短连接靠这一条回收 ——
		// 不回收的话，长会话里的表会被已经关闭的连接吃满，新流只能退回哈希。
		if proto == 6 && flags&(tcpFlagFIN|tcpFlagRST) != 0 {
			delete(t.m, key)
			t.counts[e.slot]--
			t.reclaimed++
			return e.slot
		}
		// 映射指向的槽可能已经死了（成员断开还没补上）。原来这里直接返回它，那条流
		// 就持续往死槽发、被 flushBatch 整批丢掉，直到那个槽补拨回来 —— 实测能黑到
		// 几十秒，用户看到的是「显示已连接、延迟在跳，但这条流就是不动」。
		//
		// 迁移一次：只迁一次（迁完映射立刻更新），内层 TCP 的乱序只发生在这一刻，
		// 比持续黑洞好得多。这也是本表唯一允许改映射的情形。
		if alive == nil || alive(e.slot) {
			t.m[key] = flowEntry{slot: e.slot, lastSeen: now.UnixNano(), proto: e.proto}
			return e.slot
		}
		delete(t.m, key)
		t.counts[e.slot]--
		// 落到下面重新选槽
	}
	// 定期回收空闲流：不能只在满表时才扫。没发 FIN/RST 的流（异常断开、UDP）靠
	// 空闲期回收；等到表满才扫的话，flow 会一路涨到上限才回落，看起来就是
	// 「回收不理想」。每秒最多一扫，全表遍历约 1ms 量级。
	if now.UnixNano()-t.lastSweep > int64(sweepInterval) {
		t.lastSweep = now.UnixNano()
		t.sweepLocked(now)
	}
	if len(t.m) >= t.max {
		// 新流不插入（老流的映射绝不能动 —— 那会让内层 TCP 乱序），但仍必须
		// 在**活槽**里选，直接纯哈希会把这批新流分到已经断开的成员槽上。
		t.fallbacks++
		return hashToAlive(payload, n, alive)
	}

	best := 0
	for i := 1; i < n; i++ {
		if alive != nil && !alive(i) {
			continue
		}
		if best == 0 || t.counts[i] < t.counts[best] {
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
	t.m[key] = flowEntry{slot: best, lastSeen: now.UnixNano(), proto: proto}
	t.counts[best]++
	return best
}

// sweepLocked 回收所有超过空闲期的映射。由 lastSweep 节流，每个 Slot 调用都会
// 检查一次，但真正扫表最多每秒一次。
func (t *FlowTable) sweepLocked(now time.Time) {
	for k, e := range t.m {
		limit := tcpFlowIdle
		if e.proto == 17 {
			limit = udpFlowIdle
		}
		if now.Sub(time.Unix(0, e.lastSeen)) <= limit {
			continue
		}
		delete(t.m, k)
		if e.slot > 0 && e.slot < len(t.counts) {
			t.counts[e.slot]--
		}
		t.reclaimed++
	}
}

// FlowStats 是流表当前状态的快照，只给诊断输出用。
//
// 报告要求先量「占用率、满表回退次数、各槽活跃映射数」再决定要不要做 FIN/RST 回收：
// 没有这三个数，谁也说不清长会话里的短连接到底有没有把表吃满。
type FlowStats struct {
	Size      int    // 已建立的映射数
	Max       int    // 容量上限
	Fallbacks uint64 // 表满后新流退回哈希的次数
	Reclaimed uint64 // 因 FIN/RST 或空闲期回收的映射数
	TCPCount  int    // 映射里的 TCP 流数
	UDPCount  int    // 映射里的 UDP 流数
	PerSlot   []int  // 各槽承载的流数，下标即槽号
}

// Stats 返回流表快照。锁开销可忽略：调用方是 10 分钟一次的状态日志。
func (t *FlowTable) Stats() FlowStats {
	t.mu.Lock()
	defer t.mu.Unlock()
	per := make([]int, len(t.counts))
	copy(per, t.counts)
	tcp, udp := 0, 0
	for _, e := range t.m {
		switch e.proto {
		case 6:
			tcp++
		case 17:
			udp++
		}
	}
	return FlowStats{
		Size: len(t.m), Max: t.max, Fallbacks: t.fallbacks, Reclaimed: t.reclaimed,
		TCPCount: tcp, UDPCount: udp, PerSlot: per,
	}
}

// growCounts 保证计数切片至少覆盖 n 个槽，已有计数原样保留。
func growCounts(c []int, n int) []int {
	if len(c) < n {
		c = append(c, make([]int, n-len(c))...)
	}
	return c
}

// hashToAlive 用纯哈希定一个起点，再沿环找到第一个活槽。
//
// 只靠哈希不够：它会落在断开的成员槽上（尽管还有健康成员）。但也**不能**随便挑
// 一个活槽 —— 必须让「同一个包 + 同一存活集合」永远落在同一个槽，否则同一条内层
// 流会在不同外层连接之间跳，TCP 直接乱序。
func hashToAlive(payload []byte, n int, alive func(int) bool) int {
	h := Slot(payload, n)
	if alive == nil || alive(h) {
		return h
	}
	for i := 0; i < n-1; i++ {
		c := ((h-1+i)%(n-1) + 1)
		if alive(c) {
			return c
		}
	}
	// 一个活槽都没有：保持旧行为（纯哈希），等成员补拨
	return h
}
