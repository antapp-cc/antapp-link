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
}

// SlotStatus 是一条连接的收发字节，按链路上的字节算（含帧头）。
// 空槽（成员连接断开待补）也会出现，字节数保持 0。
type SlotStatus struct {
	Slot    int    `json:"slot"`
	RxBytes uint64 `json:"rx_bytes"`
	TxBytes uint64 `json:"tx_bytes"`
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
