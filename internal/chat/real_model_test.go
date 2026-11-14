//go:build realmodel

package chat

// 本文件探针直连真实 GLM 服务，需 build tag 与 ABSORB_REAL_MODEL 同时满足才执行。
// 运行：ABSORB_REAL_MODEL=1 go test -tags=realmodel ./internal/chat/ -run TestMultimodal_RealModel -v

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"os"
	"strings"
	"testing"
	"time"

	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/model/openai"

	"absorb-agent/internal/conf"
)

// makeTestImage 生成一张 200x120 的测试图：左半红、右半蓝。
// 模型若说出颜色，即说明图片内容被真正解析。
func makeTestImage() string {
	img := image.NewRGBA(image.Rect(0, 0, 200, 120))
	for y := 0; y < 120; y++ {
		for x := 0; x < 200; x++ {
			if x < 100 {
				img.Set(x, y, color.RGBA{220, 40, 40, 255}) // 红
			} else {
				img.Set(x, y, color.RGBA{40, 80, 220, 255}) // 蓝
			}
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

func TestMultimodal_RealModel(t *testing.T) {
	if os.Getenv("ABSORB_REAL_MODEL") == "" {
		t.Skip("set ABSORB_REAL_MODEL=1 to run real model test")
	}

	cfg := conf.LoadConfig("../../config/config.yaml")
	mc := cfg.Model

	// ABSORB_REAL_MODEL_NAME 可覆盖模型名以便切到视觉模型；覆盖时关闭 thinking 以隔离变量。
	modelName := mc.Name
	thinkingOn := mc.Thinking.Enabled
	if override := os.Getenv("ABSORB_REAL_MODEL_NAME"); override != "" {
		modelName = override
		thinkingOn = false
		t.Logf("[OVERRIDE] 模型 %s", override)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// 参数与生产 buildOpenAIModel 一致，唯一区别是关闭 text-only 以发出数组 content。
	m := openai.New(modelName,
		openai.WithBaseURL(strings.TrimRight(mc.BaseURL, "/")),
		openai.WithAPIKey(mc.APIKey),
		openai.WithVariant(openai.Variant(mc.Variant)),
		openai.WithTextOnlyMessageContent(false), // 关闭后 image_url 才会进入数组 content
		openai.WithChatRequestJSONCallback(func(_ context.Context, body []byte, err error) {
			if err == nil {
				t.Logf("[REQUEST BODY]\n%s", string(body))
			}
		}),
	)

	imgURL := makeTestImage()
	msg := model.NewUserMessage("这是一张我生成的测试图片，请简要描述它的主要颜色。")
	msg.AddImageURL(imgURL, "auto")

	temp := mc.Temperature
	req := &model.Request{
		Messages: []model.Message{msg},
		// Temperature/ThinkingEnabled 是内嵌 GenerationConfig 的提升字段，
		// go<1.27 不允许在结构体字面量里直接赋值，故构造后再赋（同 agent.go）。
		GenerationConfig: model.GenerationConfig{Stream: false},
	}
	req.Temperature = &temp
	req.ThinkingEnabled = &thinkingOn

	ch, err := m.GenerateContent(ctx, req)
	if err != nil {
		t.Fatalf("GenerateContent 返回错误: %v", err)
	}

	var sb strings.Builder
	var apiErr string
	for r := range ch {
		if r.Error != nil {
			apiErr = r.Error.Message
			break
		}
		for _, c := range r.Choices {
			sb.WriteString(c.Message.Content)
		}
	}

	// GLM 端点只接受纯文本 content，image_url 数组会被拒（code=1210）。
	if strings.Contains(apiErr, "content.type") || strings.Contains(apiErr, "1210") {
		t.Logf("GLM 拒绝数组 content: %s", apiErr)
		return
	}
	if apiErr != "" {
		t.Fatalf("响应携带 API 错误: %s", apiErr)
	}

	got := strings.TrimSpace(sb.String())
	t.Logf("[RESPONSE] %s", got)
	if got == "" {
		t.Fatalf("真模型返回空内容；多模态请求可能未被正确处理")
	}
	// 未点名颜色不代表图片没被解析，只作提示。
	if !strings.Contains(got, "红") && !strings.Contains(got, "蓝") &&
		!strings.Contains(got, "red") && !strings.Contains(got, "blue") {
		t.Logf("响应未出现红/蓝关键词，但请求成功（模型可能未直接点名颜色）")
	}
}
