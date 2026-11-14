package upgrade

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// FeedFetcher 拉取并解析 feed。默认实现 HTTPFetcher；测试可注入假实现。
type FeedFetcher interface {
	Fetch(ctx context.Context, url string) (Release, error)
}

// HTTPFetcher 通过 HTTPS 获取 latest.json。未知字段忽略（feed 前向兼容），
// 必填字段由 Release.validate 兜底。
type HTTPFetcher struct {
	Client *http.Client // nil = 默认 30s 超时客户端
}

// NewHTTPFetcher 构造默认 fetcher（调用方可替换 Client 以接入系统代理）。
func NewHTTPFetcher(timeout time.Duration) *HTTPFetcher {
	return &HTTPFetcher{Client: &http.Client{Timeout: timeout}}
}

func (f *HTTPFetcher) client() *http.Client {
	if f.Client != nil {
		return f.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (f *HTTPFetcher) Fetch(ctx context.Context, url string) (Release, error) {
	if url == "" {
		return Release{}, ErrNoFeedURL
	}
	if !isHTTPS(url) {
		return Release{}, fmt.Errorf("%w: %s", ErrInsecureURL, url)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Release{}, fmt.Errorf("%w: %v", ErrBadFeed, err)
	}
	resp, err := f.client().Do(req)
	if err != nil {
		return Release{}, WithStatus("net_fail", 0, fmt.Errorf("fetch feed: %w", err))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Release{}, WithStatus("feed_unavailable", resp.StatusCode, fmt.Errorf("%w: unexpected status %s", ErrBadFeed, resp.Status))
	}

	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return Release{}, fmt.Errorf("%w: %v", ErrBadFeed, err)
	}
	if err := rel.validate(); err != nil {
		return Release{}, err
	}
	return rel, nil
}
