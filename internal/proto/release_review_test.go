package proto

import (
	"encoding/binary"
	"testing"
)

func TestReleaseReview_IPv4OptionsPreserveFlowKey(t *testing.T) {
	packet := func(opt bool) []byte {
		h := 20
		if opt {
			h = 24
		}
		p := make([]byte, h+20)
		p[0] = 0x40 | byte(h/4)
		p[9] = 6
		p[8] = 64
		binary.BigEndian.PutUint16(p[2:4], uint16(len(p)))
		copy(p[12:16], []byte{10, 10, 0, 2})
		copy(p[16:20], []byte{8, 8, 8, 8})
		binary.BigEndian.PutUint16(p[h:h+2], 45000)
		binary.BigEndian.PutUint16(p[h+2:h+4], 443)
		p[h+12] = 0x50
		p[h+13] = 0x10
		if opt {
			copy(p[20:24], []byte{1, 1, 1, 1})
		}
		return p
	}
	a, _, _, _ := flowKeyMeta(packet(false))
	b, _, _, _ := flowKeyMeta(packet(true))
	if a != b {
		t.Fatalf("same TCP flow changes key when IPv4 options appear: no_options=%x options=%x", a.b, b.b)
	}
}
func TestReleaseReview_IPv4OptionsDoNotForgeRST(t *testing.T) {
	p := make([]byte, 60)
	p[0] = 0x4a
	p[9] = 6
	binary.BigEndian.PutUint16(p[2:4], uint16(len(p)))
	copy(p[12:16], []byte{10, 10, 0, 2})
	copy(p[16:20], []byte{8, 8, 8, 8})
	copy(p[20:39], []byte{7, 19, 4, 192, 0, 2, 1, 192, 0, 2, 2, 192, 0, 4, 1, 192, 0, 2, 4})
	p[39] = 0
	binary.BigEndian.PutUint16(p[40:42], 45000)
	binary.BigEndian.PutUint16(p[42:44], 443)
	p[52] = 0x50
	p[53] = 0x10
	_, _, flags, ok := flowKeyMeta(p)
	if !ok || flags != 0x10 {
		t.Fatalf("real TCP ACK=0x10 but parser returned flags=%#x ok=%v (IPv4 RR option byte 33 is not TCP flags)", flags, ok)
	}
}

func TestReleaseReview_IPv4HeaderBounds(t *testing.T) {
	p := make([]byte, 24)
	p[9] = 6
	for _, first := range []byte{0x40, 0x44, 0x4f} {
		p[0] = first
		if _, _, _, ok := flowKeyMeta(p); ok {
			t.Fatalf("invalid IHL accepted: %x", first)
		}
	}
	p[0] = 0x45
	if _, _, flags, ok := flowKeyMeta(p); !ok || flags != 0 {
		t.Fatal("short transport header must preserve port parsing without flags")
	}
}
