package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/antapp-cc/antapp-link/internal/proto"
)

// StatusPath 是 run 进程写给 status 子命令看的运行时状态。
const StatusPath = "/run/antapp-link/status.json"

type Status struct {
	Listen      string `json:"listen"`
	Device      string `json:"device"`
	ServerIP    string `json:"server_ip"`
	ClientIP    string `json:"client_ip"`
	Client      string `json:"client,omitempty"`
	ConnectedAt string `json:"connected_at,omitempty"`
	UpdatedAt   string `json:"updated_at"`
	Members     int    `json:"members,omitempty"`
	LiveMembers int    `json:"live_members,omitempty"`
	// 每槽收发字节 + 熔断计数：灰度多连接时看流量分摊，以及 JOIN 是否正被拒
	Slots        []SlotStatus `json:"slots,omitempty"`
	JoinFailures int          `json:"join_failures,omitempty"`
	// 流表（下行选槽）：占用、回退、回收和每槽流数。服务端也跑同一套回收，
	// 这几个数能看出下行流表有没有被短连接吃满。
	FlowSize     int    `json:"flow_size,omitempty"`
	FlowMax      int    `json:"flow_max,omitempty"`
	FlowFallback uint64 `json:"flow_fallback,omitempty"`
	FlowReclaim  uint64 `json:"flow_reclaim,omitempty"`
	FlowPerSlot  []int  `json:"flow_per_slot,omitempty"`
	// 端口转发监听结果：多出来的 JSON 字段不影响旧版 status 子命令解析
	ForwardReady bool   `json:"forward_ready"`
	ForwardError string `json:"forward_error,omitempty"`
	// ForwardKnown 区分「状态文件是旧版 daemon 写的」与「新版刚启动还没刷新」。
	// 前者读出来是零值，不能据此替 daemon 宣称转发没起来。
	ForwardKnown bool `json:"forward_known,omitempty"`
	// 转发连接计数。这批端口对公网开放，扫描器连上后既不发送也不关闭，
	// 会一直占着搬运协程（实测同时挂过 25~36 条）。把它们放进状态文件，
	// 异常增长在 status 里就能看见，不必事后数 ss 才知道出了什么事。
	ForwardActive   int `json:"forward_active,omitempty"`
	ForwardTotal    int `json:"forward_total,omitempty"`
	ForwardRejected int `json:"forward_rejected,omitempty"`
	ForwardSwept    int `json:"forward_swept,omitempty"`
	// Goroutines 是写状态那一刻的 goroutine 数。长期盯它有没有单调上涨，
	// 比事后查 pprof 更容易发现「每次重连泄漏一点」这类问题。
	Goroutines int `json:"goroutines,omitempty"`
	// UDPActive 表示当前会话走的是 UDP 数据通道（mode=udp 且客户端已上报端口）。
	UDPActive bool `json:"udp_active,omitempty"`
	// UDP 数据通道实际收发的链路字节（含认证开销）。UDP 模式下行不走槽位，
	// 没有 Slots 可看，这两个数用来确认数据面是不是真的在动。
	UDPRxBytes uint64 `json:"udp_rx_bytes,omitempty"`
	UDPTxBytes uint64 `json:"udp_tx_bytes,omitempty"`

	// PID / InstanceID / StartedAt 标明状态是「哪一次启动」写的。
	// 只看 UpdatedAt 的秒级时间戳，重启前后两个进程可能落在同一秒里，分不出来。
	PID        int    `json:"pid,omitempty"`
	InstanceID string `json:"instance_id,omitempty"`
	StartedAt  string `json:"started_at,omitempty"`

	// 外层 TLS 握手并发。被拒次数异常增长说明有人在用半开握手冲服务端。
	// 只统计还在握手 / 读 HELLO 的连接：进入会话后名额已经还回去，不再计入，
	// 否则这个数字会把长时间在线的会话也算成「正在握手」。
	HandshakeInFlight int    `json:"handshake_in_flight,omitempty"`
	HandshakePeak     int    `json:"handshake_peak,omitempty"`
	HandshakeRefused  uint64 `json:"handshake_refused,omitempty"`

	// 文件描述符预算。LimitNPROC=infinity 是进程数上限，不能拿它替代 fd 预算：
	// 端口转发、TLS 握手、/proc 读取都吃 fd，耗尽后 accept 会持续 EMFILE。
	// FDKnown=false 表示这次没读到，而不是「用了 0 个」。
	FDUsed  int   `json:"fd_used"`
	FDLimit int64 `json:"fd_limit,omitempty"`
	FDKnown bool  `json:"fd_known,omitempty"`

	// TUN 丢包：ip -s link 的 dropped 列。txqueuelen=2000 只是待测缓冲，
	// 各层（TUN、槽位队列、转发器）的丢包要分开看才知道是谁在丢。
	// TUNDropsKnown=false 表示这次没读到，不代表丢包为 0。
	TUNRxDropped  uint64 `json:"tun_rx_dropped,omitempty"`
	TUNTxDropped  uint64 `json:"tun_tx_dropped,omitempty"`
	TUNDropsKnown bool   `json:"tun_drops_known,omitempty"`
}

// fillFlowStatus 把会话流表快照写进 Status。单独拆出来是为了能被测试直接覆盖：
// reportStatus 依赖整个 server，构造成本太高。
func fillFlowStatus(st *Status, ft *proto.FlowTable) {
	if ft == nil {
		return
	}
	fs := ft.Stats()
	st.FlowSize = fs.Size
	st.FlowMax = fs.Max
	st.FlowFallback = fs.Fallbacks
	st.FlowReclaim = fs.Reclaimed
	st.FlowPerSlot = fs.PerSlot
}

// reportStatus 每 5 秒刷新一次状态文件，超过这个阈值就说明写它的进程已经不在了。
// /run 是 tmpfs，systemctl stop 或崩溃后旧文件会一直留着，光看文件存在会长期误报「运行中」。
const statusStaleAfter = 20 * time.Second

// Fresh 报告状态文件是否还在被刷新，即写它的隧道进程是否还活着。
func (st Status) Fresh() bool {
	t, err := time.Parse(time.RFC3339, st.UpdatedAt)
	return err == nil && time.Since(t) <= statusStaleAfter
}

// SlotStatus 是一条连接的收发字节，按链路上的字节算（含帧头）。
// 空槽（成员连接断开待补）也会出现，字节数保持 0。
type SlotStatus struct {
	Slot    int    `json:"slot"`
	RxBytes uint64 `json:"rx_bytes"`
	TxBytes uint64 `json:"tx_bytes"`
	// Drops 是该槽写队列满、整批丢弃的次数。它的作用是给 slotQueueLen 定值提供依据：
	// 队列深度该按链路 BDP 算（跨境 200ms / 10Mbps 约 250KB），不是按写超时周期，
	// 而 8 批只有约 128KB —— 到底够不够要看真实丢包率，所以先让它可见。
	Drops uint64 `json:"drops"`
}

// WriteStatus 先写临时文件再改名，避免 status 读到半个 JSON。
func WriteStatus(st Status) error {
	st.UpdatedAt = time.Now().Format(time.RFC3339)
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(StatusPath), 0o755); err != nil {
		return err
	}
	tmp := StatusPath + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, StatusPath)
}

func ReadStatus() (Status, error) {
	raw, err := os.ReadFile(StatusPath)
	if err != nil {
		return Status{}, err
	}
	var st Status
	if err := json.Unmarshal(raw, &st); err != nil {
		return Status{}, err
	}
	return st, nil
}

// stateCommandBudget 是读状态用的外部命令预算。状态读取失败必须能被区分出来，
// 不能让「读不到」静静变成 0（那会显示成「一切健康」）。
const stateCommandBudget = 2 * time.Second

// runCommand 在预算内跑一条命令。超时/失败都返回 error，由调用方把 known 置 false；
// 空输出是合法的「真的是 0」，不能和失败混为一谈。
func runCommand(ctx context.Context, budget time.Duration, name string, args ...string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	out, err := exec.CommandContext(cctx, name, args...).Output()
	if cctx.Err() == context.DeadlineExceeded {
		return out, fmt.Errorf("%s 超时（预算 %v）", name, budget)
	}
	return out, err
}

// parseMaxOpenFiles 从 /proc/self/limits 的文本里取 soft 上限。
// "unlimited" 记为 -1；没有这一行说明格式不对，不假装解析成功。
func parseMaxOpenFiles(r io.Reader) (int64, bool) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		if !strings.HasPrefix(sc.Text(), "Max open files") {
			continue
		}
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 {
			return 0, false
		}
		soft := fields[3]
		if strings.EqualFold(soft, "unlimited") {
			return -1, true
		}
		n, err := strconv.ParseInt(soft, 10, 64)
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

// readProcFDUsage 数当前进程打开的 fd，并取 limits 里的 soft 上限。
//
// 目录本身在读取时也占一个 fd，所以结果可能比「稳定态」多 1；对容量预警没有影响。
func readProcFDUsage(procRoot string) (used int, limit int64, err error) {
	entries, err := os.ReadDir(filepath.Join(procRoot, "self", "fd"))
	if err != nil {
		return 0, 0, err
	}
	raw, err := os.ReadFile(filepath.Join(procRoot, "self", "limits"))
	if err != nil {
		return 0, 0, err
	}
	limit, ok := parseMaxOpenFiles(strings.NewReader(string(raw)))
	if !ok {
		return 0, 0, fmt.Errorf("%s 里没有 Max open files 行", filepath.Join(procRoot, "self", "limits"))
	}
	return len(entries), limit, nil
}

// runStateCommand 留成变量，测试才能用假输出验证解析，而不依赖机器上真有 ip 命令。
var runStateCommand = runCommand

// readTunDrops 读 TUN 网卡的丢包计数（ip -s link 的 dropped 列）。
func readTunDrops(ctx context.Context, device string) (rx, tx uint64, err error) {
	out, err := runStateCommand(ctx, stateCommandBudget, "ip", "-s", "link", "show", "dev", device)
	if err != nil {
		return 0, 0, fmt.Errorf("读 %s 的丢包计数: %w", device, err)
	}
	rx, tx, ok := parseIPLinkDrops(string(out))
	if !ok {
		return 0, 0, fmt.Errorf("%s 的 ip -s link 输出里没有 RX/TX 统计", device)
	}
	return rx, tx, nil
}

// parseIPLinkDrops 解析 ip -s link 的统计块：RX/TX 标题行的下一行，
// 第 4 个字段是 dropped。标题行和数字行缺一都算读不到。
func parseIPLinkDrops(out string) (rx, tx uint64, ok bool) {
	var (
		seenRX, seenTX bool
		want           byte
	)
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "RX:") {
			want = 'r'
			continue
		}
		if strings.HasPrefix(line, "TX:") {
			want = 't'
			continue
		}
		if want == 0 {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			want = 0
			continue
		}
		dropped, err := strconv.ParseUint(fields[3], 10, 64)
		if err != nil {
			want = 0
			continue
		}
		if want == 'r' {
			rx, seenRX = dropped, true
		} else {
			tx, seenTX = dropped, true
		}
		want = 0
	}
	return rx, tx, seenRX && seenTX
}
