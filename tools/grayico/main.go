package main

// 一次性工具：把 32bpp BMP 直存的 .ico 整体去色（保留 alpha），生成灰色版。
// 用法: go run grayico.go <in.ico> <out.ico>

import (
	"encoding/binary"
	"fmt"
	"os"
)

func check(err error) {
	if err != nil {
		panic(err)
	}
}

func main() {
	data, err := os.ReadFile(os.Args[1])
	check(err)
	count := binary.LittleEndian.Uint16(data[4:6])
	fmt.Printf("条目数: %d\n", count)
	out := append([]byte(nil), data[:6+16*int(count)]...)
	for i := 0; i < int(count); i++ {
		e := data[6+16*i : 6+16*i+16]
		size := binary.LittleEndian.Uint32(e[8:12])
		off := binary.LittleEndian.Uint32(e[12:16])
		img := append([]byte(nil), data[off:off+size]...)
		w := int(int32(binary.LittleEndian.Uint32(img[4:8])))
		h := int(int32(binary.LittleEndian.Uint32(img[8:12]))) / 2
		bpp := binary.LittleEndian.Uint16(img[14:16])
		if bpp != 32 {
			panic(fmt.Sprintf("条目 %d 是 %d bpp，只处理 32bpp", i, bpp))
		}
		px := img[40:]
		for y := 0; y < h; y++ {
			row := px[y*w*4 : (y+1)*w*4]
			for x := 0; x < w; x++ {
				b, g, r := row[x*4], row[x*4+1], row[x*4+2]
				lum := uint8((299*int(r) + 587*int(g) + 114*int(b)) / 1000)
				row[x*4], row[x*4+1], row[x*4+2] = lum, lum, lum
			}
		}
		newOff := uint32(len(out))
		out = append(out, img...)
		binary.LittleEndian.PutUint32(out[6+16*i+12:], newOff)
		fmt.Printf("  %dx%d 去色完成\n", w, h)
	}
	check(os.WriteFile(os.Args[2], out, 0o644))
	fmt.Println("写出:", os.Args[2], len(out), "字节")
}
