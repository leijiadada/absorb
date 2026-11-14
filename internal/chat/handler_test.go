package chat

import (
	"testing"
)

// TestImageSource 校验「URL 优先、否则回退 DataURL」的来源解析。
func TestImageSource(t *testing.T) {
	if got := imageSource(ImageInput{URL: "https://cdn.bigmodel.cn/a.png"}); got != "https://cdn.bigmodel.cn/a.png" {
		t.Fatalf("URL 应优先: got %q", got)
	}
	if got := imageSource(ImageInput{DataURL: "data:image/png;base64,AAA"}); got != "data:image/png;base64,AAA" {
		t.Fatalf("无 URL 时应回退 DataURL: got %q", got)
	}
}

// TestValidateImage 覆盖 base64 与图片 URL 两种形态的校验分支。
func TestValidateImage(t *testing.T) {
	big := make([]byte, 21*1024*1024) // 超过 20MB
	cases := []struct {
		name    string
		in      ImageInput
		wantErr bool
	}{
		{"公网 https 链接放行", ImageInput{URL: "https://cdn.bigmodel.cn/a.png"}, false},
		{"公网 http 链接放行", ImageInput{URL: "http://example.com/a.jpg"}, false},
		{"data scheme 被拒", ImageInput{URL: "data:image/png;base64,AAA"}, true},
		{"javascript scheme 被拒", ImageInput{URL: "javascript:alert(1)"}, true},
		{"base64 合法 mime", ImageInput{DataURL: "data:image/png;base64,AAA", Mime: "image/png"}, false},
		{"base64 非法 mime", ImageInput{DataURL: "x", Mime: "image/gif"}, true},
		{"base64 超大", ImageInput{DataURL: string(big), Mime: "image/png"}, true},
		{"空图片报错", ImageInput{}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateImage(c.in)
			if (err != nil) != c.wantErr {
				t.Fatalf("validateImage(%+v) = %v, wantErr=%v", c.in, err, c.wantErr)
			}
		})
	}
}
