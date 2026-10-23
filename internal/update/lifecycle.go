package update

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// 生命周期事件名。它们写进独立的 lifecycle.log，不受 client.log 按会话清空的影响。
const (
	EventProcessStart   = "process_start"
	EventUpdateStart    = "update_start"
	EventUpdateSwapped  = "swap_done"
	EventUpdateLaunched = "launch_ok"
	EventHandoffLoaded  = "handoff_loaded"
	EventUpdateReady    = "update_ready"
	EventUpdateFailed   = "update_failed"
	EventRollback       = "rollback"
	EventBackupRemoved  = "backup_removed"
	EventHandoffTimeout = "handoff_timeout"
	// 下面三个发生在 client/main 里，update 包只提供 LogLifecycle 入口。
	EventNetworkRestore = "network_restore"
	EventProcessExit    = "process_exit"
)

// defaultLifecycleMaxBytes 是生命周期日志的容量上限。轮转只留一份备份：
// 排障看的是最近一次升级，历史归档的价值低于磁盘空间。
const defaultLifecycleMaxBytes = 256 << 10

// LifecycleEvent 是生命周期日志的一行。
//
// Detail 给人看，Err 原样保存系统错误 —— 更新失败时「CreateProcess: %1 不是有效的
// Win32 应用程序」这种原文比任何翻译都值钱。
type LifecycleEvent struct {
	Time       time.Time `json:"time"`
	Event      string    `json:"event"`
	Version    string    `json:"version,omitempty"`
	PID        int       `json:"pid"`
	Invocation string    `json:"invocation,omitempty"`
	Detail     string    `json:"detail,omitempty"`
	Err        string    `json:"err,omitempty"`
}

type lifecycleLog struct {
	path    string
	maxSize int64
	now     func() time.Time
}

func newLifecycleLog(path string, maxSize int64) *lifecycleLog {
	if maxSize <= 0 {
		maxSize = defaultLifecycleMaxBytes
	}
	return &lifecycleLog{path: path, maxSize: maxSize, now: time.Now}
}

// Record 追加一行。超过上限就轮转一次：旧文件变成 .1，当前文件从空开始。
func (l *lifecycleLog) Record(e LifecycleEvent) error {
	if e.Time.IsZero() {
		e.Time = l.now()
	}
	if e.PID == 0 {
		e.PID = os.Getpid()
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')

	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return err
	}
	if st, err := os.Stat(l.path); err == nil && st.Size()+int64(len(raw)) > l.maxSize {
		// 覆盖上一份备份（Windows 上 os.Rename 也能覆盖已存在文件）
		_ = os.Rename(l.path, l.path+".1")
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(raw)
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	return cerr
}

// processInvocation 是本次进程的随机身份，写进每条生命周期事件。
// 升级前后两个进程的 PID 可能很快被复用，随机 ID 才是「同一次启动」的可靠判据。
var processInvocation = newInvocationID()

var lifecyclePathFn = defaultLifecyclePath

// defaultLifecyclePath 放在用户配置目录，而不是安装目录：客户端装在 Program Files
// 时普通用户对安装目录没有写权限，写不进去等于启动事件全丢。
func defaultLifecyclePath() string {
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		return filepath.Join(os.TempDir(), "AntAppLink", "lifecycle.log")
	}
	return filepath.Join(dir, "AntAppLink", "lifecycle.log")
}

// recordLifecycle 是 best-effort：日志写不进去绝不能影响更新本身。
func recordLifecycle(e LifecycleEvent) {
	if e.PID == 0 {
		e.PID = os.Getpid()
	}
	if e.Invocation == "" {
		e.Invocation = processInvocation
	}
	_ = newLifecycleLog(lifecyclePathFn(), defaultLifecycleMaxBytes).Record(e)
}

// LogLifecycle 给调用方记录更新流程之外的生命周期事件（启动、网络恢复、退出原因）。
// update 包内的升级事件已经自动记录；这三类只能由 client/main 在各自的入口调用。
func LogLifecycle(event, detail string, cause error) {
	e := LifecycleEvent{Event: event, Detail: detail}
	if cause != nil {
		e.Err = cause.Error()
	}
	recordLifecycle(e)
}
