package sqlite

// thumbnail（方案 A 落库缩略图降级）单元测试：
// dataURL 解析/降采样/小图直通/非法输入报错 + downscaleImagePart 分流
// （dataURL 大图替换、外链原样、Image=nil 与解码失败丢弃）。

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"

	"trpc.group/trpc-go/trpc-agent-go/model"
)

// mkImageDataURL 生成 w×h 测试图片 dataURL（红/蓝随横坐标渐变），format: "jpeg"|"png"。
func mkImageDataURL(t *testing.T, w, h int, format string) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{R: uint8(x * 255 / w), G: 64, B: uint8(255 - x*255/w), A: 255})
		}
	}
	var buf bytes.Buffer
	var err error
	if format == "png" {
		err = png.Encode(&buf, img)
	} else {
		err = jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90})
	}
	if err != nil {
		t.Fatalf("encode %s: %v", format, err)
	}
	return "data:image/" + format + ";base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

// thumbDims 解析缩略 dataURL 的像素尺寸。
func thumbDims(t *testing.T, dataURL string) (int, int) {
	t.Helper()
	const b64Mark = ";base64,"
	i := strings.Index(dataURL, b64Mark)
	if i < 0 {
		t.Fatalf("非 base64 dataURL: %.40s", dataURL)
	}
	raw, err := base64.StdEncoding.DecodeString(dataURL[i+len(b64Mark):])
	if err != nil {
		t.Fatalf("base64 解码: %v", err)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("DecodeConfig: %v", err)
	}
	return cfg.Width, cfg.Height
}

func TestDecodeThumbDataURL(t *testing.T) {
	t.Run("横图缩到最长边192", func(t *testing.T) {
		in := mkImageDataURL(t, 1280, 800, "jpeg")
		out, err := decodeThumbDataURL(in)
		if err != nil {
			t.Fatalf("decodeThumbDataURL: %v", err)
		}
		if out == in {
			t.Fatal("大图应被缩放，不应原样返回")
		}
		if !strings.HasPrefix(out, "data:image/jpeg;base64,") {
			t.Errorf("缩略图应为 jpeg dataURL: %.40s", out)
		}
		w, h := thumbDims(t, out)
		if w != 192 || h != 120 { // 1280x800 等比 → 192x120
			t.Errorf("dims = %dx%d, want 192x120", w, h)
		}
	})

	t.Run("竖图等比缩放", func(t *testing.T) {
		out, err := decodeThumbDataURL(mkImageDataURL(t, 600, 1200, "jpeg"))
		if err != nil {
			t.Fatalf("decodeThumbDataURL: %v", err)
		}
		w, h := thumbDims(t, out)
		if w != 96 || h != 192 { // 600x1200 等比 → 96x192
			t.Errorf("dims = %dx%d, want 96x192", w, h)
		}
	})

	t.Run("png大图转jpeg缩略", func(t *testing.T) {
		out, err := decodeThumbDataURL(mkImageDataURL(t, 400, 400, "png"))
		if err != nil {
			t.Fatalf("decodeThumbDataURL: %v", err)
		}
		if !strings.HasPrefix(out, "data:image/jpeg;base64,") {
			t.Errorf("png 大图应转 jpeg 缩略: %.40s", out)
		}
		if w, h := thumbDims(t, out); w != 192 || h != 192 {
			t.Errorf("dims = %dx%d, want 192x192", w, h)
		}
	})

	t.Run("小图原样返回零损耗", func(t *testing.T) {
		in := mkImageDataURL(t, 100, 80, "jpeg")
		out, err := decodeThumbDataURL(in)
		if err != nil {
			t.Fatalf("decodeThumbDataURL: %v", err)
		}
		if out != in {
			t.Errorf("小图应原样返回，避免二次编码损耗")
		}
	})

	t.Run("非法输入报错不 panic", func(t *testing.T) {
		bad := []string{
			"",
			"https://example.com/a.jpg",
			"data:image/webp;base64,AAAA", // base64 合法但内容非图片（模拟 webp 等标准库不支持的格式）
			"data:image/jpeg;base64,!!!!", // 非法 base64
			"data:text/plain;base64,AAAA",
		}
		for _, in := range bad {
			if out, err := decodeThumbDataURL(in); err == nil {
				t.Errorf("应报错: %q → %.40s", in, out)
			}
		}
	})
}

func TestDownscaleImagePart(t *testing.T) {
	t.Run("dataURL大图替换为缩略图", func(t *testing.T) {
		p := model.ContentPart{Type: model.ContentType("image"),
			Image: &model.Image{URL: mkImageDataURL(t, 1280, 800, "jpeg"), Detail: "auto"}}
		if !downscaleImagePart(&p) {
			t.Fatal("大图 part 应保留（降级为缩略图）")
		}
		if !strings.HasPrefix(p.Image.URL, "data:image/jpeg;base64,") {
			t.Errorf("应替换为 jpeg 缩略: %.40s", p.Image.URL)
		}
		if p.Image.Format != "jpeg" {
			t.Errorf("Format = %q, want jpeg", p.Image.Format)
		}
		if w, h := thumbDims(t, p.Image.URL); w != 192 || h != 120 {
			t.Errorf("dims = %dx%d, want 192x120", w, h)
		}
	})

	t.Run("外链图原样保留", func(t *testing.T) {
		for _, u := range []string{"http://example.com/a.png", "https://example.com/b.jpg"} {
			p := model.ContentPart{Type: model.ContentType("image"), Image: &model.Image{URL: u}}
			if !downscaleImagePart(&p) {
				t.Fatalf("外链 %s 应原样保留", u)
			}
			if p.Image.URL != u {
				t.Errorf("外链 URL 不应被改写: %q", p.Image.URL)
			}
		}
	})

	t.Run("Image为nil与解码失败丢弃", func(t *testing.T) {
		if downscaleImagePart(&model.ContentPart{Type: model.ContentType("image")}) {
			t.Error("Image=nil 应丢弃")
		}
		p := model.ContentPart{Type: model.ContentType("image"),
			Image: &model.Image{URL: "data:image/webp;base64,AAAA"}}
		if downscaleImagePart(&p) {
			t.Error("解码失败（webp 等不支持格式）应丢弃，维持旧策略")
		}
	})
}
