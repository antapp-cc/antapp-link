package client

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// rotatingLog 是够用的按大小轮转日志。只留一份备份：排查现场通常只看最近那次，
// 多留几份只是白占磁盘。
type rotatingLog struct {
	path    string
	maxSize int64

	mu   sync.Mutex
	file *os.File
	size int64
}

func OpenLog(path string, maxSize int64) (*rotatingLog, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	l := &rotatingLog{path: path, maxSize: maxSize}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *rotatingLog) open() error {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	l.file = f
	l.size = info.Size()
	return nil
}

func (l *rotatingLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return 0, os.ErrClosed
	}
	if l.size+int64(len(p)) > l.maxSize {
		if err := l.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := l.file.Write(p)
	l.size += int64(n)
	return n, err
}

func (l *rotatingLog) rotate() error {
	if err := l.file.Close(); err != nil {
		return err
	}
	l.file = nil
	if err := os.Rename(l.path, l.path+".1"); err != nil && !os.IsNotExist(err) {
		return err
	}
	l.size = 0
	return l.open()
}

// SwitchFile 把日志切到新文件并清空：每次连接会话的日志从零开始，
// 打开文件看到的就是本次连接的最新内容（旧会话不保留）。
func (l *rotatingLog) SwitchFile(path string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		_ = l.file.Close()
		l.file = nil
	}
	l.path = path
	// 新会话覆盖写
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	l.file = f
	l.size = 0
	return nil
}

func (l *rotatingLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}

func (l *rotatingLog) Path() string { return l.path }

// LogBuffer 保留最近若干行日志供界面显示。
//
// 界面不去 tail 日志文件：文件是按大小轮转的，读它还要处理并发和轮转；
// 而在写日志时顺手留一份几乎不花成本。
type LogBuffer struct {
	mu    sync.Mutex
	lines []string
	max   int
	// seq 是累计写入过的行数，只增不减。界面靠它算出「上次看到哪了」，
	// 从而只追加新行 —— 整体 SetText 会让只读框变成全选，能不用就不用。
	seq uint64
}

func NewLogBuffer(max int) *LogBuffer {
	if max <= 0 {
		max = 200
	}
	return &LogBuffer{max: max}
}

func (b *LogBuffer) addLine(line string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lines = append(b.lines, line)
	b.seq++
	if len(b.lines) > b.max {
		keep := make([]string, b.max)
		copy(keep, b.lines[len(b.lines)-b.max:])
		b.lines = keep
	}
}

// Since 返回序号 >= seq 的那些行，以及新的序号。
//
// 界面用它做增量追加。seq 太旧（那些行已经被轮转丢掉）时从现有最早一行开始，
// 调用方拿到的是「现在缓冲区里的全部内容」。
func (b *LogBuffer) Since(seq uint64) ([]string, uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()

	start := b.seq - uint64(len(b.lines)) // 缓冲区里第一行的序号
	if seq < start {
		seq = start
	}
	out := make([]string, 0, b.seq-seq)
	if idx := int(seq - start); idx < len(b.lines) {
		out = append(out, b.lines[idx:]...)
	}
	return out, b.seq
}

// Clear 清空界面缓冲：新连接会话的界面日志同样从零开始。
func (b *LogBuffer) Clear() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lines = nil
}

// Tail 返回最后 n 行。n<=0 表示全部。
func (b *LogBuffer) Tail(n int) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n <= 0 || n > len(b.lines) {
		n = len(b.lines)
	}
	out := make([]string, n)
	copy(out, b.lines[len(b.lines)-n:])
	return out
}

// teeHandler 让同一份日志有两个去处：文件里保留结构化原文（排查时字段好搜），
// 界面上给一行人类读得懂的短格式（时间 + 级别 + 消息 + 字段）。
//
// 直接在界面里显示 slog 的原文也行，但那样满屏都是 time=/level=/msg=，
// 跟用户已经习惯的 OpenVPN 客户端日志观感差得远。
type teeHandler struct {
	file slog.Handler
	buf  *LogBuffer
}

func (h *teeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.file.Enabled(ctx, level)
}

func (h *teeHandler) Handle(ctx context.Context, r slog.Record) error {
	if err := h.file.Handle(ctx, r); err != nil {
		return err
	}

	var b strings.Builder
	b.WriteString(r.Time.Format("01-02 15:04:05"))
	b.WriteString("  ")
	b.WriteString(levelTag(r.Level))
	b.WriteString("  ")
	b.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		b.WriteString("  ")
		b.WriteString(a.Key)
		b.WriteString("=")
		b.WriteString(a.Value.String())
		return true
	})
	h.buf.addLine(b.String())
	return nil
}

// WithAttrs / WithGroup 在本项目里用不到（都是直接 slog.Info/Warn/Error），
// 但 slog.Handler 要求实现，这里保持语义正确即可。
func (h *teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &teeHandler{file: h.file.WithAttrs(attrs), buf: h.buf}
}

func (h *teeHandler) WithGroup(name string) slog.Handler {
	return &teeHandler{file: h.file.WithGroup(name), buf: h.buf}
}

func levelTag(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "错误"
	case l >= slog.LevelWarn:
		return "警告"
	default:
		return "信息"
	}
}

// sessionLogger = 文件日志 + 界面缓冲：每次连接会话切换文件并清空。
type sessionLogger struct {
	rot *rotatingLog
	buf *LogBuffer
}

// activeSession 客户端单实例，包内单例安全。NewFileLogger 时赋值。
var activeSession *sessionLogger

var rootDirForLogs string

// SwitchLogSession 把日志切到与配置文件同名的会话文件（pinode.antapp → pinode.log），
// 文件与界面缓冲同时清空——新连接会话的日志从零开始，看到的永远是最新的。
// 失败安静忽略：日志切换失败不该挡住连接。
func SwitchLogSession(source string) {
	if activeSession == nil {
		return
	}
	name := strings.TrimSuffix(source, ".antapp")
	if name == "" {
		name = "client"
	}
	_ = activeSession.rot.SwitchFile(filepath.Join(LogsDir(rootDirForLogs), name+".log"))
	// client.log 只装启动瞬间的过渡日志（自愈/更新检查等，发生在选定配置之前），
	// 会话已切走，它没有保留价值——删掉，别在 logs 目录里留残渣
	_ = os.Remove(filepath.Join(LogsDir(rootDirForLogs), "client.log"))
	activeSession.buf.Clear()
}

// NewFileLogger 建一个同时写文件与界面缓冲的 logger。
func NewFileLogger(root string) (*slog.Logger, *LogBuffer, func(), error) {
	rootDirForLogs = root
	rot, err := OpenLog(filepath.Join(LogsDir(root), "client.log"), 2<<20)
	if err != nil {
		return nil, nil, nil, err
	}
	buf := NewLogBuffer(500)
	activeSession = &sessionLogger{rot: rot, buf: buf}
	fileHandler := slog.NewTextHandler(rot, &slog.HandlerOptions{Level: slog.LevelInfo})
	return slog.New(&teeHandler{file: fileHandler, buf: buf}), buf,
		func() { _ = rot.Close() }, nil
}
