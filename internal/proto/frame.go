// Package proto 实现隧道内层的帧编解码，服务端与客户端共用。
//
// 帧格式：1 字节类型 + 24 位大端长度 + 载荷。
package proto

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Type 是帧类型。
type Type uint8

const (
	TypeHello    Type = 0x01
	TypeHelloAck Type = 0x02
	TypeIP       Type = 0x03
	TypePing     Type = 0x04
	TypePong     Type = 0x05
	TypeBye      Type = 0x06
)

func (t Type) String() string {
	switch t {
	case TypeHello:
		return "HELLO"
	case TypeHelloAck:
		return "HELLO_ACK"
	case TypeIP:
		return "IP"
	case TypePing:
		return "PING"
	case TypePong:
		return "PONG"
	case TypeBye:
		return "BYE"
	default:
		return fmt.Sprintf("TYPE(0x%02X)", uint8(t))
	}
}

const (
	// HeaderSize 是帧头长度。
	HeaderSize = 4

	// MaxPayload 留出余量覆盖 1500 字节 MTU 再加帧头，同时挡住对端声称的大长度，
	// 避免被一个畸形帧骗去分配 16 MiB。
	MaxPayload = 2048
)

var (
	ErrPayloadTooLarge = errors.New("proto: payload too large")
)

// WriteFrame 写一帧。超长时在写出任何字节之前就返回错误，避免在对端留下半个帧。
func WriteFrame(w io.Writer, t Type, payload []byte) error {
	if len(payload) > MaxPayload {
		return fmt.Errorf("%w: %d > %d", ErrPayloadTooLarge, len(payload), MaxPayload)
	}
	var hdr [HeaderSize]byte
	hdr[0] = byte(t)
	hdr[1] = byte(len(payload) >> 16)
	hdr[2] = byte(len(payload) >> 8)
	hdr[3] = byte(len(payload))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	_, err := w.Write(payload)
	return err
}

// ReadFrame 读一帧。未知类型如实返回，由上层决定忽略 —— 将来加帧类型不会打断已发布的客户端。
func ReadFrame(r io.Reader) (Type, []byte, error) {
	var hdr [HeaderSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := int(hdr[1])<<16 | int(hdr[2])<<8 | int(hdr[3])
	if n > MaxPayload {
		return 0, nil, fmt.Errorf("%w: %d > %d", ErrPayloadTooLarge, n, MaxPayload)
	}
	if n == 0 {
		return Type(hdr[0]), nil, nil
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, nil, err
	}
	return Type(hdr[0]), buf, nil
}

// PutUint32/GetUint32 供握手载荷复用，避免各处重复 import encoding/binary。
func PutUint32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

func GetUint32(b []byte) (uint32, bool) {
	if len(b) != 4 {
		return 0, false
	}
	return binary.BigEndian.Uint32(b), true
}
