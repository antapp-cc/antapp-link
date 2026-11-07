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
// 已知限制：TCP 靠 RST、FIN 短宽限期或空闲期回收，UDP 靠空闲期；异常断开且没发
// RST/FIN 的流只能等空闲兜底。表满时不淘汰旧映射；能登记旁路条目时新流仍按负载
// 选槽，旁路表也溢出后只能按稳定哈希选，避免逐包换槽。未提供快照的兼容调用保留
// 纯哈希回退。新流选槽仍持全局锁。

// flowEntry 是一条内层流的选槽映射。
//
// lastSeen 只用于空闲兜底：TCP 看到 RST 会立即删映射、收到 FIN 后按短宽限期回收，
// 这个时间只是异常断开（没发 RST/FIN 就消失）的兜底；UDP 没有结束标记，只能靠它判空闲。
type flowEntry struct {
	slot     int
	lastSeen int64 // UnixNano
	// finAt 是收到 FIN 的时刻（UnixNano，0 = 未半关闭）。FIN 不再立即回收是为了
	// 保序，但也不能让 30k 容量绑在 5 分钟空闲期上 —— 半关闭流只留一个短宽限期。
	finAt int64
	proto uint8
}

// recentFlow 是被空闲清扫删掉、但短期内可能还有包回来的映射。
//
// 直接删会让下一包重新选槽，内层 TCP 乱序一次。留一个 60 秒的墓碑，同 key 的包
// 在这段时间内回来就恢复原槽；超过就彻底忘掉。
type recentFlow struct {
	slot      int
	deletedAt int64
	proto     uint8
}

// 回收期限。FIN 可能是半关闭，仍按 TCP 空闲期保留亲和，免得误删正在
// 长跑但暂时安静的流；UDP 没有结束标记，只能靠空闲期，取得保守一些。
const (
	tcpFlowIdle   = 5 * time.Minute
	udpFlowIdle   = 5 * time.Minute
	sweepInterval = time.Second
	// recentTTL 是删除后保留原槽的时间窗口；窗口内同 key 的包恢复原槽。
	recentTTL = 60 * time.Second
	// recentMax 限制墓碑数量，避免大量短连接把内存吃穿。
	recentMax = 8192
	// finHalfCloseIdle 是收到 FIN 后保留亲和的宽限期。正常关闭的短连接靠它回收；
	// 取 30 秒足够覆盖半关闭后剩余的 ACK/另一方向数据，又不会把表容量绑在 5 分钟上。
	finHalfCloseIdle = 30 * time.Second
)

type FlowTable struct {
	mu    sync.Mutex
	m     map[flowKey]flowEntry
	max   int
	lastN int
	// fallbacks 是主表满时进入回退分支的次数，包含旁路流的重复包，并非独立流数。
	fallbacks uint64
	// reclaimed 是主表因 RST 或空闲期被回收的映射数。回收生效时它应该持续上涨，
	// 而 Size 稳定在活跃流数量级。
	reclaimed uint64
	// lastSweep 把主表、旁路表和墓碑的全表清扫限制为每秒最多一次。
	lastSweep int64
	// counts[s] 是映射到槽 s 的流数。之所以要单独记：新流分配原先是遍历整张映射
	// 数一遍，表接近上限后每条新流都是 O(表大小)，累计 O(N²)。计数只随单条映射
	// 增减而变，选槽就只剩 O(槽数)；计数一旦漂移，某个槽会永远选不到，所以每条
	// 改映射的路径都必须同步维护它。
	counts []int
	// tcpCount/udpCount 维护映射里的协议分布，Stats 就不用再遍历全表。
	tcpCount int
	udpCount int
	// recent 是被空闲清扫删掉、短时间内可恢复原槽的映射。
	recent map[flowKey]recentFlow
	// recentOrder 按插入顺序记录墓碑 key，用于淘汰最旧的一条；可能含已删除的 key（惰性）。
	recentOrder []flowKey
	// fallback 是主表满时记录流与槽位亲和的小容量旁路表。
	//
	// 表满时新流不插主映射。旁路表记录的流沿用第一次的槽；槽失效时才重选。
	// 旁路表溢出后无法记住所有流，本会话未知流固定哈希，死槽恢复前可能丢包。
	// 淘汰已记录条目会让对应流失去亲和。
	fallback map[flowKey]flowEntry
	// 溢出后无法辨认未登记的旧流；本会话后续未知流须持续使用稳定哈希。
	fallbackOverflowed bool
	// 溢出固定哈希选中非存活槽的次数，不等于实际丢包数；受 mu 保护。
	overflowDeadSelections uint64
	// clock 是时间源，测试注入用；nil 表示 time.Now。
	clock func() time.Time
}

// maxFallback 是旁路表的容量。表满本身是极端场景，这里只兜住几百条活跃的长尾流；
// 超出的流不记；溢出后未知流用确定性哈希，负载或存活集合变化不参与选槽。
const maxFallback = 1024

// maxFlows 是流表容量上限。每条 entry 约 24 字节，三万条约 1MB 级（含 map 开销），
// 对两端都不算负担。上限不是"永不淘汰"的借口：RST 和空闲期回收才是控制占用的
// 手段，这个数只是兜底。
const maxFlows = 30000

// flowKey 是规范化的流标识，可比较。长度随协议类型不同（9~13 字节），用 len 区分。
type flowKey struct {
	b   [13]byte
	len uint8
}

func NewFlowTable() *FlowTable {
	return &FlowTable{
		m:      make(map[flowKey]flowEntry, 256),
		recent: make(map[flowKey]recentFlow, 256),
		max:    maxFlows,
	}
}

func (t *FlowTable) now() time.Time {
	if t.clock != nil {
		return t.clock()
	}
	return time.Now()
}

// Slot 是兼容入口：等价于不带队列快照的 SlotWithPressure。
//
// 已有映射直接复用；新流在活槽中按承载流数分配（没有 pending 字节快照可比）。
// alive 报告某槽此刻是否真的可用（成员连接在、能写），传 nil 视为全部可用。
//
// alive 不能省：把新流分给一个已经断开的空槽，那条流的下行会被 flushBatch 整批丢掉
// （槽空即丢，这是为了让数据不倒进心跳通道），表现为「部分新连接完全不通」，而且流表
// 不会自我纠正 —— 只要那条流还在，它就一直往死槽发。
func (t *FlowTable) Slot(payload []byte, n int, alive func(int) bool) int {
	return t.SlotWithPressure(payload, n, alive, nil)
}

// SlotWithPressure 只在给新流分配槽位时参考 pending 字节快照；已建立的映射永远
// 保持原槽，避免内层 TCP 乱序。
func (t *FlowTable) SlotWithPressure(payload []byte, n int, alive func(int) bool, pendingBytes []int64) int {
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
		t.tcpCount, t.udpCount = 0, 0
		for _, e := range t.m {
			if e.slot > 0 && e.slot < n {
				t.counts[e.slot]++
			}
			t.addCounts(e.proto, 1)
		}
	} else if len(t.counts) < n {
		// 零值或手工构造的表可能没走过上面的重建，补齐长度即可，计数全零也是对的。
		t.counts = growCounts(t.counts, n)
	}

	if now.UnixNano()-t.lastSweep > int64(sweepInterval) {
		t.lastSweep = now.UnixNano()
		t.sweepLockedExcept(now, key)
	}
	if e, ok := t.m[key]; ok {
		// RST 立即结束连接：原槽仍存活时复用，随后删掉
		// 映射。FIN 仅关闭一个方向，仍保留亲和，直到既有空闲期限回收。
		if proto == 6 && flags&tcpFlagRST != 0 {
			delete(t.m, key)
			delete(t.fallback, key)
			t.counts[e.slot]--
			t.addCounts(e.proto, -1)
			t.reclaimed++
			if alive != nil && !alive(e.slot) {
				return hashToAlive(payload, n, alive)
			}
			return e.slot
		}
		// 映射指向的槽可能已经死了（成员断开还没补上）。原来这里直接返回它，那条流
		// 就持续往死槽发、被 flushBatch 整批丢掉，直到那个槽补拨回来 —— 实测能黑到
		// 几十秒，用户看到的是「显示已连接、延迟在跳，但这条流就是不动」。
		//
		// 迁移一次：只迁一次（迁完映射立刻更新），内层 TCP 的乱序只发生在这一刻，
		// 比持续黑洞好得多。这也是本表唯一允许改映射的情形。
		if alive == nil || alive(e.slot) {
			e.lastSeen = now.UnixNano()
			if proto == 6 {
				if flags&tcpFlagFIN != 0 {
					e.finAt = now.UnixNano()
				} else {
					e.finAt = 0
				}
			}
			t.m[key] = e
			return e.slot
		}
		delete(t.m, key)
		t.counts[e.slot]--
		t.addCounts(e.proto, -1)
		// 落到下面重新选槽
	}
	// 刚被空闲清扫删掉的流又回来了：恢复原槽，避免这一次换槽乱序。
	if r, ok := t.recent[key]; ok {
		if now.UnixNano()-r.deletedAt <= int64(recentTTL) &&
			// 用 n 而不是 len(t.counts)：counts 只增不减，成员数变少后它仍比 n 长，
			// 按它判会把流恢复到一个已经不存在的槽上。
			r.slot > 0 && r.slot < n &&
			(alive == nil || alive(r.slot)) {
			if proto == 6 && flags&tcpFlagRST != 0 {
				delete(t.recent, key)
				return r.slot
			}
			// 允许比上限多一条：这一条是恢复的旧流，不是新流；后续新流仍走回退，
			// 等清扫腾出空间。宁可短暂超限，也不让内层 TCP 在墓碑窗口内换槽。
			if len(t.m) <= t.max {
				if t.m == nil {
					t.m = make(map[flowKey]flowEntry, 1)
				}
				t.m[key] = flowEntry{slot: r.slot, lastSeen: now.UnixNano(), proto: r.proto}
				t.counts[r.slot]++
				t.addCounts(r.proto, 1)
				delete(t.recent, key)
				return r.slot
			}
			// 主表仍满时保留原槽，但**不续期**：续期会让这条墓碑永远不过期，既挤占
			// 墓碑容量，又可能被按插入顺序淘汰后让活跃流重新选槽（内层 TCP 乱序）。
			// 有空位时顺手记进旁路表：墓碑过期后仍有映射，避免这条流再换一次槽。
			if len(t.fallback) < maxFallback {
				if t.fallback == nil {
					t.fallback = make(map[flowKey]flowEntry, 64)
				}
				t.fallback[key] = flowEntry{slot: r.slot, lastSeen: now.UnixNano(), proto: r.proto}
			}
			t.recent[key] = r
			return r.slot
		}
		delete(t.recent, key)
	}
	if len(t.m) >= t.max {
		// 新流不插入主表；已登记流保留亲和，未知流在旁路溢出前优先选活槽。
		t.fallbacks++
		e, ok := t.fallback[key]
		s := e.slot
		if ok && s > 0 && s < n && (alive == nil || alive(s)) {
			// 记住的槽仍然有效：沿用，避免成员闪断期间换槽。
			if proto == 6 && flags&tcpFlagRST != 0 {
				delete(t.fallback, key)
			} else {
				e.lastSeen = now.UnixNano()
				if proto == 6 {
					if flags&tcpFlagFIN != 0 {
						e.finAt = now.UnixNano()
					} else {
						e.finAt = 0
					}
				}
				t.fallback[key] = e
			}
			return s
		}
		if ok {
			// 记过的槽已经不可用（成员断开或槽数变少）：丢掉旧值重新选，
			// 绝不能把包发回一个不存在/已断开的槽。
			delete(t.fallback, key)
		}
		trackFallback := len(t.fallback) < maxFallback
		if !trackFallback {
			t.fallbackOverflowed = true
		}
		switch {
		case !trackFallback:
			// 旁路表满、没法登记：只能用稳定哈希。逐包按负载选会让同一未登记流换槽。
			// 哈希若落到死槽（全员失效）就如实计数，但不要把它登记成映射。
			s = hashToAlive(payload, n, alive)
			if alive != nil && !alive(s) {
				t.overflowDeadSelections++
			}
		case len(pendingBytes) == 0:
			s = hashToAlive(payload, n, alive)
		default:
			// 能登记就还能按负载选：首包选一次、记进旁路表，后续沿用不会乱序。
			s = leastPendingSlot(payload, n, alive, pendingBytes, t.counts)
		}
		// 旁路表满后用稳定哈希；按动态队列压力逐包选槽会让未登记流乱序。
		if trackFallback {
			if t.fallback == nil {
				t.fallback = make(map[flowKey]flowEntry, 64)
			}
			fe := flowEntry{slot: s, lastSeen: now.UnixNano(), proto: proto}
			if proto == 6 && flags&tcpFlagFIN != 0 {
				fe.finAt = now.UnixNano()
			}
			t.fallback[key] = fe
		}
		// RST 清掉旁路条目；FIN 后续报文仍用原槽，旁路空闲项统一清扫。
		if proto == 6 && flags&tcpFlagRST != 0 {
			delete(t.fallback, key)
		}
		return s
	}

	best := 0
	// 表刚满过又降下来的流：旁路表里已经记过它原来的槽，这里直接转正。
	// 不查的话它会被当作新流重新选槽 —— 过渡期换槽一次 = 内层 TCP 乱序一次。
	if e, ok := t.fallback[key]; ok {
		if e.slot > 0 && e.slot < n && (alive == nil || alive(e.slot)) {
			best = e.slot
		}
		delete(t.fallback, key)
	}
	if best == 0 && t.fallbackOverflowed {
		// 容量恢复时按活槽哈希转正：溢出期间没有留下映射，但也不该把它送进死槽。
		best = hashToAlive(payload, n, alive)
		if alive != nil && !alive(best) {
			// 一个活槽都没有：不要把这个「全员失效」的瞬态登记成映射。
			t.overflowDeadSelections++
			return best
		}
	}
	if best == 0 {
		for i := 1; i < n; i++ {
			if alive != nil && !alive(i) {
				continue
			}
			if best == 0 || slotHasLessLoad(i, best, pendingBytes, t.counts) {
				best = i
			}
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
	if proto == 6 && flags&tcpFlagRST != 0 {
		return best
	}
	t.m[key] = flowEntry{slot: best, lastSeen: now.UnixNano(), proto: proto}
	t.counts[best]++
	t.addCounts(proto, 1)
	return best
}

func slotHasLessLoad(a, b int, pendingBytes []int64, counts []int) bool {
	loadA, loadB := int64(0), int64(0)
	if a < len(pendingBytes) {
		loadA = pendingBytes[a]
	}
	if b < len(pendingBytes) {
		loadB = pendingBytes[b]
	}
	if loadA != loadB {
		return loadA < loadB
	}
	return counts[a] < counts[b]
}

func leastPendingSlot(payload []byte, n int, alive func(int) bool, pendingBytes []int64, counts []int) int {
	best := 0
	for i := 1; i < n; i++ {
		if alive != nil && !alive(i) {
			continue
		}
		if best == 0 || slotHasLessLoad(i, best, pendingBytes, counts) {
			best = i
		}
	}
	if best == 0 {
		return Slot(payload, n)
	}
	return best
}

// sweepLocked 回收所有超过空闲期的映射。由 lastSweep 节流，每个 Slot 调用都会
// 检查一次，但真正扫表最多每秒一次。
func (t *FlowTable) sweepLocked(now time.Time) {
	t.sweepLockedExcept(now, flowKey{})
}

// sweepLockedExcept 与 sweepLocked 相同，但跳过当前包对应的流。否则一条安静超过
// 空闲期的活跃流会在同一次 Slot 调用里被扫掉、又从墓碑恢复，Reclaimed 每 5 分钟
// 涨一次，监控上看起来像「回收一直在生效」。
// flowExpired 判断一条映射是否该回收：UDP 只能按空闲期；TCP 发过 FIN 的按短宽限期
// 回收，其余按空闲期。短宽限期是为了不让正常关闭的短连接把 30k 容量绑在 5 分钟窗口上。
func flowExpired(e flowEntry, now time.Time) bool {
	if e.proto == 17 {
		return now.Sub(time.Unix(0, e.lastSeen)) > udpFlowIdle
	}
	if e.finAt > 0 && now.Sub(time.Unix(0, e.finAt)) > finHalfCloseIdle {
		return true
	}
	return now.Sub(time.Unix(0, e.lastSeen)) > tcpFlowIdle
}

func (t *FlowTable) sweepLockedExcept(now time.Time, skip flowKey) {
	for k, e := range t.m {
		if k == skip {
			continue
		}
		if !flowExpired(e, now) {
			continue
		}
		delete(t.m, k)
		if e.slot > 0 && e.slot < len(t.counts) {
			t.counts[e.slot]--
		}
		t.addCounts(e.proto, -1)
		t.reclaimed++
		// 墓碑：下一包在近期回来还能恢复原槽，超过窗口就忘掉。
		t.addRecentLocked(k, recentFlow{slot: e.slot, deletedAt: now.UnixNano(), proto: e.proto})
	}
	for k, e := range t.fallback {
		if k == skip {
			continue
		}
		if !flowExpired(e, now) {
			continue
		}
		delete(t.fallback, k)
		t.addRecentLocked(k, recentFlow{slot: e.slot, deletedAt: now.UnixNano(), proto: e.proto})
	}
	for k, r := range t.recent {
		if now.UnixNano()-r.deletedAt > int64(recentTTL) {
			delete(t.recent, k)
		}
	}
}

// addRecentLocked 添加墓碑并按插入顺序淘汰最旧的一条。
//
// recentOrder 可能含已经恢复/过期的 key（惰性删除），淘汰时跳过它们。
func (t *FlowTable) addRecentLocked(k flowKey, r recentFlow) {
	if t.recent == nil {
		t.recent = make(map[flowKey]recentFlow, 64)
	}
	t.recent[k] = r
	t.recentOrder = append(t.recentOrder, k)
	for len(t.recent) >= recentMax && len(t.recentOrder) > 0 {
		old := t.recentOrder[0]
		t.recentOrder = t.recentOrder[1:]
		if _, ok := t.recent[old]; ok {
			delete(t.recent, old)
		}
	}
	// 大量恢复/过期后 order 会带着已删除的 key 变长，定期重建避免底层数组膨胀。
	if len(t.recentOrder) > recentMax*2 {
		rebuilt := make([]flowKey, 0, len(t.recent))
		for key := range t.recent {
			rebuilt = append(rebuilt, key)
		}
		t.recentOrder = rebuilt
	}
}

// addCounts 维护协议分布计数；只认协议号 6/17（分片包的 proto 也是 6/17，
// 会正常计入，不会落到 OtherCount）。
func (t *FlowTable) addCounts(proto uint8, delta int) {
	switch proto {
	case 6:
		t.tcpCount += delta
	case 17:
		t.udpCount += delta
	}
}

// FlowStats 是流表当前状态的快照，只给诊断输出用。
//
// 报告要求先量「占用率、满表回退次数、各槽活跃映射数」再判断 RST/空闲回收是否有效：
// 没有这三个数，谁也说不清长会话里的短连接到底有没有把表吃满。
type FlowStats struct {
	Size                   int    // 已建立的映射数
	Max                    int    // 容量上限
	Fallbacks              uint64 // 主表满时进入回退分支的次数，含旁路流重复包
	Reclaimed              uint64 // 主表因 RST 或空闲期回收的映射数
	TCPCount               int    // 映射里的 TCP 流数
	UDPCount               int    // 映射里的 UDP 流数
	OtherCount             int    // 其他协议（非 TCP/UDP）占用的条目数
	PerSlot                []int  // 各槽承载的流数，下标即槽号
	FallbackSize           int    // 旁路表当前条目数
	Overflowed             bool   // 本会话曾发生旁路溢出，清扫后仍保持
	OverflowDeadSelections uint64 // 溢出固定哈希选中非存活槽的次数，非丢包数
}

// Stats 返回流表快照。锁开销可忽略：调用方是 10 分钟一次的状态日志。
func (t *FlowTable) Stats() FlowStats {
	t.mu.Lock()
	defer t.mu.Unlock()
	// counts 只增不减，成员数变少后尾部会留一串全零槽位；按当前成员数裁掉，
	// 状态输出里的 per-slot 才和本会话真实槽数一致。
	perLen := len(t.counts)
	if t.lastN > 0 && t.lastN < perLen {
		perLen = t.lastN
	}
	per := make([]int, perLen)
	copy(per, t.counts[:perLen])
	return FlowStats{
		Size: len(t.m), Max: t.max, Fallbacks: t.fallbacks, Reclaimed: t.reclaimed,
		TCPCount: t.tcpCount, UDPCount: t.udpCount,
		OtherCount:   len(t.m) - t.tcpCount - t.udpCount,
		PerSlot:      per,
		FallbackSize: len(t.fallback), Overflowed: t.fallbackOverflowed,
		OverflowDeadSelections: t.overflowDeadSelections,
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
