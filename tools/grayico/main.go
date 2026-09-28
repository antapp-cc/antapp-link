package main

// 一次性工具：由原始 .ico 生成托盘用的两个变色版（32bpp BMP 直存格式）。
// 每个像素按原亮度在目标色的暗端→亮端之间取值，保留 alpha 与明暗层次：
//
//	go run main.go <原始.ico> <黑灰-未连接.ico> <鲜绿-已连接.ico>

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

	// 每通道 {暗端, 亮端}
	variants := [2][3][2]int{
		{{25, 118}, {27, 122}, {30, 128}}, // 未连接：黑灰
		{{18, 70}, {92, 235}, {42, 105}},  // 已连接：鲜绿
	}
	outs := make([][]byte, 2)
	for v := range outs {
		outs[v] = append([]byte(nil), data[:6+16*int(count)]...)
	}

	for i := 0; i < int(count); i++ {
		e := data[6+16*i : 6+16*i+16]
		size := binary.LittleEndian.Uint32(e[8:12])
		off := binary.LittleEndian.Uint32(e[12:16])
		for v, out := range outs {
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
					lum := (299*int(r) + 587*int(g) + 114*int(b)) / 1000
					for c := 0; c < 3; c++ {
						lo, hi := variants[v][c][0], variants[v][c][1]
						row[x*4+c] = uint8(lo + (hi-lo)*lum/255)
					}
				}
			}
			newOff := uint32(len(out))
			out = append(out, img...)
			binary.LittleEndian.PutUint32(out[6+16*i+12:], newOff)
			outs[v] = out
			fmt.Printf("  %dx%d 变色完成（%s）\n", w, h, map[int]string{0: "黑灰", 1: "鲜绿"}[v])
		}
	}
	check(os.WriteFile(os.Args[2], outs[0], 0o644))
	check(os.WriteFile(os.Args[3], outs[1], 0o644))
	fmt.Println("写出:", os.Args[2], len(outs[0]), "字节;", os.Args[3], len(outs[1]), "字节")
}
