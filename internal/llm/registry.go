package llm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"aiops/internal/cryptox"
	"aiops/internal/store"
)

// ErrNoProvider 表示供应商不存在。
var ErrNoProvider = errors.New("llm: provider not found")

type cachedClient struct {
	version  int64
	client   Client
	provider store.Provider
}

// Registry 按 providerID + updated_at 缓存客户端。缓存只在内存中持有解密后的
// Key，不落盘、不写日志。
type Registry struct {
	st     store.Store
	cipher *cryptox.Cipher
	http   *http.Client
	log    *slog.Logger

	mu    sync.Mutex
	cache map[string]cachedClient
}

// NewRegistry 构造 Registry。
func NewRegistry(st store.Store, cipher *cryptox.Cipher, httpClient *http.Client, log *slog.Logger) *Registry {
	if httpClient == nil {
		httpClient = defaultClient()
	}
	return &Registry{
		st:     st,
		cipher: cipher,
		http:   httpClient,
		log:    log,
		cache:  make(map[string]cachedClient),
	}
}

// Client 返回供应商客户端与其元数据。
func (r *Registry) Client(ctx context.Context, providerID string) (Client, store.Provider, error) {
	if r == nil || r.st == nil {
		return nil, store.Provider{}, fmt.Errorf("%w: store is not configured", ErrNoProvider)
	}
	if providerID == "" {
		return nil, store.Provider{}, ErrNoProvider
	}
	provider, err := r.st.Providers().Get(ctx, providerID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, store.Provider{}, ErrNoProvider
		}
		return nil, store.Provider{}, fmt.Errorf("llm: load provider: %w", err)
	}

	version := provider.UpdatedAt.UnixNano()
	r.mu.Lock()
	if entry, ok := r.cache[providerID]; ok && entry.version == version {
		client := entry.client
		r.mu.Unlock()
		return client, provider, nil
	}
	r.mu.Unlock()

	apiKey := ""
	if provider.APIKeyEncrypted != "" {
		if r.cipher == nil {
			return nil, store.Provider{}, fmt.Errorf("%w: cipher is not configured", ErrInvalidConfig)
		}
		apiKey, err = r.cipher.Decrypt(provider.APIKeyEncrypted)
		if err != nil {
			return nil, store.Provider{}, fmt.Errorf("%w: cannot decrypt api key", ErrInvalidConfig)
		}
	}

	cfg := ProviderConfig{
		ID:           provider.ID,
		Name:         provider.Name,
		Kind:         provider.Kind,
		BaseURL:      provider.BaseURL,
		APIKey:       apiKey,
		DefaultModel: provider.DefaultModel,
	}
	var client Client
	switch provider.Kind {
	case store.ProviderOpenAICompatible:
		client, err = NewOpenAI(cfg, r.http)
	case store.ProviderAnthropic:
		client, err = NewAnthropic(cfg, r.http)
	default:
		return nil, store.Provider{}, fmt.Errorf("%w: unsupported provider kind", ErrInvalidConfig)
	}
	if err != nil {
		return nil, store.Provider{}, err
	}

	r.mu.Lock()
	r.cache[providerID] = cachedClient{version: version, client: client, provider: provider}
	r.mu.Unlock()
	if r.log != nil {
		r.log.Debug("llm_client_cached", "provider_id", provider.ID, "kind", string(provider.Kind))
	}
	return client, provider, nil
}

// Test 测试供应商连通性：GET /models，返延迟与模型数。
func (r *Registry) Test(ctx context.Context, providerID string) (TestResult, error) {
	client, _, err := r.Client(ctx, providerID)
	if err != nil {
		return TestResult{}, err
	}
	start := time.Now()
	models, err := client.ListModels(ctx)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return TestResult{}, err
	}
	return TestResult{LatencyMS: latency, ModelCount: len(models)}, nil
}

// FetchModels 拉取模型列表（排序去重）。
func (r *Registry) FetchModels(ctx context.Context, providerID string) ([]string, error) {
	client, _, err := r.Client(ctx, providerID)
	if err != nil {
		return nil, err
	}
	models, err := client.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	return sortUnique(models), nil
}

// Invalidate 清除供应商缓存。
func (r *Registry) Invalidate(providerID string) {
	r.mu.Lock()
	delete(r.cache, providerID)
	r.mu.Unlock()
}

// Close 释放缓存（不关闭由 main 持有的 http.Client）。
func (r *Registry) Close() {
	r.mu.Lock()
	r.cache = make(map[string]cachedClient)
	r.mu.Unlock()
}
