package client

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"log/slog"
	"testing"
	"testing/synctest"
	"time"

	"github.com/antapp-cc/antapp-link/internal/proto"
)

func TestRunStatsIncludesFlowOverflowDiagnostics(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ft := proto.NewFlowTable()
		for i := 0; i < 31025; i++ {
			p := make([]byte, 40)
			p[0] = 0x45
			p[9] = 6
			binary.BigEndian.PutUint16(p[2:4], 40)
			copy(p[12:16], []byte{10, 0, 0, 2})
			copy(p[16:20], []byte{8, 8, 8, 8})
			binary.BigEndian.PutUint16(p[20:22], uint16(1000+i))
			binary.BigEndian.PutUint16(p[22:24], 443)
			p[32] = 0x50
			p[33] = 0x10
			ft.Slot(p, 4, nil)
		}
		var buf bytes.Buffer
		a := &App{log: slog.New(slog.NewJSONHandler(&buf, nil))}
		tun := &Tunnel{}
		tun.flow.Store(ft)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan struct{})
		go func() { defer close(done); a.watchRunStats(ctx, tun) }()
		time.Sleep(10*time.Minute + time.Millisecond)
		synctest.Wait()
		cancel()
		<-done
		var st map[string]any
		if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &st); err != nil {
			t.Fatalf("日志无效: %v %s", err, buf.Bytes())
		}
		if st["flow_overflowed"] != true || st["flow_fallback_size"] != float64(1024) || st["flow_overflow_dead_selections"] != float64(0) {
			t.Fatalf("客户端遗漏溢出诊断: %s", buf.Bytes())
		}
	})
}
