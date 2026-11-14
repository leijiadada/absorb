package kind

import "testing"

// TestProviderVariant 锁定“厂商 → 方言”映射，防止误改后静默破坏。
func TestProviderVariant(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		p    Provider
		want Variant
	}{
		{"bigmodel→glm", ProviderBigModel, VariantGLM},
		{"deepseek", ProviderDeepSeek, VariantDeepSeek},
		{"kimi", ProviderKimi, VariantKimi},
		{"minimax", ProviderMiniMax, VariantMiniMax},
		{"dashscope 无方言", ProviderDashScope, ""},
		{"siliconflow 无方言", ProviderSiliconFlow, ""},
		{"volces 无方言", ProviderVolces, ""},
		{"hunyuan 无方言", ProviderHunyuan, ""},
		{"openrouter 无方言", ProviderOpenRouter, ""},
		{"custom 无方言", ProviderCustom, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.p.Variant(); got != c.want {
				t.Errorf("Provider(%q).Variant() = %q, want %q", c.p, got, c.want)
			}
		})
	}
}

// TestProviderValid 确认已知厂商合法、未知厂商非法。
func TestProviderValid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		p    Provider
		want bool
	}{
		{"bigmodel", ProviderBigModel, true},
		{"deepseek", ProviderDeepSeek, true},
		{"kimi", ProviderKimi, true},
		{"dashscope", ProviderDashScope, true},
		{"siliconflow", ProviderSiliconFlow, true},
		{"volces", ProviderVolces, true},
		{"hunyuan", ProviderHunyuan, true},
		{"minimax", ProviderMiniMax, true},
		{"openrouter", ProviderOpenRouter, true},
		{"custom", ProviderCustom, true},
		{"未知厂商非法", Provider("bogus"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.p.Valid(); got != c.want {
				t.Errorf("Provider(%q).Valid() = %v, want %v", c.p, got, c.want)
			}
		})
	}
}

// TestVariantValid 确认已知方言（含空串=自动推断）合法、未知方言非法。
func TestVariantValid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		v    Variant
		want bool
	}{
		{"空串-自动推断", "", true},
		{"glm", VariantGLM, true},
		{"deepseek", VariantDeepSeek, true},
		{"kimi", VariantKimi, true},
		{"minimax", VariantMiniMax, true},
		{"qwen", VariantQwen, true},
		{"未知方言非法", Variant("bogus"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.v.Valid(); got != c.want {
				t.Errorf("Variant(%q).Valid() = %v, want %v", c.v, got, c.want)
			}
		})
	}
}
