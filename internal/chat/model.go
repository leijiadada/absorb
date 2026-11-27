package chat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/model/openai"

	fetch "absorb-agent/tool/fetch_page"

	"absorb-agent/internal/errs"
	"absorb-agent/internal/kind"
	"absorb-agent/internal/log"
	"absorb-agent/internal/store"
)

// errNoModel 尚未配置任何模型时的统一错误（问答/蒸馏均会收到）。
var errNoModel = errors.New("还没有可用模型，请先在桌面端“模型”页添加并设为默认")

// providerVariant 查服务商对应的 openai 协议变体（空=由框架按 base_url 自动推断）。
// 映射集中维护在 kind.Provider.Variant，避免散落多份真相源。
// 变体影响思考字段格式等厂商差异（如 GLM 的嵌套 thinking 结构）。
func providerVariant(provider string) string {
	return string(kind.Provider(provider).Variant())
}

// effectiveBaseURL 计算传给 openai 客户端的 base URL：
// 完整 URL 模式剥掉尾部 /chat/completions（客户端会自动补回），统一去尾斜杠。
func effectiveBaseURL(mc *store.ModelConfig) string {
	base := strings.TrimRight(strings.TrimSpace(mc.BaseURL), "/")
	if mc.FullURL {
		base = strings.TrimSuffix(base, "/chat/completions")
	}
	return base
}

// buildOpenAIModel 按模型配置构建 openai 兼容客户端。
// proxyFn 每次请求实时返回全局代理地址（空=跟随系统代理），与 fetch 共用同一解析逻辑，
// 保证配置保存后模型 API 调用也即时走代理，无需重启。
func buildOpenAIModel(mc *store.ModelConfig, proxyFn func() string) model.Model {
	opts := []openai.Option{
		openai.WithBaseURL(effectiveBaseURL(mc)),
		openai.WithAPIKey(mc.APIKey),
		// 按模型视觉能力决定是否强制字符串 content：ImageSupport=false 时剥离图片 part。
		// GLM 默认被标为支持图片（元数据未修正），此处放行，由端点返回 1210 报错。
		openai.WithTextOnlyMessageContent(!mc.ImageSupport),
		// 模型窗口最终防线：请求超窗口预算时自动裁剪（保留 system 与最后一轮，中段轮次优先删）。
		// 未知模型名按框架默认 128K 窗口推算预算，正常请求不受影响；估算口径对齐中文
		// （CJK 约 1.6 rune/token），与 L1 层启发式一致。L1 业务预算超限由工作集层自行降级。
		openai.WithEnableTokenTailoring(true),
		openai.WithTailoringStrategy(model.NewMiddleOutStrategy(
			model.NewSimpleTokenCounter(model.WithApproxRunesPerToken(1.6)),
		)),
		openai.WithChatRequestJSONCallback(func(ctx context.Context, body []byte, err error) {
			if err != nil {
				log.Errorf("[Absorb] request serialization error: %v", err)
				return
			}
			log.Debugf("[Absorb] request body sent to model:\n%s", string(body))
		}),
	}
	v := providerVariant(mc.Provider)
	if v != "" {
		opts = append(opts, openai.WithVariant(openai.Variant(v)))
		opts = append(opts, variantOptions(kind.Variant(v))...)
	}
	if proxyFn != nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.Proxy = fetch.ProxyResolver(proxyFn)
		opts = append(opts, openai.WithHTTPClientOptions(openai.WithHTTPClientTransport(tr)))
	}
	return openai.New(mc.ModelID, opts...)
}

// DynamicModel 按默认模型配置动态选择底层模型的包装器：
// 每次调用读取 default_model_id → 缓存命中直接复用，未命中构建并缓存；
// cacheKey 覆盖 model_id/base_url/key/变体，切换默认或编辑配置自动走新客户端，无需重启。
// 问答 runner、会话摘要、画像蒸馏、知识草稿共用同一实例（main.go 装配）。
type DynamicModel struct {
	sess    *store.SessionService
	proxyFn func() string
	mu      sync.Mutex
	cache   map[string]model.Model
}

func NewDynamicModel(sess *store.SessionService, proxyFn func() string) *DynamicModel {
	return &DynamicModel{sess: sess, proxyFn: proxyFn, cache: map[string]model.Model{}}
}

func (d *DynamicModel) Info() model.Info {
	m, _, err := d.Current(context.Background())
	if err != nil || m == nil {
		return model.Info{Name: ""}
	}
	return m.Info()
}

func (d *DynamicModel) GenerateContent(ctx context.Context, req *model.Request) (<-chan *model.Response, error) {
	m, mc, err := d.Current(ctx)
	if err != nil {
		return nil, err
	}
	applyModelGenConfig(req, mc)
	return m.GenerateContent(ctx, req)
}

// Current 解析当前默认模型：返回底层模型客户端（未经聊天参数包装，草稿等
// 特殊链路据此绕开 applyModelGenConfig）与其配置。
func (d *DynamicModel) Current(ctx context.Context) (model.Model, *store.ModelConfig, error) {
	mc, err := d.sess.GetDefaultModelConfig(ctx, AppName)
	if err != nil {
		return nil, nil, fmt.Errorf("读取默认模型配置失败: %w", err)
	}
	if mc == nil {
		return nil, nil, errNoModel
	}
	key := mc.ModelID + "|" + mc.APIKey + "|" + providerVariant(mc.Provider) + "|" + effectiveBaseURL(mc) + "|" + strconv.FormatBool(mc.ImageSupport)
	d.mu.Lock()
	defer d.mu.Unlock()
	if m, ok := d.cache[key]; ok {
		return m, mc, nil
	}
	m := buildOpenAIModel(mc, d.proxyFn)
	d.cache[key] = m
	return m, mc, nil
}

// applyModelGenConfig 把 per-model 生成参数覆盖到请求上；
// nil 值清空对应字段（跟随模型侧默认）。思考模式三态：
// 0=跟随默认（清空 ThinkingEnabled，不向请求下发思考参数）、1=开启、2=关闭。
func applyModelGenConfig(req *model.Request, mc *store.ModelConfig) {
	if req == nil {
		return
	}
	req.Temperature = mc.Temperature
	req.MaxTokens = mc.MaxTokens
	switch mc.Thinking {
	case 1:
		on := true
		req.ThinkingEnabled = &on
	case 2:
		off := false
		req.ThinkingEnabled = &off
	default:
		req.ThinkingEnabled = nil
	}
	req.ThinkingTokens = nil
}

// TestResult 连通性测试结果（App 绑定层返回给前端）。
type TestResult struct {
	OK        bool   `json:"ok"`
	LatencyMs int64  `json:"latencyMs"`
	Error     string `json:"error"`
}

// TestModelConn 用表单当前值发一次真实请求（max_tokens=1、思考关闭、非流式），
// 返回耗时或人话错误。proxyFn 复用全局代理解析（同问答链路）。
func TestModelConn(mc *store.ModelConfig, proxyFn func() string) TestResult {
	m := buildOpenAIModel(mc, proxyFn)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	mcCtx := errs.ModelContext{Provider: mc.Provider, ModelID: mc.ModelID, DisplayName: mc.DisplayName}
	thinkingOff := false
	maxTokens := 1
	req := &model.Request{
		Messages: []model.Message{{Role: model.Role("user"), Content: "ping"}},
		GenerationConfig: model.GenerationConfig{
			Stream:          false,
			MaxTokens:       &maxTokens,
			ThinkingEnabled: &thinkingOff,
		},
	}
	start := time.Now()
	ch, err := m.GenerateContent(ctx, req)
	if err != nil {
		ne := errs.FromError(err, mcCtx)
		log.Errorf("[Absorb] model test failed | provider=%s model=%s base_url=%s code=%s cause=%v",
			mc.Provider, mc.ModelID, mc.BaseURL, ne.Code, ne.Cause)
		return TestResult{Error: ne.Message}
	}
	done := false
	for resp := range ch {
		if resp == nil {
			continue
		}
		if resp.Error != nil {
			ne := errs.FromModel(resp.Error, mcCtx)
			log.Errorf("[Absorb] model test failed | provider=%s model=%s base_url=%s code=%s cause=%v",
				mc.Provider, mc.ModelID, mc.BaseURL, ne.Code, ne.Cause)
			return TestResult{LatencyMs: elapsedMs(start), Error: ne.Message}
		}
		if resp.Done {
			done = true
			break
		}
	}
	// 通道关闭仍未见到 Done：ctx 超时或异常中断，不能当成功返回
	if !done {
		if ctx.Err() != nil {
			return TestResult{LatencyMs: elapsedMs(start), Error: "连接超时，请检查网络或代理后重试"}
		}
		return TestResult{LatencyMs: elapsedMs(start), Error: "服务未返回完整响应"}
	}
	return TestResult{OK: true, LatencyMs: elapsedMs(start)}
}

func elapsedMs(start time.Time) int64 {
	return time.Since(start).Round(time.Millisecond).Milliseconds()
}
