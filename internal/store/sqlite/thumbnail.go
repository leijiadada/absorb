package sqlite

// 多模态落库降级（方案 A，design/image-pipeline-plan.md）：dataURL 图片在 AppendEvent
// 落库前降采样为缩略图（替换原 part），控制 SQLite 体积的同时为跨轮上下文保留视觉参考
// （SDK scope 重放与工作集 L1 注入共用库内缩略图）。纯标准库实现，不引第三方依赖。
//
// 降级策略（downscaleImagePart）：
//   - 外链图（http/https）：体积本身可控（仅 URL 数十字节），原样保留——视觉模型跨轮
//     仍可经 URL 取图；非视觉模型由 openai 层 WithTextOnlyMessageContent（见 chat/agent.go）
//     在序列化时剥离 image part，无 1210 风险；
//   - dataURL 图：解码 → 最长边超限时均值池化缩到 192px → JPEG q60 重编码替换原 URL；
//     小图（≤192px）已为 KB 级，原样保留避免二次编码损耗；
//   - 解码/编码失败（损坏数据、webp 等标准库不支持的格式）：返回 false，调用方丢弃
//     （维持旧策略「落库剥离」，宁缺勿错，不阻断落库）。
//
// 透明通道说明：均值池化输出 RGBA，标准库 JPEG 编码忽略 alpha——含透明的 PNG 缩略图
// 透明区呈黑色。截图类图片基本不透明，可接受。
//
// 位置说明：本文件置于 sqlite 包而非 chat 包——降级仅被 AppendEvent 调用，且 chat 已
// import store，反向依赖会成环。

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	_ "image/png" // 注册 PNG 解码器（jpeg 由 image/jpeg 显式引入已注册）
	"strings"

	"trpc.group/trpc-go/trpc-agent-go/event"
	"trpc.group/trpc-go/trpc-agent-go/model"
)

// thumbMaxSide 缩略图最长边（px）：JPEG q60 下约 3-8KB/张，9 张上限 ~70KB，
// SQLite 增量可忽略；对视觉模型足以辨认布局与主要元素。
const thumbMaxSide = 192

// thumbQuality 缩略图 JPEG 质量。
const thumbQuality = 60

// hasImageParts 报告 parts 中是否存在 image part（决定是否值得构造降级副本）。
func hasImageParts(parts []model.ContentPart) bool {
	for _, p := range parts {
		if p.Type == model.ContentType("image") {
			return true
		}
	}
	return false
}

// shallowDowngradeEvent 构造落库用的降级副本：Response/Choices/Message 逐层浅拷贝，
// image part 经 downscaleImagePart 降级（外链原样 / dataURL 缩略替换 / 失败丢弃），
// 其余 part 与无图事件共享原引用。原 evt 及内存 sess 中引用保持原图不变。
func shallowDowngradeEvent(evt *event.Event) *event.Event {
	if evt == nil || evt.Response == nil {
		return evt
	}
	resp := *evt.Response
	chs := make([]model.Choice, len(resp.Choices))
	copy(chs, resp.Choices)
	downgraded := false
	for ci := range chs {
		parts := chs[ci].Message.ContentParts
		if len(parts) == 0 || !hasImageParts(parts) {
			continue
		}
		kept := make([]model.ContentPart, 0, len(parts))
		for _, p := range parts { // range 复制元素：降级只作用于副本
			if p.Type == model.ContentType("image") {
				if downscaleImagePart(&p) {
					kept = append(kept, p)
				}
				continue
			}
			kept = append(kept, p)
		}
		chs[ci].Message.ContentParts = kept
		downgraded = true
	}
	if !downgraded {
		return evt // 无图事件返回原 evt，避免无谓分配
	}
	resp.Choices = chs
	out := *evt
	out.Response = &resp
	return &out
}

// downscaleImagePart 原地降级单个 image part。返回 true 表示 part 应保留
// （外链原样 / dataURL 已降为缩略图 / 小图原样）；false 表示应丢弃。
func downscaleImagePart(p *model.ContentPart) bool {
	if p.Image == nil {
		return false
	}
	u := p.Image.URL
	// 外链图：URL 数十字节，原样保留（跨轮视觉模型仍可按 URL 取图）
	if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
		return true
	}
	thumb, err := decodeThumbDataURL(u)
	if err != nil {
		return false
	}
	if thumb == u { // 小图原样（decodeThumbDataURL 对 ≤maxSide 返回入参）
		return true
	}
	// Image 是指针：必须整结构替换而非改字段——p 是 part 副本但 *Image 与原 evt
	// 共享，原地改 URL 会污染内存 sess（当轮请求应保持原图，见 shallowDowngradeEvent）
	img := *p.Image
	img.URL = thumb
	img.Format = "jpeg"
	img.Data = nil
	p.Image = &img
	return true
}

// decodeThumbDataURL 解析 dataURL 图片并按需降采样。最长边 > thumbMaxSide 时返回
// JPEG 缩略 dataURL（等比，均值池化）；否则原样返回入参（小图零损耗）。
// 解析/解码失败（损坏数据、不支持格式如 webp）返回错误。
func decodeThumbDataURL(dataURL string) (string, error) {
	const b64Mark = ";base64,"
	if !strings.HasPrefix(dataURL, "data:image/") {
		return "", fmt.Errorf("非 data:image 形态的 dataURL")
	}
	i := strings.Index(dataURL, b64Mark)
	if i < 0 {
		return "", fmt.Errorf("dataURL 缺少 base64 段")
	}
	raw, err := base64.StdEncoding.DecodeString(dataURL[i+len(b64Mark):])
	if err != nil {
		return "", fmt.Errorf("base64 解码失败: %w", err)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("图片解析失败（含标准库不支持的格式如 webp）: %w", err)
	}
	if cfg.Width <= thumbMaxSide && cfg.Height <= thumbMaxSide {
		return dataURL, nil
	}
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("图片解码失败: %w", err)
	}
	tw, th := fitThumb(cfg.Width, cfg.Height)
	dst := resizeBox(src, tw, th)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: thumbQuality}); err != nil {
		return "", fmt.Errorf("jpeg 编码失败: %w", err)
	}
	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// fitThumb 等比计算缩放目标尺寸（最长边 = thumbMaxSide，短边至少 1px）。
func fitThumb(w, h int) (int, int) {
	if w >= h {
		nh := h * thumbMaxSide / w
		if nh < 1 {
			nh = 1
		}
		return thumbMaxSide, nh
	}
	nw := w * thumbMaxSide / h
	if nw < 1 {
		nw = 1
	}
	return nw, thumbMaxSide
}

// resizeBox 均值池化缩放（box filter）：目标像素取源图对应矩形区域的通道均值，
// 比近邻采样抗锯齿（截图文字缩略更可辨）。RGBA() 返回 16bit 预乘值，平均后折算 8bit。
func resizeBox(src image.Image, w, h int) *image.RGBA {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		y0, y1 := y*sh/h, (y+1)*sh/h
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < w; x++ {
			x0, x1 := x*sw/w, (x+1)*sw/w
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var r, g, bl, a, n uint64
			for yy := y0; yy < y1 && yy < sh; yy++ {
				for xx := x0; xx < x1 && xx < sw; xx++ {
					cr, cg, cb, ca := src.At(b.Min.X+xx, b.Min.Y+yy).RGBA()
					r += uint64(cr)
					g += uint64(cg)
					bl += uint64(cb)
					a += uint64(ca)
					n++
				}
			}
			if n == 0 {
				n = 1
			}
			dst.SetRGBA(x, y, color.RGBA{
				R: uint8(r / n >> 8), G: uint8(g / n >> 8),
				B: uint8(bl / n >> 8), A: uint8(a / n >> 8),
			})
		}
	}
	return dst
}
