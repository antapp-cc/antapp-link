package server

import (
	"encoding/json"
	"strings"
	"testing"
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
