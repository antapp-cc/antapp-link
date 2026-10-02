package client

import (
	"context"
	"sync"
	"testing"
	"time"
)

type countingCloser struct {
	mu     sync.Mutex
	n      int
	closed chan struct{}
}

func newCountingCloser() *countingCloser {
	return &countingCloser{closed: make(chan struct{})}
}

func (c *countingCloser) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	if c.n == 1 {
		close(c.closed)
	}
	return nil
}

func (c *countingCloser) closes() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// 成员连接的读循环阻塞在 ReadFrame 上，ctx 取消叫不醒它——只能靠关连接把它放出来。
// 不关连接就是一个漏掉的 fd 加一个永不退出的 goroutine。
func TestCloseOnDoneClosesWhenContextEnds(t *testing.T) {
	closer := newCountingCloser()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := closeOnDone(ctx, closer)
	defer stop()

	select {
	case <-closer.closed:
		t.Fatal("ctx 还没结束就关掉了连接")
	default:
	}

	cancel()
	select {
	case <-closer.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 结束后必须关闭连接，否则读循环永远卡在 ReadFrame 上")
	}
	if got := closer.closes(); got != 1 {
		t.Fatalf("关闭次数 %d，期望 1", got)
	}
}
