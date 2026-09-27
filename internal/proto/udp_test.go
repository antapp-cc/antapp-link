package proto

import (
	"bytes"
	"crypto/rand"
	"testing"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

func TestUDPSealOpenRoundTrip(t *testing.T) {
	key := testKey(t)
	send, err := NewCipher(key, 1)
	if err != nil {
		t.Fatal(err)
	}
	recv, err := NewCipher(key, 1)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 5; i++ {
		msg := []byte{byte(i), 0xAA, 0xBB}
		pkt := send.Seal(msg)
		if len(pkt) != NonceLen+len(msg)+TagLen {
			t.Fatalf("包长 = %d, want %d", len(pkt), NonceLen+len(msg)+TagLen)
		}
		got, err := recv.Open(pkt)
		if err != nil {
			t.Fatalf("第 %d 个包解开失败: %v", i, err)
		}
		if !bytes.Equal(got, msg) {
			t.Errorf("第 %d 个包内容 = %v, want %v", i, got, msg)
		}
	}
}

// 两个方向的密钥必须不同：同一个 nonce 空间被两方共用会直接毁掉 GCM 的安全性。
func TestUDPDirectionsAreIndependent(t *testing.T) {
	key := testKey(t)
	c2s, _ := NewCipher(key, 1) // 客户端发、服务端收
	s2c, _ := NewCipher(key, 2) // 服务端发、客户端收

	pkt := c2s.Seal([]byte("hello"))

	// 用另一个方向的解密器去开，AAD 不同，必须失败
	wrong, _ := NewCipher(key, 2)
	if _, err := wrong.Open(pkt); err == nil {
		t.Error("跨方向解密应该失败")
	}
	// 正确方向可以开
	right, _ := NewCipher(key, 1)
	if _, err := right.Open(pkt); err != nil {
		t.Errorf("同方向解密应该成功: %v", err)
	}
	_ = s2c
}

func TestUDPWrongKeyFails(t *testing.T) {
	send, _ := NewCipher(testKey(t), 1)
	recv, _ := NewCipher(testKey(t), 1)

	pkt := send.Seal([]byte("secret"))
	if _, err := recv.Open(pkt); err == nil {
		t.Error("密钥不同时必须解密失败")
	}
}

func TestUDPTamperedPacketFails(t *testing.T) {
	key := testKey(t)
	send, _ := NewCipher(key, 1)
	recv, _ := NewCipher(key, 1)

	pkt := send.Seal([]byte("authentic"))
	pkt[len(pkt)-1] ^= 0x01 // 翻一个 bit

	if _, err := recv.Open(pkt); err == nil {
		t.Error("被篡改的包必须被拒绝")
	}
}

func TestUDPReplayRejected(t *testing.T) {
	key := testKey(t)
	send, _ := NewCipher(key, 1)
	recv, _ := NewCipher(key, 1)

	pkt := send.Seal([]byte("once"))

	if _, err := recv.Open(pkt); err != nil {
		t.Fatalf("第一次应该成功: %v", err)
	}
	if _, err := recv.Open(pkt); err == nil {
		t.Error("重放同一个包必须被拒绝")
	}
}

// UDP 会乱序，窗口内的乱序包必须照收，只有重复包和太老的包才丢。
func TestUDPOutOfOrderWithinWindowAccepted(t *testing.T) {
	key := testKey(t)
	send, _ := NewCipher(key, 1)
	recv, _ := NewCipher(key, 1)

	pkts := make([][]byte, 4)
	for i := range pkts {
		pkts[i] = send.Seal([]byte{byte(i)})
	}

	// 按 0, 2, 1, 3 的顺序到达
	for _, idx := range []int{0, 2, 1, 3} {
		if _, err := recv.Open(pkts[idx]); err != nil {
			t.Errorf("乱序包 %d 应该被接受: %v", idx, err)
		}
	}
}

func TestUDPTooOldRejected(t *testing.T) {
	key := testKey(t)
	send, _ := NewCipher(key, 1)
	recv, _ := NewCipher(key, 1)

	old := send.Seal([]byte("old"))
	// 先推 100 个包，把窗口推过去
	for i := 0; i < 100; i++ {
		if _, err := recv.Open(send.Seal([]byte{byte(i)})); err != nil {
			t.Fatalf("第 %d 个包: %v", i, err)
		}
	}
	if _, err := recv.Open(old); err == nil {
		t.Error("落到窗口外的老包必须被拒绝")
	}
}

func TestUDPShortPacket(t *testing.T) {
	recv, _ := NewCipher(testKey(t), 1)
	if _, err := recv.Open([]byte{1, 2, 3}); err != ErrShortPacket {
		t.Errorf("短包应返回 ErrShortPacket，实际 %v", err)
	}
}

func TestUDPKeyLengthChecked(t *testing.T) {
	if _, err := NewCipher(make([]byte, 16), 1); err == nil {
		t.Error("16 字节密钥应该被拒绝（AES-256 要 32 字节）")
	}
}
