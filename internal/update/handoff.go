package update

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// handoffFileName 是「旧版拉起新版」这次交接的状态文件。
//
// 它解决的是第 13 章第 6 项：Start() 返回 nil 只说明进程创建成功，新版完全可能
// 在初始化阶段就退出。旧版要有办法在超时内确认「新进程真的起来了」，否则它一退，
// 用户手上就既没有能跑的也没有能回滚的。
const handoffFileName = "handoff.json"

const (
	handoffLaunching = "launching"
	handoffReady     = "ready"
	handoffFailed    = "failed"
)

// 交接环境变量由旧版在拉起新版时注入，新版在 update 包初始化阶段据此写 ready。
// 只传目录和一次性 ID，不传命令/路径，避免变成通用的「让新进程干点什么」的口子。
const (
	handoffDirEnv        = "ANTAPP_UPDATE_HANDOFF_DIR"
	handoffInvocationEnv = "ANTAPP_UPDATE_HANDOFF_ID"
)

// Handoff 一次进程交接的状态。InvocationID 是判据，时间戳只是给人看的辅助信息。
type Handoff struct {
	InvocationID string     `json:"invocation_id"`
	Phase        string     `json:"phase"`
	OldPID       int        `json:"old_pid"`
	NewPID       int        `json:"new_pid,omitempty"`
	OldVersion   string     `json:"old_version,omitempty"`
	NewVersion   string     `json:"new_version,omitempty"`
	StartedAt    time.Time  `json:"started_at"`
	Deadline     time.Time  `json:"deadline"`
	ReadyAt      *time.Time `json:"ready_at,omitempty"`
	Err          string     `json:"err,omitempty"`
}

func handoffPath(dir string) string { return filepath.Join(dir, handoffFileName) }

// newInvocationID 给每次交接一个随机身份。两次重启可能落在同一秒，靠时间戳认亲
// 会把上一次的 ready 当成这一次的。
func newInvocationID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// NewHandoff 写下一份交接状态，返回给调用方等待时用。
func NewHandoff(dir, oldVersion, newVersion string, window time.Duration) (*Handoff, error) {
	now := time.Now()
	h := &Handoff{
		InvocationID: newInvocationID(),
		Phase:        handoffLaunching,
		OldPID:       os.Getpid(),
		OldVersion:   oldVersion,
		NewVersion:   newVersion,
		StartedAt:    now,
		Deadline:     now.Add(window),
	}
	if err := WriteHandoff(dir, h); err != nil {
		return nil, err
	}
	return h, nil
}

// WriteHandoff 先写临时文件再改名，读的一方不会看到半个 JSON。
func WriteHandoff(dir string, h *Handoff) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(h)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".handoff-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, handoffPath(dir)); err != nil {
		return err
	}
	return nil
}

func ReadHandoff(dir string) (Handoff, error) {
	raw, err := os.ReadFile(handoffPath(dir))
	if err != nil {
		return Handoff{}, err
	}
	var h Handoff
	if err := json.Unmarshal(raw, &h); err != nil {
		return Handoff{}, err
	}
	return h, nil
}

// MarkReady 由新版进程调用，把「我已经启动」写回交接状态。
//
// InvocationID 不匹配就拒绝：那说明这份状态属于另一次交接，覆盖它会骗过正在等待的
// 旧版（两个版本同时认为自己被接管）。
func MarkReady(dir, invocationID string) error {
	h, err := ReadHandoff(dir)
	if err != nil {
		return err
	}
	if invocationID == "" || h.InvocationID != invocationID {
		return fmt.Errorf("交接 ID 不匹配（文件 %s，本次 %s）", h.InvocationID, invocationID)
	}
	now := time.Now()
	h.Phase = handoffReady
	h.NewPID = os.Getpid()
	h.ReadyAt = &now
	h.Err = ""
	if err := WriteHandoff(dir, &h); err != nil {
		return err
	}
	recordLifecycle(LifecycleEvent{
		Event:      EventUpdateReady,
		Version:    h.NewVersion,
		Invocation: h.InvocationID,
		Detail:     "新版已启动并确认接手",
	})
	return nil
}

// FailHandoff 记下交接失败的原因。错误原文保留，别让「更新失败」吞掉系统报错。
func FailHandoff(dir, invocationID string, cause error) error {
	h, err := ReadHandoff(dir)
	if err != nil {
		return err
	}
	if h.InvocationID != invocationID {
		return fmt.Errorf("交接 ID 不匹配（文件 %s，本次 %s）", h.InvocationID, invocationID)
	}
	h.Phase = handoffFailed
	if cause != nil {
		h.Err = cause.Error()
	}
	if err := WriteHandoff(dir, &h); err != nil {
		return err
	}
	recordLifecycle(LifecycleEvent{
		Event:      EventUpdateFailed,
		Version:    h.NewVersion,
		Invocation: h.InvocationID,
		Detail:     "交接失败",
		Err:        h.Err,
	})
	return nil
}

// WaitReady 在 ctx 截止前等新版写 ready。返回的错误就是 ctx 的错误，
// 调用方据此区分「超时」与「状态文件读不到」。
func WaitReady(ctx context.Context, dir, invocationID string, poll time.Duration) (Handoff, error) {
	if poll <= 0 {
		poll = 50 * time.Millisecond
	}
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		if h, err := ReadHandoff(dir); err == nil &&
			h.InvocationID == invocationID && h.Phase == handoffReady && h.NewPID > 0 {
			return h, nil
		}
		select {
		case <-ctx.Done():
			return Handoff{}, ctx.Err()
		case <-t.C:
		}
	}
}

// consumeHandoffEnv 是新版进程的自动确认点：Go 运行时执行到这里时，二进制已经
// 成功加载、依赖包也初始化完毕 —— 比 Start() 返回 nil 强，但仍不等于「数据面已就绪」。
// 完整的「初始化完成」信号要等调用方接入（见报告）。
func consumeHandoffEnv() {
	dir := os.Getenv(handoffDirEnv)
	id := os.Getenv(handoffInvocationEnv)
	if dir == "" || id == "" {
		return
	}
	_ = MarkReady(dir, id)
}

func init() { consumeHandoffEnv() }
