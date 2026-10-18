package proto

import (
	"bytes"
	"testing"
)

// 伪造的高序号包不能推进重放窗口。
//
// 曾经的实现是「读序号 → accept 推进窗口 → GCM 解密」：攻击者发一个序号极大、
// 但密文是垃圾的包，就能把窗口推过去，之后所有合法包都被判成"太老"而丢弃。
// 现在改成先只检查、认证通过后才提交。
func TestForgedHighSeqDoesNotAdvanceWindow(t *testing.T) {
	send, err := NewCipher(bytes.Repeat([]byte{1}, 32), 1)
	if err != nil {
		t.Fatal(err)
	}
	recv, err := NewCipher(bytes.Repeat([]byte{1}, 32), 1)
	if err != nil {
		t.Fatal(err)
	}

	// 先正常收 3 个包
	for i := 0; i < 3; i++ {
		pkt := send.Seal([]byte{byte(i)})
		if _, err := recv.Open(pkt); err != nil {
			t.Fatalf("第 %d 个合法包应被接收: %v", i, err)
		}
	}

	// 伪造：序号写成极大值，但密文仍是上一个合法包的（认证必然失败）
	good := send.Seal([]byte("next"))
	forged := append([]byte(nil), good...)
	for i := 0; i < NonceLen; i++ {
		forged[i] = 0xff
	}
	if _, err := recv.Open(forged); err == nil {
		t.Fatal("伪造包不应该通过认证")
	}

	// 关键断言：合法包仍能被接收（窗口没被伪造包推走）
	if _, err := recv.Open(good); err != nil {
		t.Fatalf("伪造包把窗口推走了，合法包被拒: %v", err)
	}
}

// 乱序但在窗口内的包必须照收；重复包和太老的包丢弃。
func TestReplayWindowBoundaries(t *testing.T) {
	key := bytes.Repeat([]byte{2}, 32)
	send, _ := NewCipher(key, 1)
	recv, _ := NewCipher(key, 1)

	pkts := make([][]byte, 5)
	for i := range pkts {
		pkts[i] = send.Seal([]byte{byte(i)})
	}

	// 乱序收：3,1,4,2,0（覆盖 0..4 全部）
	for _, idx := range []int{3, 1, 4, 2, 0} {
		if _, err := recv.Open(pkts[idx]); err != nil {
			t.Fatalf("窗口内乱序包 %d 应被接收: %v", idx, err)
		}
	}
	// 重复包
	if _, err := recv.Open(pkts[2]); err == nil {
		t.Error("重复包应被拒绝")
	}
}
