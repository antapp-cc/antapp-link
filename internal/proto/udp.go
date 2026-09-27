// Package proto 的 UDP 数据通道。
//
// 设计取舍：**控制通道走 TLS over TCP，数据包走 UDP**。
// 密钥不是自己协商的，而是从 TLS 会话里导出来的（RFC 5705 的
// ExportKeyingMaterial）—— 两端拿到的字节完全一致，认证仍由 TLS 证书完成。
// 这样既不用自己写密钥交换（那是安全关键代码），也不用引入 DTLS/QUIC 这类第三方库。
//
// 代价要说清楚：TCP 被完全封死时连握手都做不了，这套就起不来。
// 它解决的是「TCP 能连但被限速/干扰」，不是「TCP 完全不通」。
package proto

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	// UDPLabelC2S / UDPSLabelS2C 是导出密钥用的标签。
	// 两个方向各导一份，避免同一密钥在两个方向复用 nonce 空间。
	UDPLabelC2S = "antapp-link/udp/c2s"
	UDPLabelS2C = "antapp-link/udp/s2c"

	// nonceSize 是 AES-GCM 的 nonce 长度。
	nonceSize = 12

	// NonceLen 是 UDP 包头里明文携带的计数器长度（大端 8 字节）。
	NonceLen = 8

	// TagLen 是 GCM 认证标签长度，留出来是为了预分配缓冲。
	TagLen = 16

	// MaxUDPPacket 是单个 UDP 包的上限：MTU 1500 加帧头与标签仍绰绰有余。
	MaxUDPPacket = 1600
)

var (
	// ErrShortPacket 表示收到的包连 nonce 都不完整。
	ErrShortPacket = errors.New("proto: udp packet too short")
	// ErrReplay 表示这个包不是「比上次更新」，直接丢掉。
	ErrReplay = errors.New("proto: udp packet replayed or out of order")
)

// Cipher 是一个方向的 UDP 加解密器。
//
// UDP 会乱序，所以不能要求「严格递增」，只要求「没收到过」；这里用一个
// 64 位的滑动窗口记录最近见过的序号 —— 跟 IPsec 的做法一样，
// 窗口外的老包一律丢弃，防止重放。
type Cipher struct {
	aead  cipher.AEAD
	nonce [nonceSize]byte

	sendSeq uint64

	// 接收侧滑动窗口。hi 是见过的最大序号，bitmap 记录 hi 之前的 64 个序号。
	hi     uint64
	bitmap uint64
	seen   bool
}

// NewCipher 用 32 字节密钥建一个方向的加解密器。
func NewCipher(key []byte, aad byte) (*Cipher, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("proto: 需要 32 字节密钥，实际 %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	c := &Cipher{aead: aead}
	// AAD 里带一个方向字节，两个方向的密文不能互换使用
	c.nonce[nonceSize-1] = aad
	return c, nil
}

// Seal 把明文封成一个 UDP 包：8 字节序号 + 密文（含 16 字节标签）。
//
// 序号明文可见 —— 这跟 WireGuard 一样，只泄露「这是第几个包」，
// 不泄露内容，换来的是接收端不必先解密就能判重放。
func (c *Cipher) Seal(plaintext []byte) []byte {
	seq := c.sendSeq
	c.sendSeq++

	out := make([]byte, NonceLen, NonceLen+len(plaintext)+TagLen)
	binary.BigEndian.PutUint64(out, seq)

	c.setNonce(seq)
	return c.aead.Seal(out, c.nonce[:], plaintext, []byte{c.nonce[nonceSize-1]})
}

// Open 解开一个 UDP 包，返回明文。
func (c *Cipher) Open(pkt []byte) ([]byte, error) {
	if len(pkt) < NonceLen+TagLen {
		return nil, ErrShortPacket
	}
	seq := binary.BigEndian.Uint64(pkt[:NonceLen])
	if !c.accept(seq) {
		return nil, ErrReplay
	}

	c.setNonce(seq)
	plain, err := c.aead.Open(nil, c.nonce[:], pkt[NonceLen:], []byte{c.nonce[nonceSize-1]})
	if err != nil {
		return nil, fmt.Errorf("proto: udp 解密失败（密钥不符或包被篡改）: %w", err)
	}
	return plain, nil
}

func (c *Cipher) setNonce(seq uint64) {
	binary.BigEndian.PutUint64(c.nonce[:8], seq)
	// 最后 1 字节留给方向标记（构造时已写入），中间 3 字节保持 0
}

// accept 判断这个序号是否可以接收，可以就把它记进滑动窗口。
func (c *Cipher) accept(seq uint64) bool {
	if !c.seen {
		c.seen, c.hi, c.bitmap = true, seq, 1
		return true
	}
	if seq > c.hi {
		shift := seq - c.hi
		if shift >= 64 {
			c.bitmap = 0
		} else {
			c.bitmap <<= shift
		}
		c.bitmap |= 1
		c.hi = seq
		return true
	}
	// 落在窗口内：看那一位有没有被标记过
	diff := c.hi - seq
	if diff >= 64 {
		return false // 太老，窗口已经滑过去了
	}
	mask := uint64(1) << diff
	if c.bitmap&mask != 0 {
		return false // 重复包
	}
	c.bitmap |= mask
	return true
}
