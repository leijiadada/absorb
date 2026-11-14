package store

// AppendEvent 落库缩略图降级（方案 A）集成测试：事件经 SessionService.AppendEvent
// 落库后，从 DB 读回的事件中 image part 应为 192px JPEG 缩略（<10KB），外链图原样
// 保留，解码失败 part 丢弃，文本 part 不受影响。缩放函数本身的单测见
// internal/store/sqlite/thumbnail_test.go；本文件验证「落库链路 + 读回形态」。

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"strings"
	"testing"

	"trpc.group/trpc-go/trpc-agent-go/event"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/session"
)

// mkTestImage 生成 w×h JPEG dataURL（渐变内容，真实可解码）。
func mkTestImage(t *testing.T, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{R: uint8(x * 255 / w), G: 64, B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode test image: %v", err)
	}
	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

// mkImageEvent 构造一条 user 消息事件：文本 + 若干 image part（URL 直传）。
func mkImageEvent(content string, imageURLs ...string) *event.Event {
	finish := "stop"
	parts := make([]model.ContentPart, 0, len(imageURLs)+1)
	parts = append(parts, model.ContentPart{Type: model.ContentType("text"), Text: &content})
	for _, u := range imageURLs {
		parts = append(parts, model.ContentPart{Type: model.ContentType("image"),
			Image: &model.Image{URL: u, Detail: "auto"}})
	}
	return &event.Event{
		Response: &model.Response{
			ID: "resp", Object: "chat.completion", Model: "test",
			Choices: []model.Choice{{
				Index: 0,
				Message: model.Message{
					Role: model.RoleUser, Content: content, ContentParts: parts,
				},
				FinishReason: &finish,
			}},
		},
	}
}

func TestAppendEventDownscaleImages(t *testing.T) {
	svc, _ := newTestService(t)
	defer svc.Close()
	ctx := context.Background()
	key := session.Key{AppName: "absorb", UserID: "u1", SessionID: "img1"}

	if _, err := svc.CreateSession(ctx, key, nil); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	sess, err := svc.GetSession(ctx, key)
	if err != nil || sess == nil {
		t.Fatalf("GetSession: %v, %v", sess, err)
	}

	big := mkTestImage(t, 1280, 800) // 真实可解码大图（q90 下远超 10KB）
	evt := mkImageEvent("看这张图",
		big,                           // dataURL 大图 → 应降为缩略图
		"https://example.com/a.png",   // 外链 → 原样保留
		"data:image/webp;base64,AAAA", // 标准库不支持格式 → 丢弃
	)
	if err := svc.AppendEvent(ctx, sess, evt); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	// 重新 GetSession 从 DB 加载（非内存 sess），断言落库形态
	got, err := svc.GetSession(ctx, key)
	if err != nil {
		t.Fatalf("GetSession after append: %v", err)
	}
	// 落库副本降级不得污染内存 sess：AppendEvent 后传入 sess 中的 user 事件
	// 应保持原图（当轮模型请求与工具轮上下文可见原图，仅跨轮历史用落库缩略图）
	var memImg string
	for _, e := range sess.Events {
		if e.Response == nil || len(e.Response.Choices) == 0 {
			continue
		}
		if m := &e.Response.Choices[0].Message; m.Role == model.RoleUser {
			for _, p := range m.ContentParts {
				if p.Type == model.ContentType("image") && p.Image != nil && len(p.Image.URL) > len(memImg) {
					memImg = p.Image.URL // 取最长 dataURL（原图必然长于缩略）
				}
			}
		}
	}
	if memImg != big {
		t.Fatalf("内存 sess 的 user 事件应保持原图（当轮可见），实际 %.40s（len=%d，原图 len=%d）",
			memImg, len(memImg), len(big))
	}
	var userMsg *model.Message
	for _, e := range got.Events {
		if e.Response == nil || len(e.Response.Choices) == 0 {
			continue
		}
		if m := &e.Response.Choices[0].Message; m.Role == model.RoleUser {
			userMsg = m // 取最后一条 user 消息
		}
	}
	if userMsg == nil {
		t.Fatal("读回事件中未找到 user 消息")
	}

	var keptImages, externalLinks int
	for _, p := range userMsg.ContentParts {
		switch {
		case p.Type == model.ContentType("text"):
			if p.Text == nil || *p.Text != "看这张图" {
				t.Errorf("文本 part 应不受影响: %+v", p.Text)
			}
		case p.Type == model.ContentType("image"):
			if strings.HasPrefix(p.Image.URL, "http") {
				externalLinks++
				if p.Image.URL != "https://example.com/a.png" {
					t.Errorf("外链 URL 应原样: %q", p.Image.URL)
				}
				continue
			}
			keptImages++
			if !strings.HasPrefix(p.Image.URL, "data:image/jpeg;base64,") {
				t.Errorf("dataURL 图应降为 jpeg 缩略: %.40s", p.Image.URL)
			}
			if n := len(p.Image.URL); n >= 10*1024 {
				t.Errorf("缩略图应 <10KB，实际 %d", n)
			}
			if n := len(p.Image.URL); n >= len(big) {
				t.Errorf("缩略图应小于原图: 缩略 %d >= 原 %d", n, len(big))
			}
		default:
			t.Errorf("意外 part 类型: %v", p.Type)
		}
	}
	if keptImages != 1 {
		t.Errorf("应保留 1 个缩略 image part，实际 %d", keptImages)
	}
	if externalLinks != 1 {
		t.Errorf("应保留 1 个外链 part，实际 %d", externalLinks)
	}
}
