package client

import (
	"log/slog"
	"os"
	"path/filepath"
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

// NewFileLogger 建一个写文件的 logger。放文件而不是 stderr：托盘程序没有控制台，
// 用户排查问题时需要一个能直接打开看的地方。
func NewFileLogger(dataDir string) (*slog.Logger, func(), error) {
	rot, err := OpenLog(filepath.Join(dataDir, "logs", "client.log"), 2<<20)
	if err != nil {
		return nil, nil, err
	}
	logger := slog.New(slog.NewTextHandler(rot, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return logger, func() { _ = rot.Close() }, nil
}
