package client

import (
	"io"
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
// 界面不去 tail 日志文件：文件按大小轮转，读它还要处理并发和轮转；
// 而在写日志时顺手留一份几乎不花成本。
type LogBuffer struct {
	mu      sync.Mutex
	lines   []string
	partial string
	max     int
}

func NewLogBuffer(max int) *LogBuffer {
	if max <= 0 {
		max = 200
	}
	return &LogBuffer{max: max}
}

func (b *LogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.partial += string(p)
	for {
		i := strings.IndexByte(b.partial, '\n')
		if i < 0 {
			break
		}
		b.lines = append(b.lines, b.partial[:i])
		b.partial = b.partial[i+1:]
	}
	if len(b.lines) > b.max {
		keep := make([]string, b.max)
		copy(keep, b.lines[len(b.lines)-b.max:])
		b.lines = keep
	}
	return len(p), nil
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

// NewFileLogger 建一个同时写文件与内存缓冲的 logger。
//
// 写文件是因为托盘程序没有控制台；留缓冲是因为界面要能实时显示，
// 而让界面去读一个正在轮转的文件既不安全也不及时。
func NewFileLogger(dataDir string) (*slog.Logger, *LogBuffer, func(), error) {
	rot, err := OpenLog(filepath.Join(dataDir, "logs", "client.log"), 2<<20)
	if err != nil {
		return nil, nil, nil, err
	}
	buf := NewLogBuffer(400)
	handler := slog.NewTextHandler(io.MultiWriter(rot, buf), &slog.HandlerOptions{Level: slog.LevelInfo})
	return slog.New(handler), buf, func() { _ = rot.Close() }, nil
}
