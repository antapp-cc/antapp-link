package server

import (
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"

	"github.com/antapp-cc/antapp-link/internal/proto"
)

// 服务端流表统计要真的写进状态文件：status 命令读的就是这份 JSON。
func TestStatusFlowFieldsJSON(t *testing.T) {
	st := Status{
		FlowSize:     123,
		FlowMax:      30000,
		FlowFallback: 4,
		FlowReclaim:  56,
		FlowPerSlot:  []int{0, 1, 2, 3},
	}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"flow_size":123`,
		`"flow_max":30000`,
		`"flow_fallback":4`,
		`"flow_reclaim":56`,
		`"flow_per_slot":[0,1,2,3]`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("状态 JSON 缺少 %s：%s", want, raw)
		}
	}
}

// fillFlowStatus 是 reportStatus 真正用的填充函数，直接测它，避免只测 JSON 序列化。
func TestFillFlowStatus(t *testing.T) {
	ft := proto.NewFlowTable()
	for i := 0; i < 2; i++ {
		p := make([]byte, 24)
		p[0] = 0x45
		binary.BigEndian.PutUint16(p[2:4], 24)
		p[9] = 6
		copy(p[12:16], []byte{10, 0, 0, 2})
		copy(p[16:20], []byte{8, 8, 8, 8})
		binary.BigEndian.PutUint16(p[20:22], uint16(40000+i))
		binary.BigEndian.PutUint16(p[22:24], 443)
		if got := ft.Slot(p, 4, nil); got == 0 {
			t.Fatalf("新流不该落到控制槽")
		}
	}

	var st Status
	fillFlowStatus(&st, ft)
	if st.FlowSize != 2 || st.FlowMax != 30000 {
		t.Fatalf("流表规模不对：size=%d max=%d", st.FlowSize, st.FlowMax)
	}
	if len(st.FlowPerSlot) != 4 {
		t.Fatalf("每槽流数长度应为 4，实际 %d", len(st.FlowPerSlot))
	}
	if sum := st.FlowPerSlot[1] + st.FlowPerSlot[2] + st.FlowPerSlot[3]; sum != 2 {
		t.Fatalf("每槽流数总和应为 2，实际 %d", sum)
	}
}
