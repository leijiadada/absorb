// genicon 从方形源图生成 Windows 多尺寸 ICO。
//
// 背景：build/windows/icon.ico 曾因手工生成写入损坏的图像数据（各尺寸渲染为
// 噪点），导致安装器/卸载器图标异常，而 exe 侧走 wails 的 appicon.png 链路
// 不受影响——两套链路的差异使问题被掩盖了很久。本工具统一从 appicon.png
// 一键再生成，消除手工环节。
//
// 编码规则：≤128px 走 BMP-in-ICO（BITMAPINFOHEADER + 自底向上 BGRA + AND
// 掩码，兼容性最好）；256px 走 PNG 内嵌（Vista+ 标准，省体积）。
//
// 用法：go run ./cmd/genicon -src build/appicon.png -out build/windows/icon.ico
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/image/draw"
)

func main() {
	src := flag.String("src", "build/appicon.png", "方形源图 PNG（建议 ≥256px）")
	out := flag.String("out", "build/windows/icon.ico", "ICO 输出路径")
	sizesFlag := flag.String("sizes", "16,24,32,48,64,128,256", "逗号分隔的尺寸列表（每项 ≤256）")
	flag.Parse()

	img, err := decodeSource(*src)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取源图失败: %v\n", err)
		os.Exit(1)
	}
	sizes, err := parseSizes(*sizesFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "尺寸列表非法: %v\n", err)
		os.Exit(1)
	}

	data, count, err := buildICO(img, sizes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "打包 ICO 失败: %v\n", err)
		os.Exit(1)
	}
	if dir := filepath.Dir(*out); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "创建输出目录失败: %v\n", err)
			os.Exit(1)
		}
	}
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "写出失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("已生成 %s：%d 字节，%d 个尺寸 %v\n", *out, len(data), count, sizes)
}

// decodeSource 读取并解码方形源图；ICO 各尺寸均为正方形，非方形源图直接拒绝
// 而不是隐式裁切（裁切策略属于设计决策，应由设计源决定）。
func decodeSource(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		return nil, err
	}
	b := img.Bounds()
	if b.Dx() != b.Dy() {
		return nil, fmt.Errorf("源图 %dx%d 非正方形", b.Dx(), b.Dy())
	}
	return img, nil
}

// parseSizes 解析逗号分隔的尺寸列表，去重升序并校验范围。
func parseSizes(s string) ([]int, error) {
	seen := map[int]bool{}
	var sizes []int
	for _, part := range strings.Split(s, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil {
			return nil, fmt.Errorf("%q 不是数字", part)
		}
		if n < 1 || n > 256 {
			return nil, fmt.Errorf("%d 超出 1~256", n)
		}
		if !seen[n] {
			seen[n] = true
			sizes = append(sizes, n)
		}
	}
	if len(sizes) == 0 {
		return nil, fmt.Errorf("至少需要一个尺寸")
	}
	sort.Ints(sizes)
	return sizes, nil
}

// scaleImage 把源图等比缩放到 size×size（Catmull-Rom，小尺寸下边缘保真度
// 明显优于双线性，16px 图标的可辨识度依赖于此）。
func scaleImage(src image.Image, size int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, size, size))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Over, nil)
	return dst
}

// buildICO 打包全部尺寸：目录项前置、图像数据依次后置（offset 顺序累加）。
func buildICO(src image.Image, sizes []int) ([]byte, int, error) {
	type entry struct {
		side int
		data []byte
	}
	var entries []entry
	for _, s := range sizes {
		scaled := scaleImage(src, s)
		if s >= 256 {
			// 256px 条目直接内嵌 PNG（Vista+ 标准做法），省去 BMP 展开体积
			var buf bytes.Buffer
			if err := png.Encode(&buf, scaled); err != nil {
				return nil, 0, err
			}
			entries = append(entries, entry{s, buf.Bytes()})
			continue
		}
		data, err := bmpEntry(scaled)
		if err != nil {
			return nil, 0, err
		}
		entries = append(entries, entry{s, data})
	}

	var out bytes.Buffer
	// ICONDIR：reserved=0 / type=1(icon) / count
	_ = binary.Write(&out, binary.LittleEndian, uint16(0))
	_ = binary.Write(&out, binary.LittleEndian, uint16(1))
	_ = binary.Write(&out, binary.LittleEndian, uint16(len(entries)))
	offset := 6 + 16*len(entries)
	for _, e := range entries {
		out.WriteByte(byte(e.side % 256)) // 256 记作 0
		out.WriteByte(byte(e.side % 256))
		out.WriteByte(0)                                        // 调色板数（32bpp 无调色板）
		out.WriteByte(0)                                        // 保留
		_ = binary.Write(&out, binary.LittleEndian, uint16(1))  // planes
		_ = binary.Write(&out, binary.LittleEndian, uint16(32)) // bitcount
		_ = binary.Write(&out, binary.LittleEndian, uint32(len(e.data)))
		_ = binary.Write(&out, binary.LittleEndian, uint32(offset))
		offset += len(e.data)
	}
	for _, e := range entries {
		out.Write(e.data)
	}
	return out.Bytes(), len(entries), nil
}

// bmpEntry 编码 BMP-in-ICO 图像数据：40 字节 BITMAPINFOHEADER（biHeight 为
// XOR+AND 两段之和即 2×高）+ 自底向上 BGRA 像素 + 1bpp AND 透明掩码。
// 掩码行按 32 位对齐（BMP 规范）；alpha=0 的像素置掩码位，保证无 alpha
// 混合路径下的透明语义。
func bmpEntry(img *image.RGBA) ([]byte, error) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	xor := make([]byte, w*4*h)
	andRow := ((w + 31) / 32) * 4
	and := make([]byte, andRow*h)
	for y := 0; y < h; y++ {
		row := h - 1 - y
		for x := 0; x < w; x++ {
			r, g, bl, a := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
			i := (row*w + x) * 4
			xor[i] = byte(bl >> 8)
			xor[i+1] = byte(g >> 8)
			xor[i+2] = byte(r >> 8)
			xor[i+3] = byte(a >> 8)
			if a == 0 {
				and[row*andRow+x/8] |= 0x80 >> (x % 8)
			}
		}
	}
	var buf bytes.Buffer
	bi := make([]byte, 40)
	binary.LittleEndian.PutUint32(bi[0:], 40) // biSize
	binary.LittleEndian.PutUint32(bi[4:], uint32(w))
	binary.LittleEndian.PutUint32(bi[8:], uint32(h*2))
	binary.LittleEndian.PutUint16(bi[12:], 1) // planes
	binary.LittleEndian.PutUint16(bi[14:], 32)
	binary.LittleEndian.PutUint32(bi[20:], uint32(len(xor)+len(and)))
	buf.Write(bi)
	buf.Write(xor)
	buf.Write(and)
	return buf.Bytes(), nil
}
