package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"testing"
)

// 构造 32px 渐变测试图：像素 (x,y) 颜色随坐标线性变化，便于回读校验。
func gradientImage(side int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, side, side))
	for y := 0; y < side; y++ {
		for x := 0; x < side; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 8), G: uint8(y * 8), B: 0x40, A: 0xFF})
		}
	}
	return img
}

// ICO 结构守卫：ICONDIR 头、目录项字段、BMP 条目可被 bmp 解码器回读且像素
// 自底向上映射正确——曾因手工生成损坏 ico 导致安装器图标异常，此测试防回归。
func TestBuildICO_BMPEntryStructure(t *testing.T) {
	const side = 32
	src := gradientImage(side)

	data, count, err := buildICO(src, []int{side})
	if err != nil {
		t.Fatalf("buildICO: %v", err)
	}
	if count != 1 {
		t.Fatalf("条目数 = %d, 期望 1", count)
	}
	if got := binary.LittleEndian.Uint16(data[0:]); got != 0 {
		t.Errorf("ICONDIR reserved = %d, 期望 0", got)
	}
	if got := binary.LittleEndian.Uint16(data[2:]); got != 1 {
		t.Errorf("ICONDIR type = %d, 期望 1 (icon)", got)
	}
	if got := binary.LittleEndian.Uint16(data[4:]); got != 1 {
		t.Errorf("ICONDIR count = %d, 期望 1", got)
	}
	// 目录项：32 记原值、planes/bitcount、数据偏移紧跟目录
	entry := data[6 : 6+16]
	if entry[0] != side || entry[1] != side {
		t.Errorf("条目尺寸 = %dx%d, 期望 %dx%d", entry[0], entry[1], side, side)
	}
	if planes := binary.LittleEndian.Uint16(entry[4:]); planes != 1 {
		t.Errorf("planes = %d, 期望 1", planes)
	}
	if bits := binary.LittleEndian.Uint16(entry[6:]); bits != 32 {
		t.Errorf("bitcount = %d, 期望 32", bits)
	}
	if off := binary.LittleEndian.Uint32(entry[12:]); int(off) != 6+16 {
		t.Errorf("数据偏移 = %d, 期望 %d", off, 6+16)
	}

	// BMP 条目头：biSize=40、biHeight=XOR+AND 两段即 2×高
	imgData := data[6+16:]
	if size := binary.LittleEndian.Uint32(imgData[0:]); size != 40 {
		t.Errorf("biSize = %d, 期望 40", size)
	}
	if h := binary.LittleEndian.Uint32(imgData[8:]); h != side*2 {
		t.Errorf("biHeight = %d, 期望 %d (XOR+AND)", h, side*2)
	}

	// XOR 段像素直接按布局断言（x/image/bmp 不支持 32bpp BI_RGB 回读）：
	// 行自底向上，首行 = 源图最后一行；BGRA 字节序。源图渐变 R 沿 x、G 沿 y，
	// 像素 (0, side-1)：B=0x40、G=(side-1)*8、R=0、A=0xFF。
	andRow := ((side + 31) / 32) * 4
	andLen := andRow * side
	if total := 40 + side*4*side + andLen; len(imgData) != total {
		t.Fatalf("BMP 数据长 = %d, 期望 %d", len(imgData), total)
	}
	bgra := imgData[40 : 40+4] // XOR 顶行首像素
	if bgra[0] != 0x40 || bgra[1] != (side-1)*8 || bgra[2] != 0 || bgra[3] != 0xFF {
		t.Errorf("顶行首像素 BGRA = % x, 期望 B=40 G=%d R=0 A=FF", bgra, (side-1)*8)
	}
}

// 256px 条目应走 PNG 内嵌：数据以 PNG 签名开头且可解码回原图尺寸。
func TestBuildICO_PNGEntryFor256(t *testing.T) {
	src := gradientImage(256)
	data, count, err := buildICO(src, []int{256})
	if err != nil {
		t.Fatalf("buildICO: %v", err)
	}
	if count != 1 {
		t.Fatalf("条目数 = %d, 期望 1", count)
	}
	entry := data[6 : 6+16]
	if entry[0] != 0 || entry[1] != 0 {
		t.Errorf("256 应记作 0，got %d/%d", entry[0], entry[1])
	}
	imgData := data[6+16:]
	if imgData[0] != 0x89 || imgData[1] != 0x50 {
		t.Fatalf("256px 条目应以 PNG 签名开头，got % x", imgData[:2])
	}
	decoded, err := png.Decode(bytes.NewReader(imgData))
	if err != nil {
		t.Fatalf("png 回读失败: %v", err)
	}
	if decoded.Bounds().Dx() != 256 || decoded.Bounds().Dy() != 256 {
		t.Errorf("回读尺寸 = %v, 期望 256x256", decoded.Bounds())
	}
}

// parseSizes 去重、升序归一与范围校验。
func TestParseSizes(t *testing.T) {
	got, err := parseSizes("256, 16, 48,16, 24")
	if err != nil {
		t.Fatalf("parseSizes: %v", err)
	}
	want := []int{16, 24, 48, 256}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("结果 = %v, 期望 %v", got, want)
		}
	}
	if _, err := parseSizes("0"); err == nil {
		t.Error("0 应被拒绝")
	}
	if _, err := parseSizes("300"); err == nil {
		t.Error("300 应被拒绝")
	}
	if _, err := parseSizes("  "); err == nil {
		t.Error("空列表应被拒绝")
	}
}
