package main

// 一次性工具：由原始 .ico 生成客户端用的三件套（32bpp BMP 直存格式）。
// 各尺寸统一从最大条目双线性重采样：先裁掉四周透明边距，再放大到画布 96% 铺满
// （微信/QQ 那种顶满的视觉大小），最后按目标变色：
//
//	go run main.go <原始.ico> <彩色铺满.ico> <黑灰-未连接.ico> <鲜绿-已连接.ico>

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

type entry struct {
	w, h int
	rgba []byte // top-down RGBA
	raw  []byte // 原条目字节（头 + 像素 + 掩码），掩码原样保留
}

func main() {
	data, err := os.ReadFile(os.Args[1])
	check(err)
	count := int(binary.LittleEndian.Uint16(data[4:6]))
	fmt.Printf("条目数: %d\n", count)

	ents := make([]entry, count)
	for i := 0; i < count; i++ {
		e := data[6+16*i : 6+16*i+16]
		size := int(binary.LittleEndian.Uint32(e[8:12]))
		off := int(binary.LittleEndian.Uint32(e[12:16]))
		raw := append([]byte(nil), data[off:off+size]...)
		w := int(int32(binary.LittleEndian.Uint32(raw[4:8])))
		h := int(int32(binary.LittleEndian.Uint32(raw[8:12]))) / 2
		if binary.LittleEndian.Uint16(raw[14:16]) != 32 {
			panic(fmt.Sprintf("条目 %d 不是 32bpp", i))
		}
		rgba := make([]byte, w*h*4)
		for y := 0; y < h; y++ {
			src := raw[40+(h-1-y)*w*4 : 40+(h-y)*w*4]
			dst := rgba[y*w*4 : (y+1)*w*4]
			for x := 0; x < w; x++ {
				dst[x*4], dst[x*4+1], dst[x*4+2], dst[x*4+3] = src[x*4+2], src[x*4+1], src[x*4], src[x*4+3]
			}
		}
		ents[i] = entry{w: w, h: h, rgba: rgba, raw: raw}
	}

	// 最大条目当统一采样源：小尺寸从它缩小，质量最好
	src := ents[0]
	for _, e := range ents[1:] {
		if e.w*e.h > src.w*src.h {
			src = e
		}
	}
	bx0, by0, bx1, by1 := alphaBBox(src)
	bw, bh := bx1-bx0+1, by1-by0+1
	fmt.Printf("内容边界: (%d,%d)-(%d,%d) %dx%d / 画布 %dx%d\n", bx0, by0, bx1, by1, bw, bh, src.w, src.h)

	const fill = 0.96
	variants := []struct {
		name string
		tint *[3][2]int
	}{
		{"彩色铺满", nil},
		{"黑灰", &[3][2]int{{25, 118}, {27, 122}, {30, 128}}},
		{"鲜绿", &[3][2]int{{18, 70}, {92, 235}, {42, 105}}},
	}

	for vi, v := range variants {
		out := append([]byte(nil), data[:6+16*count]...)
		for i, ent := range ents {
			img := append([]byte(nil), ent.raw...)
			canvas := renderScaled(src, bx0, by0, bw, bh, ent.w, ent.h, fill)
			px := img[40 : 40+ent.w*ent.h*4]
			for y := 0; y < ent.h; y++ {
				for x := 0; x < ent.w; x++ {
					d := (ent.h-1-y)*ent.w*4 + x*4 // 输出 bottom-up BGRA
					s := (y*ent.w + x) * 4
					r, g, b, a := canvas[s], canvas[s+1], canvas[s+2], canvas[s+3]
					if v.tint != nil && a > 0 {
						lum := (299*int(r) + 587*int(g) + 114*int(b)) / 1000
						rr := tintC(v.tint[0], lum)
						gg := tintC(v.tint[1], lum)
						bb := tintC(v.tint[2], lum)
						r, g, b = uint8(rr), uint8(gg), uint8(bb)
					}
					px[d], px[d+1], px[d+2], px[d+3] = b, g, r, a
				}
			}
			newOff := uint32(len(out))
			out = append(out, img...)
			binary.LittleEndian.PutUint32(out[6+16*i+12:], newOff)
			_ = vi
		}
		check(os.WriteFile(os.Args[2+vi], out, 0o644))
		fmt.Printf("写出 %s: %s（%d 字节）\n", v.name, os.Args[2+vi], len(out))
	}
}

func tintC(ch [2]int, lum int) int {
	return ch[0] + (ch[1]-ch[0])*lum/255
}

func alphaBBox(e entry) (x0, y0, x1, y1 int) {
	x0, y0, x1, y1 = e.w, e.h, 0, 0
	for y := 0; y < e.h; y++ {
		for x := 0; x < e.w; x++ {
			if e.rgba[(y*e.w+x)*4+3] > 16 {
				if x < x0 {
					x0 = x
				}
				if x > x1 {
					x1 = x
				}
				if y < y0 {
					y0 = y
				}
				if y > y1 {
					y1 = y
				}
			}
		}
	}
	return
}

// renderScaled：把 src 的内容边界区双线性缩放到正好铺满 W×H 画布的 96%，居中。
func renderScaled(src entry, bx0, by0, bw, bh, W, H int, fill float64) []byte {
	out := make([]byte, W*H*4)
	s := float64(W) * fill / float64(bw)
	if sh := float64(H) * fill / float64(bh); sh < s {
		s = sh
	}
	cw, ch := int(float64(bw)*s+0.5), int(float64(bh)*s+0.5)
	ox, oy := (W-cw)/2, (H-ch)/2
	for y := 0; y < ch; y++ {
		fy := float64(by0) + (float64(y)+0.5)/s - 0.5
		y0 := clamp(int(fy), 0, src.h-1)
		y1 := clamp(y0+1, 0, src.h-1)
		wy := clampF(fy-float64(y0), 0, 1)
		for x := 0; x < cw; x++ {
			fx := float64(bx0) + (float64(x)+0.5)/s - 0.5
			x0 := clamp(int(fx), 0, src.w-1)
			x1 := clamp(x0+1, 0, src.w-1)
			wx := clampF(fx-float64(x0), 0, 1)
			d := ((y+oy)*W + (x + ox)) * 4
			for c := 0; c < 4; c++ {
				p00 := float64(src.rgba[(y0*src.w+x0)*4+c])
				p01 := float64(src.rgba[(y0*src.w+x1)*4+c])
				p10 := float64(src.rgba[(y1*src.w+x0)*4+c])
				p11 := float64(src.rgba[(y1*src.w+x1)*4+c])
				out[d+c] = uint8(bilerp(p00, p01, p10, p11, wx, wy) + 0.5)
			}
		}
	}
	return out
}

func bilerp(p00, p01, p10, p11, wx, wy float64) float64 {
	top := p00 + (p01-p00)*wx
	bot := p10 + (p11-p10)*wx
	return top + (bot-top)*wy
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func clampF(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
