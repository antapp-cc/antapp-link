package proto

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestWriteReadRoundTrip(t *testing.T) {
	cases := []struct {
		name    string
		typ     Type
		payload []byte
	}{
		{"握手带 JSON", TypeHello, []byte(`{"version":1}`)},
		{"空载荷", TypePong, nil},
		{"零长非 nil 载荷", TypePing, []byte{}},
		{"IP 包", TypeIP, bytes.Repeat([]byte{0x45}, 1400)},
		{"上限载荷", TypeIP, bytes.Repeat([]byte{0xAA}, MaxPayload)},
		{"未知类型照样透传", Type(0x7F), []byte("future")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteFrame(&buf, c.typ, c.payload); err != nil {
				t.Fatalf("WriteFrame: %v", err)
			}
			typ, payload, err := ReadFrame(&buf)
			if err != nil {
				t.Fatalf("ReadFrame: %v", err)
			}
			if typ != c.typ {
				t.Errorf("type = %v, want %v", typ, c.typ)
			}
			if !bytes.Equal(payload, c.payload) {
				t.Errorf("payload = %d 字节, want %d 字节", len(payload), len(c.payload))
			}
			if buf.Len() != 0 {
				t.Errorf("还剩 %d 字节没消费，帧边界算错了", buf.Len())
			}
		})
	}
}

func TestWriteFrameRejectsOversizeWithoutWriting(t *testing.T) {
	var buf bytes.Buffer
	err := WriteFrame(&buf, TypeIP, make([]byte, MaxPayload+1))
	if !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("err = %v, want ErrPayloadTooLarge", err)
	}
	if buf.Len() != 0 {
		t.Errorf("超长帧不该写出任何字节，实际写了 %d 字节", buf.Len())
	}
}

func TestReadFrameTruncated(t *testing.T) {
	t.Run("帧头截断", func(t *testing.T) {
		_, _, err := ReadFrame(bytes.NewReader([]byte{byte(TypeIP), 0x00}))
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("err = %v, want io.ErrUnexpectedEOF", err)
		}
	})
	t.Run("载荷截断", func(t *testing.T) {
		full := new(bytes.Buffer)
		if err := WriteFrame(full, TypeIP, []byte("0123456789")); err != nil {
			t.Fatal(err)
		}
		_, _, err := ReadFrame(bytes.NewReader(full.Bytes()[:HeaderSize+4]))
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("err = %v, want io.ErrUnexpectedEOF", err)
		}
	})
	t.Run("空流", func(t *testing.T) {
		_, _, err := ReadFrame(bytes.NewReader(nil))
		if !errors.Is(err, io.EOF) {
			t.Errorf("err = %v, want io.EOF", err)
		}
	})
}

func TestReadFrameRejectsOversizeLength(t *testing.T) {
	hdr := []byte{byte(TypeIP), 0xFF, 0xFF, 0xFF}
	_, _, err := ReadFrame(bytes.NewReader(hdr))
	if !errors.Is(err, ErrPayloadTooLarge) {
		t.Errorf("err = %v, want ErrPayloadTooLarge", err)
	}
}

func TestUint32Helpers(t *testing.T) {
	b := PutUint32(0xDEADBEEF)
	v, ok := GetUint32(b)
	if !ok || v != 0xDEADBEEF {
		t.Errorf("GetUint32 = (%v, %v), want (0xDEADBEEF, true)", v, ok)
	}
	if _, ok := GetUint32([]byte{1, 2, 3}); ok {
		t.Error("长度不对时应该返回 false")
	}
}

// 类型名会出现在日志与握手错误里，新增帧类型必须登记，否则排障只看到 TYPE(0x08)。
func TestTypeStringCoversAllKnownTypes(t *testing.T) {
	for typ, want := range map[Type]string{
		TypeHello:     "HELLO",
		TypeHelloAck:  "HELLO_ACK",
		TypeIP:        "IP",
		TypePing:      "PING",
		TypePong:      "PONG",
		TypeBye:       "BYE",
		TypeUDPPort:   "UDP_PORT",
		TypeMemberAck: "MEMBER_ACK",
	} {
		if got := typ.String(); got != want {
			t.Errorf("Type(%#02x).String() = %q，期望 %q", uint8(typ), got, want)
		}
	}
}
