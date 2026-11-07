package server

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"

	"github.com/antapp-cc/antapp-link/internal/proto"
)

func TestFlowOverflowDiagnosticsReachStatusJSON(t *testing.T) {
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
		dead := proto.Slot(p, 4)
		ft.Slot(p, 4, func(k int) bool { return k != dead })
	}
	var st Status
	fillFlowStatus(&st, ft)
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"flow_overflowed":true`, `"flow_fallback_size":1024`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("服务端状态缺少 %s: %s", want, raw)
		}
	}
	var old Status
	if err := json.Unmarshal([]byte(`{"flow_size":2,"flow_max":30000}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.FlowOverflowed || old.FlowFallbackSize != 0 || old.FlowOverflowDeadSelections != 0 {
		t.Fatal("旧状态 JSON 的新增字段必须保持零值")
	}
}

func TestWriteFlowStatusKeepsOverflowVisibleWhenMainTableEmpty(t *testing.T) {
	var out bytes.Buffer
	writeFlowStatus(&out, Status{FlowMax: 30000, FlowOverflowed: true, FlowOverflowDeadSelections: 2})
	for _, want := range []string{"0/30000", "溢出锁存 true", "非存活槽 2 次", "非实际丢包数"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("空主表时丢失诊断 %q: %s", want, out.String())
		}
	}
}

func TestWriteFlowStatusCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name string
		st   Status
		want string
	}{
		{"inactive", Status{}, ""},
		{"old", Status{FlowSize: 2, FlowMax: 30000}, "2/30000"},
		{"fallback", Status{FlowFallbackSize: 3}, "流表旁路    : 3 条"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			writeFlowStatus(&out, tc.st)
			if tc.want == "" && out.Len() != 0 || tc.want != "" && !strings.Contains(out.String(), tc.want) {
				t.Fatalf("状态输出不兼容: %s", out.String())
			}
		})
	}
}
