package update

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ErrHandoffFailed 表示新版进程明确报告了启动失败（没拿到单实例互斥体、
// 工作目录或日志初始化失败等）。旧进程拿到它要恢复运行，而不是退出。
var ErrHandoffFailed = errors.New("新版进程报告启动失败")

// handoffFileName 是「旧版拉起新版」这次交接的状态文件。
//
// 它解决的是第 13 章第 6 项：Start() 返回 nil 只说明进程创建成功，新版完全可能
// 在初始化阶段就退出。旧版要有办法在超时内确认「新进程真的起来了」，否则它一退，
// 用户手上就既没有能跑的也没有能回滚的。
const handoffFileName = "handoff.json"

const (
	handoffLaunching = "launching"
	// loaded 只说明二进制加载了、update 包 init 执行到了；ready 才说明关键初始化和
	// 界面都起来了。旧进程只能凭 ready 退出：在真实案例里新版以 exit 17 死在 main
	// 中途，而 init 阶段写的状态让旧进程以为交接成功，结果更新完一个客户端都不剩。
	handoffLoaded = "loaded"
	handoffReady  = "ready"
	handoffFailed = "failed"
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

// MarkLoaded 由 update 包的 init 调用，记下「二进制真的加载并执行到这里」。
//
// 这个信息本身有价值：它能把「文件坏了/依赖初始化崩了」和「main 里某个初始化失败」
// 区分开。但它不是交接成功的凭据 —— 单实例互斥体、工作目录、日志、网络自愈都还在
// main 里，所以这里绝不写 ready。
func MarkLoaded(dir, invocationID string) error {
	h, err := ReadHandoff(dir)
	if err != nil {
		return err
	}
	if invocationID == "" || h.InvocationID != invocationID {
		return fmt.Errorf("交接 ID 不匹配（文件 %s，本次 %s）", h.InvocationID, invocationID)
	}
	// 状态只能往前走：重复初始化不能把 ready / failed 倒退回去。
	if h.Phase != handoffLaunching {
		return nil
	}
	h.Phase = handoffLoaded
	h.NewPID = os.Getpid()
	if err := WriteHandoff(dir, &h); err != nil {
		return err
	}
	recordLifecycle(LifecycleEvent{
		Event:      EventHandoffLoaded,
		Version:    h.NewVersion,
		Invocation: h.InvocationID,
		Detail:     "新版二进制已加载",
	})
	return nil
}

// MarkReady 由新版进程在关键初始化完成后调用，把「我已经接手」写回交接状态。
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
	// failed 是终态：已经报过失败又改口成 ready，会让等了两轮的旧进程判断错乱。
	if h.Phase == handoffFailed {
		return fmt.Errorf("%w：状态已是 failed（%s）", ErrHandoffFailed, h.Err)
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
	// 已经确认接手的交接不再改写成 failed：旧进程那边已经据此退出，覆盖只会让
	// 下一次启动读到互相矛盾的历史。
	if h.Phase == handoffReady {
		return nil
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

// WaitReady 在 ctx 截止前等新版写 ready。超时返回 ctx 的错误，调用方据此区分
// 「超时」与「状态文件读不到」；新版自报失败则返回 ErrHandoffFailed。
//
// 新版明确写 failed 时立刻返回：等到窗口尽头再报「没确认」会把真正的原因
// （拿不到互斥体等）埋掉，也白让旧进程等下去。
func WaitReady(ctx context.Context, dir, invocationID string, poll time.Duration) (Handoff, error) {
	if poll <= 0 {
		poll = 50 * time.Millisecond
	}
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		if h, err := ReadHandoff(dir); err == nil && h.InvocationID == invocationID {
			switch h.Phase {
			case handoffReady:
				if h.NewPID > 0 {
					return h, nil
				}
			case handoffFailed:
				if h.Err != "" {
					return h, fmt.Errorf("%w：%s", ErrHandoffFailed, h.Err)
				}
				return h, ErrHandoffFailed
			}
		}
		select {
		case <-ctx.Done():
			return Handoff{}, ctx.Err()
		case <-t.C:
		}
	}
}

// handoffEnvDir / handoffEnvID 是本次进程从旧版继承的交接身份，init 时记下来，
// main 里分两个阶段用它写 loaded 与 ready / failed。
var (
	handoffEnvDir string
	handoffEnvID  string
)

// HandoffPending 报告本进程是被旧版拉起来接手的，且还没走到确认点。
func HandoffPending() bool { return handoffEnvID != "" }

// MarkStartupReady 由新版在「单实例互斥体、工作目录、日志、网络自愈都完成，
// 且图标、窗口、托盘也建起来」之后调用；现在这个时点在 client.RunUI 里。
// 这是旧进程唯一会据以退出的信号，任何更早的时点都等于重演问题 1：
// 界面起不来的新版被当成更新成功，旧进程退出后用户手上什么都没有。
func MarkStartupReady() error {
	if handoffEnvID == "" {
		return nil
	}
	return MarkReady(handoffEnvDir, handoffEnvID)
}

// FailStartup 把「新版没能接手」写回交接状态，让旧进程恢复而不是退出。
// 没有待确认的交接时是 no-op，普通双击启动碰不到任何状态文件。
func FailStartup(cause error) {
	if handoffEnvID == "" {
		return
	}
	if cause == nil {
		cause = errors.New("启动未完成")
	}
	_ = FailHandoff(handoffEnvDir, handoffEnvID, cause)
}

// 单实例互斥体在旧进程手里，而新版要拿到它才写 ready。旧进程在等待期间必须
// 让出这个锁，否则两边互等到各自超时 —— 这是把 ready 挪到拿到互斥体之后最危险的
// 副作用，所以让出和收回都做成显式钩子，由 cmd/antapp-link 提供实现。
var (
	relinquishInstance = func() {}
	reclaimInstance    = func() bool { return true }
)

// SetInstanceHandoffHooks 由 cmd/antapp-link 在拿到互斥体后注册。
// relinquish 把锁让出去；reclaim 在交接失败时尝试收回，返回是否还握着锁。
func SetInstanceHandoffHooks(relinquish func(), reclaim func() bool) {
	if relinquish != nil {
		relinquishInstance = relinquish
	}
	if reclaim != nil {
		reclaimInstance = reclaim
	}
}

// consumeHandoffEnv 是新版进程的自动确认点：Go 运行时执行到这里时，二进制已经
// 成功加载、依赖包也初始化完毕 —— 比 Start() 返回 nil 强，但仍不等于「界面可用」。
// 所以这里只写 loaded，ready 留给界面建好之后的 MarkStartupReady。
func consumeHandoffEnv() {
	dir := os.Getenv(handoffDirEnv)
	id := os.Getenv(handoffInvocationEnv)
	if dir == "" || id == "" {
		return
	}
	handoffEnvDir, handoffEnvID = dir, id
	_ = MarkLoaded(dir, id)
}

func init() { consumeHandoffEnv() }
