package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
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
	ForwardIdle     int `json:"forward_idle,omitempty"`
	// Goroutines 是写状态那一刻的 goroutine 数。长期盯它有没有单调上涨，
	// 比事后查 pprof 更容易发现「每次重连泄漏一点」这类问题。
	Goroutines int `json:"goroutines,omitempty"`
	// UDPActive 表示当前会话走的是 UDP 数据通道（mode=udp 且客户端已上报端口）。
	UDPActive bool `json:"udp_active,omitempty"`
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
