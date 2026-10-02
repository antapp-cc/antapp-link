package client

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"sync"
	"time"
)

// OrderTracer 把每个进出隧道的 IP 包记一行（方向、槽位、五元组、TCP 序号）。
//
// 存在的理由：抓包工具（pktmon / ETW）的时间戳会把一批包刷写成同一个瞬间，
// 判不出「同一内层流是否始终走同一个槽」；槽位是包实际走的连接，不受它影响。
// 默认关闭，nil 时所有方法都是空操作。
type OrderTracer struct {
	mu     sync.Mutex
	w      *bufio.Writer
	f      *os.File
	base   time.Time
	done   chan struct{}
	closed bool
}

// NewOrderTracer 打开（或截断）追踪文件并写表头。
func NewOrderTracer(path string) (*OrderTracer, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("创建顺序追踪文件: %w", err)
	}
	t := &OrderTracer{
		w:    bufio.NewWriterSize(f, 1<<16),
		f:    f,
		base: time.Now(),
		done: make(chan struct{}),
	}
	fmt.Fprintln(t.w, "# ts_us dir slot src sport dst dport proto seq len")
	go t.flusher()
	return t, nil
}

// flusher 定期把缓冲刷到磁盘：数据面只写内存，不被磁盘 IO 拖慢；进程被强杀时
// 最多丢半个周期的记录。
func (t *OrderTracer) flusher() {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-t.done:
			return
		case <-ticker.C:
			t.mu.Lock()
			_ = t.w.Flush()
			t.mu.Unlock()
		}
	}
}

// Record 记一个包。dir 取 "tx"（送进隧道）或 "rx"（从隧道收到）。
func (t *OrderTracer) Record(dir string, slot int, pkt []byte) {
	if t == nil {
		return
	}
	flow, ok := parseFlow(pkt)
	if !ok {
		return
	}
	us := time.Since(t.base).Microseconds()
	t.mu.Lock()
	fmt.Fprintf(t.w, "%d %s %d %s %d %s %d %d %d %d\n",
		us, dir, slot, flow.src, flow.sport, flow.dst, flow.dport, flow.proto, flow.seq, len(pkt))
	t.mu.Unlock()
}

// Close 刷盘并关闭文件。可重复调用。
func (t *OrderTracer) Close() error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	t.mu.Unlock()

	close(t.done)
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.w.Flush(); err != nil {
		return err
	}
	return t.f.Close()
}

type flowKey struct {
	src, dst            string
	sport, dport, proto int
	seq                 uint32
}

// parseFlow 从 IPv4 包里取五元组；TCP 另外取序号。非 IPv4 或头部截断则报 false。
func parseFlow(pkt []byte) (flowKey, bool) {
	var f flowKey
	if len(pkt) < 20 || pkt[0]>>4 != 4 {
		return f, false
	}
	ihl := int(pkt[0]&0x0F) * 4
	if ihl < 20 || len(pkt) < ihl {
		return f, false
	}
	f.proto = int(pkt[9])
	f.src = net.IP(pkt[12:16]).String()
	f.dst = net.IP(pkt[16:20]).String()
	switch {
	case f.proto == 6 && len(pkt) >= ihl+20: // TCP
		f.sport = int(binary.BigEndian.Uint16(pkt[ihl:]))
		f.dport = int(binary.BigEndian.Uint16(pkt[ihl+2:]))
		f.seq = binary.BigEndian.Uint32(pkt[ihl+4:])
	case f.proto == 17 && len(pkt) >= ihl+8: // UDP
		f.sport = int(binary.BigEndian.Uint16(pkt[ihl:]))
		f.dport = int(binary.BigEndian.Uint16(pkt[ihl+2:]))
	}
	return f, true
}
