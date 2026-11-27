package sqlite

// thumbnail（方案 A 落库缩略图降级）单元测试：
// dataURL 解析/降采样/小图直通/非法输入报错 + downscaleImagePart 分流
// （dataURL 大图替换、外链原样、Image=nil 与解码失败丢弃）。

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"

	"trpc.group/trpc-go/trpc-agent-go/event"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/session"
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

// thumbStoredEvent 读回会话库中该会话唯一一条事件的 JSON 并反序列化。
func thumbStoredEvent(t *testing.T, st *sqliteStore, key session.Key) event.Event {
	t.Helper()
	var raw string
	if err := st.db.QueryRow(
		`SELECT event FROM session_events WHERE app_name=? AND user_id=? AND session_id=?`,
		key.AppName, key.UserID, key.SessionID).Scan(&raw); err != nil {
		t.Fatalf("读回落库事件: %v", err)
	}
	var evt event.Event
	if err := json.Unmarshal([]byte(raw), &evt); err != nil {
		t.Fatalf("落库 JSON 反序列化: %v", err)
	}
	return evt
}

// TestAppendEventDegradeThumbnail 验证 AppendEvent 落库降级钩子的端到端接线（方案 A）：
// 大图 dataURL 落库后替换为 jpeg 缩略图且原图不出现在库内；解码失败图整体丢弃但文本保留；
// 传入 evt 不被原地改写——原图必须在当轮内存中存活（SDK 请求/工具轮上下文用原图，
// 仅跨轮历史消费落库缩略图，见 shallowDowngradeEvent）。
func TestAppendEventDegradeThumbnail(t *testing.T) {
	ctx := context.Background()
	key := session.Key{AppName: "app", UserID: "user", SessionID: "s-thumb"}

	mustSession := func(st *sqliteStore) *session.Session {
		t.Helper()
		s, err := st.CreateSession(ctx, key, nil)
		if err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		return s
	}

	t.Run("大图落库缩略且内存evt保留原图", func(t *testing.T) {
		st := newRoundsTestStore(t)
		s := mustSession(st)
		original := mkImageDataURL(t, 800, 600, "png")
		evt := &event.Event{Version: event.CurrentVersion, FilterKey: "https://a.com", Response: &model.Response{
			Choices: []model.Choice{{Message: model.Message{
				Role:    model.RoleUser,
				Content: "看这张图",
				ContentParts: []model.ContentPart{
					{Type: model.ContentType("image"), Image: &model.Image{URL: original}},
				},
			}}},
		}}
		if err := st.AppendEvent(ctx, s, evt); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}

		if got := evt.Response.Choices[0].Message.ContentParts[0].Image.URL; got != original {
			t.Errorf("内存 evt 被原地改写，当轮请求将只见缩略图: %.60s", got)
		}

		stored := thumbStoredEvent(t, st, key)
		msg := stored.Response.Choices[0].Message
		if msg.Content != "看这张图" {
			t.Errorf("落库文本 = %q, want 看这张图", msg.Content)
		}
		if len(msg.ContentParts) != 1 {
			t.Fatalf("落库 parts = %d, want 1", len(msg.ContentParts))
		}
		thumb := msg.ContentParts[0].Image.URL
		if !strings.HasPrefix(thumb, "data:image/jpeg;base64,") {
			t.Errorf("落库图应替换为 jpeg 缩略: %.60s", thumb)
		}
		if w, h := thumbDims(t, thumb); w != 192 || h != 144 { // 800x600 等比 → 192x144
			t.Errorf("缩略 dims = %dx%d, want 192x144", w, h)
		}
	})

	t.Run("解码失败图丢弃文本保留", func(t *testing.T) {
		st := newRoundsTestStore(t)
		s := mustSession(st)
		evt := &event.Event{Version: event.CurrentVersion, FilterKey: "https://a.com", Response: &model.Response{
			Choices: []model.Choice{{Message: model.Message{
				Role:    model.RoleUser,
				Content: "坏图轮",
				ContentParts: []model.ContentPart{
					{Type: model.ContentType("image"), Image: &model.Image{URL: "data:image/webp;base64,AAAA"}},
				},
			}}},
		}}
		if err := st.AppendEvent(ctx, s, evt); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}

		stored := thumbStoredEvent(t, st, key)
		msg := stored.Response.Choices[0].Message
		if msg.Content != "坏图轮" {
			t.Errorf("文本应保留: %q", msg.Content)
		}
		if len(msg.ContentParts) != 0 {
			t.Errorf("解码失败图应丢弃，落库 parts = %d", len(msg.ContentParts))
		}
	})
}
