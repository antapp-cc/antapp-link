package client

import (
	"bytes"
	"fmt"
	"testing"
)

// 混淆密钥长 10 字节，往返测试必须跨过这个边界：读写两侧的异或索引一旦错位，
// 从第一个字节起就全解错（gzip 魔数都对不上）。这条就是「云端分流表永远读不回来」
// 的回归网 —— 该函数此前没有任何往返测试。
func TestSealCacheRoundTrip(t *testing.T) {
	var table bytes.Buffer
	for i := 0; i < 800; i++ {
		fmt.Fprintf(&table, "10.%d.%d.0/24\n", i/256, i%256)
	}

	cases := []struct {
		name string
		text []byte
	}{
		{"空内容", nil},
		{"单字节", []byte{0x1f}},
		{"比密钥短", bytes.Repeat([]byte{0xab}, 9)},
		{"与密钥等长", bytes.Repeat([]byte{0xcd}, 10)},
		{"比密钥长", bytes.Repeat([]byte{0xef}, 11)},
		{"网段表", table.Bytes()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := openSealedCache(sealCache(c.text))
			if err != nil {
				t.Fatalf("openSealedCache: %v", err)
			}
			if !bytes.Equal(got, c.text) {
				t.Errorf("往返后不一致：写入 %d 字节，读出 %d 字节", len(c.text), len(got))
			}
		})
	}
}
