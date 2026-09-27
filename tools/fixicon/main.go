//go:build ignore

// Command fixicon 从多帧 ico 里挑出 BMP 帧，重新打包成一个干净的 ico。
//
// 起因：walk 的 NewIconFromFile 走的是 LoadIconWithScaleDown，遇到含 PNG 压缩帧的 ico
// 会直接失败 —— 而原始图标的 256x256 帧正是 PNG。窗口和托盘只用得到 16~48，
// 所以把 PNG 帧和大尺寸帧一并去掉，只留 BMP 帧。
//
// 用法：go run tools/fixicon/main.go 源.ico 目标.ico
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
)

const (
	dirHeaderSize = 6
	dirEntrySize  = 16
)

type entry struct {
	width, height byte
	colorCount    byte
	reserved      byte
	planes        uint16
	bpp           uint16
	size          uint32
	offset        uint32
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "用法: fixicon <源.ico> <目标.ico>")
		os.Exit(2)
	}
	raw, err := os.ReadFile(os.Args[1])
	check(err)

	if len(raw) < dirHeaderSize {
		fail("文件太小，不像 ico")
	}
	if binary.LittleEndian.Uint16(raw[2:4]) != 1 {
		fail("不是图标类型（ICONDIR.type != 1）")
	}
	count := int(binary.LittleEndian.Uint16(raw[4:6]))

	var keep []entry
	for i := 0; i < count; i++ {
		off := dirHeaderSize + i*dirEntrySize
		if off+dirEntrySize > len(raw) {
			fail("目录项越界")
		}
		e := entry{
			width:      raw[off],
			height:     raw[off+1],
			colorCount: raw[off+2],
			reserved:   raw[off+3],
			planes:     binary.LittleEndian.Uint16(raw[off+4 : off+6]),
			bpp:        binary.LittleEndian.Uint16(raw[off+6 : off+8]),
			size:       binary.LittleEndian.Uint32(raw[off+8 : off+12]),
			offset:     binary.LittleEndian.Uint32(raw[off+12 : off+16]),
		}
		side := int(e.width)
		if side == 0 { // 256 在目录里记作 0
			side = 256
		}
		end := int(e.offset) + int(e.size)
		if end > len(raw) {
			fail("图像数据越界")
		}
		data := raw[e.offset:end]

		switch {
		case bytes.HasPrefix(data, []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}):
			fmt.Printf("跳过 %dx%d：PNG 压缩帧（LoadIconWithScaleDown 不支持）\n", side, side)
		case side > 48:
			fmt.Printf("跳过 %dx%d：超出窗口与托盘所需\n", side, side)
		default:
			fmt.Printf("保留 %dx%d %dbpp %d 字节\n", side, side, e.bpp, e.size)
			keep = append(keep, e)
		}
	}
	if len(keep) == 0 {
		fail("没有任何可保留的帧")
	}

	out := make([]byte, dirHeaderSize+len(keep)*dirEntrySize)
	binary.LittleEndian.PutUint16(out[2:4], 1)
	binary.LittleEndian.PutUint16(out[4:6], uint16(len(keep)))

	var body bytes.Buffer
	for i, e := range keep {
		off := dirHeaderSize + i*dirEntrySize
		copy(out[off:off+dirEntrySize], raw[dirHeaderSize+i*dirEntrySize:dirHeaderSize+(i+1)*dirEntrySize])
		// 重新指向新的数据偏移
		newOffset := uint32(dirHeaderSize + len(keep)*dirEntrySize + body.Len())
		binary.LittleEndian.PutUint32(out[off+12:off+16], newOffset)
		body.Write(raw[e.offset : e.offset+e.size])
	}

	check(os.WriteFile(os.Args[2], append(out, body.Bytes()...), 0o644))
	fmt.Printf("写出 %s：%d 帧，%d 字节\n", os.Args[2], len(keep), dirHeaderSize+len(keep)*dirEntrySize+body.Len())
}

func check(err error) {
	if err != nil {
		fail(err.Error())
	}
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "fixicon:", msg)
	os.Exit(1)
}
