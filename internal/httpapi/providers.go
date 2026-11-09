package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"aiops/internal/llm"
	"aiops/internal/store"
)

// providerCreateRequest 对应 OpenAPI ProviderCreate。
type providerCreateRequest struct {
	Name         string   `json:"name"`
	Preset       string   `json:"preset"`
	Kind         string   `json:"kind"`
	BaseURL      string   `json:"base_url"`
	APIKey       *string  `json:"api_key"`
	DefaultModel string   `json:"default_model"`
	Models       []string `json:"models"`
}

// providerUpdateRequest 对应 OpenAPI ProviderUpdate。
type providerUpdateRequest struct {
	Name         *string   `json:"name"`
	Preset       *string   `json:"preset"`
	Kind         *string   `json:"kind"`
	BaseURL      *string   `json:"base_url"`
	APIKey       *string   `json:"api_key"`
	ClearAPIKey  bool      `json:"clear_api_key"`
	DefaultModel *string   `json:"default_model"`
	Models       *[]string `json:"models"`
}

// handleProviderPresets 返回内置预设。
func (d *Deps) handleProviderPresets(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"items": llm.Presets()})
}

// handleListProviders 返回供应商列表。
func (d *Deps) handleListProviders(c *gin.Context) {
	if d.Store == nil {
		writeError(c, "SERVICE_UNAVAILABLE", "storage is not available")
		return
	}
	providers, err := d.Store.Providers().List(c.Request.Context())
	if err != nil {
		writeError(c, "INTERNAL", "failed to list providers")
		return
	}
	items := make([]store.Provider, 0, len(providers))
	for _, p := range providers {
		items = append(items, maskProvider(p))
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

// handleCreateProvider 新增供应商；api_key 只写不读。
func (d *Deps) handleCreateProvider(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
	var req providerCreateRequest
	if !decodeJSONBody(c, &req) {
		return
	}
	if d.Store == nil {
		writeError(c, "SERVICE_UNAVAILABLE", "storage is not available")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || utf8.RuneCountInString(name) > 100 {
		writeError(c, "VALIDATION_FAILED", "name must be 1..100 characters")
		return
	}
	presetValue, ok := parsePreset(req.Preset)
	if !ok {
		writeError(c, "VALIDATION_FAILED", "unknown preset")
		return
	}
	preset, _ := llm.PresetByValue(presetValue)
	kindValue, ok := parseKind(req.Kind)
	if !ok || kindValue != preset.Kind {
		writeError(c, "VALIDATION_FAILED", "preset and kind do not match")
		return
	}
	baseURL, ok := normalizeProviderBaseURL(req.BaseURL)
	if !ok {
		writeError(c, "VALIDATION_FAILED", "base_url must be an absolute http(s) URL")
		return
	}
	defaultModel := strings.TrimSpace(req.DefaultModel)
	if defaultModel == "" || utf8.RuneCountInString(defaultModel) > 255 {
		writeError(c, "VALIDATION_FAILED", "default_model must be 1..255 characters")
		return
	}
	models, ok := normalizeModels(req.Models)
	if !ok {
		writeError(c, "VALIDATION_FAILED", "models are invalid")
		return
	}

	keyProvided := req.APIKey != nil && *req.APIKey != ""
	if preset.APIKeyRequired && !keyProvided {
		writeError(c, "VALIDATION_FAILED", "api_key is required for this preset")
		return
	}
	if req.APIKey != nil && len(*req.APIKey) > 8192 {
		writeError(c, "VALIDATION_FAILED", "api_key is too long")
		return
	}

	var key *store.ProviderAPIKey
	if req.APIKey != nil && *req.APIKey != "" {
		encrypted, ok := d.encryptProviderKey(c, *req.APIKey)
		if !ok {
			return
		}
		key = &store.ProviderAPIKey{Encrypted: encrypted, Hint: lastRunes(*req.APIKey, 4)}
	}

	provider, err := d.Store.Providers().Create(c.Request.Context(), store.CreateProviderParams{
		Name:         name,
		Preset:       presetValue,
		Kind:         kindValue,
		BaseURL:      baseURL,
		APIKey:       key,
		DefaultModel: defaultModel,
		Models:       models,
	})
	if err != nil {
		d.writeProviderStoreError(c, err)
		return
	}
	if d.LLM != nil {
		d.LLM.Invalidate(provider.ID)
	}
	d.enqueueAudit(c.Request.Context(), c, "provider.create", "provider", provider.ID, nil, nil, map[string]any{
		"name": provider.Name,
	})
	c.JSON(http.StatusCreated, maskProvider(provider))
}

// handleGetProvider 返回供应商详情。
func (d *Deps) handleGetProvider(c *gin.Context) {
	if d.Store == nil {
		writeError(c, "SERVICE_UNAVAILABLE", "storage is not available")
		return
	}
	provider, err := d.Store.Providers().Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		d.writeProviderStoreError(c, err)
		return
	}
	c.JSON(http.StatusOK, maskProvider(provider))
}

// handleUpdateProvider 更新供应商；省略字段表示不变。
func (d *Deps) handleUpdateProvider(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
	var req providerUpdateRequest
	if !decodeJSONBody(c, &req) {
		return
	}
	if req.Name == nil && req.Preset == nil && req.Kind == nil && req.BaseURL == nil &&
		req.APIKey == nil && !req.ClearAPIKey && req.DefaultModel == nil && req.Models == nil {
		writeError(c, "VALIDATION_FAILED", "at least one field is required")
		return
	}
	if req.APIKey != nil && req.ClearAPIKey {
		writeError(c, "VALIDATION_FAILED", "api_key and clear_api_key cannot both be set")
		return
	}
	if d.Store == nil {
		writeError(c, "SERVICE_UNAVAILABLE", "storage is not available")
		return
	}

	ctx := c.Request.Context()
	existing, err := d.Store.Providers().Get(ctx, c.Param("id"))
	if err != nil {
		d.writeProviderStoreError(c, err)
		return
	}

	params := store.UpdateProviderParams{}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" || utf8.RuneCountInString(name) > 100 {
			writeError(c, "VALIDATION_FAILED", "name must be 1..100 characters")
			return
		}
		params.Name = &name
	}
	if req.Preset != nil || req.Kind != nil {
		presetValue := existing.Preset
		if req.Preset != nil {
			p, ok := parsePreset(*req.Preset)
			if !ok {
				writeError(c, "VALIDATION_FAILED", "unknown preset")
				return
			}
			presetValue = p
		}
		kindValue := existing.Kind
		if req.Kind != nil {
			k, ok := parseKind(*req.Kind)
			if !ok {
				writeError(c, "VALIDATION_FAILED", "unknown provider kind")
				return
			}
			kindValue = k
		}
		preset, _ := llm.PresetByValue(presetValue)
		if preset.Kind != kindValue {
			writeError(c, "VALIDATION_FAILED", "preset and kind do not match")
			return
		}
		params.Preset = &presetValue
		params.Kind = &kindValue
	}
	if req.BaseURL != nil {
		baseURL, ok := normalizeProviderBaseURL(*req.BaseURL)
		if !ok {
			writeError(c, "VALIDATION_FAILED", "base_url must be an absolute http(s) URL")
			return
		}
		params.BaseURL = &baseURL
	}
	if req.DefaultModel != nil {
		model := strings.TrimSpace(*req.DefaultModel)
		if model == "" || utf8.RuneCountInString(model) > 255 {
			writeError(c, "VALIDATION_FAILED", "default_model must be 1..255 characters")
			return
		}
		params.DefaultModel = &model
	}
	if req.Models != nil {
		models, ok := normalizeModels(*req.Models)
		if !ok {
			writeError(c, "VALIDATION_FAILED", "models are invalid")
			return
		}
		params.Models = &models
	}
	switch {
	case req.ClearAPIKey:
		params.APIKey = &store.ProviderAPIKey{}
	case req.APIKey != nil:
		if *req.APIKey == "" {
			params.APIKey = &store.ProviderAPIKey{}
		} else {
			if len(*req.APIKey) > 8192 {
				writeError(c, "VALIDATION_FAILED", "api_key is too long")
				return
			}
			encrypted, ok := d.encryptProviderKey(c, *req.APIKey)
			if !ok {
				return
			}
			params.APIKey = &store.ProviderAPIKey{Encrypted: encrypted, Hint: lastRunes(*req.APIKey, 4)}
		}
	}

	provider, err := d.Store.Providers().Update(ctx, existing.ID, params)
	if err != nil {
		d.writeProviderStoreError(c, err)
		return
	}
	if d.LLM != nil {
		d.LLM.Invalidate(provider.ID)
	}
	d.enqueueAudit(ctx, c, "provider.update", "provider", provider.ID, nil, nil, map[string]any{
		"name": provider.Name,
	})
	c.JSON(http.StatusOK, maskProvider(provider))
}

// handleDeleteProvider 删除供应商并失效客户端缓存。
func (d *Deps) handleDeleteProvider(c *gin.Context) {
	if d.Store == nil {
		writeError(c, "SERVICE_UNAVAILABLE", "storage is not available")
		return
	}
	id := c.Param("id")
	if err := d.Store.Providers().Delete(c.Request.Context(), id); err != nil {
		d.writeProviderStoreError(c, err)
		return
	}
	if d.LLM != nil {
		d.LLM.Invalidate(id)
	}
	d.enqueueAudit(c.Request.Context(), c, "provider.delete", "provider", id, nil, nil, nil)
	c.Status(http.StatusNoContent)
}

// handleTestProvider 调用 /models 验证连通性。
func (d *Deps) handleTestProvider(c *gin.Context) {
	if d.LLM == nil {
		writeError(c, "INTERNAL", "llm registry is not configured")
		return
	}
	ctx := c.Request.Context()
	id := c.Param("id")
	result, err := d.LLM.Test(ctx, id)
	if err != nil {
		d.writeProviderUpstreamError(c, err, "PROVIDER_TEST_FAILED")
		return
	}
	d.enqueueAudit(ctx, c, "provider.test", "provider", id, nil, nil, map[string]any{
		"latency_ms": result.LatencyMS, "model_count": result.ModelCount,
	})
	c.JSON(http.StatusOK, gin.H{
		"ok":          true,
		"latency_ms":  result.LatencyMS,
		"model_count": result.ModelCount,
	})
}

// handleRefreshProviderModels 拉取并保存模型列表。
func (d *Deps) handleRefreshProviderModels(c *gin.Context) {
	if d.LLM == nil {
		writeError(c, "INTERNAL", "llm registry is not configured")
		return
	}
	if d.Store == nil {
		writeError(c, "SERVICE_UNAVAILABLE", "storage is not available")
		return
	}
	ctx := c.Request.Context()
	id := c.Param("id")
	models, err := d.LLM.FetchModels(ctx, id)
	if err != nil {
		d.writeProviderUpstreamError(c, err, "PROVIDER_MODELS_FAILED")
		return
	}
	if models == nil {
		models = []string{}
	}
	if _, err := d.Store.Providers().Update(ctx, id, store.UpdateProviderParams{Models: &models}); err != nil {
		d.writeProviderStoreError(c, err)
		return
	}
	d.LLM.Invalidate(id)
	d.enqueueAudit(ctx, c, "provider.models_refresh", "provider", id, nil, nil, map[string]any{
		"model_count": len(models),
	})
	now := timeNow(d)
	c.JSON(http.StatusOK, gin.H{"models": models, "refreshed_at": now})
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

func parsePreset(raw string) (store.ProviderPreset, bool) {
	v := store.ProviderPreset(strings.TrimSpace(raw))
	_, ok := llm.PresetByValue(v)
	return v, ok
}

func parseKind(raw string) (store.ProviderKind, bool) {
	switch store.ProviderKind(strings.TrimSpace(raw)) {
	case store.ProviderOpenAICompatible:
		return store.ProviderOpenAICompatible, true
	case store.ProviderAnthropic:
		return store.ProviderAnthropic, true
	default:
		return "", false
	}
}

func normalizeProviderBaseURL(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 2048 {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", false
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}
	return strings.TrimRight(raw, "/"), true
}

func normalizeModels(in []string) ([]string, bool) {
	if len(in) > 500 {
		return nil, false
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, m := range in {
		m = strings.TrimSpace(m)
		if m == "" || utf8.RuneCountInString(m) > 255 {
			return nil, false
		}
		if _, ok := seen[m]; ok {
			continue
		}
		seen[m] = struct{}{}
		out = append(out, m)
	}
	return out, true
}

func lastRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) < n {
		return ""
	}
	return string(runes[len(runes)-n:])
}

func maskProvider(p store.Provider) store.Provider {
	if p.Models == nil {
		p.Models = []string{}
	}
	if !p.HasAPIKey {
		p.APIKeyMasked = ""
		return p
	}
	if p.APIKeyMasked == "" {
		if p.APIKeyHint != "" {
			p.APIKeyMasked = "****" + p.APIKeyHint
		} else {
			p.APIKeyMasked = "****"
		}
	}
	return p
}

func (d *Deps) encryptProviderKey(c *gin.Context, key string) (string, bool) {
	if d.Cipher == nil {
		writeError(c, "INTERNAL", "credential cipher is not configured")
		return "", false
	}
	encrypted, err := d.Cipher.Encrypt(key)
	if err != nil {
		writeError(c, "INTERNAL", "failed to encrypt api key")
		return "", false
	}
	return encrypted, true
}

func (d *Deps) writeProviderStoreError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(c, "PROVIDER_NOT_FOUND", "provider not found")
	case errors.Is(err, store.ErrConflict):
		writeError(c, "CONFLICT", "provider name already exists")
	case errors.Is(err, store.ErrInvalid):
		writeError(c, "VALIDATION_FAILED", "provider is invalid")
	default:
		writeError(c, "INTERNAL", "provider operation failed")
	}
}

func (d *Deps) writeProviderUpstreamError(c *gin.Context, err error, fallback string) {
	switch {
	case errors.Is(err, llm.ErrNoProvider):
		writeError(c, "PROVIDER_NOT_FOUND", "provider not found")
	case errors.Is(err, llm.ErrTimeout), errors.Is(err, context.DeadlineExceeded):
		writeError(c, "LLM_TIMEOUT", "provider request timed out")
	default:
		// 对齐 ssh/database（round 6）：连接/上游失败必须透出底层原因。
		writeError(c, fallback, fmt.Sprintf("provider request failed: %v", err))
	}
}

func timeNow(d *Deps) time.Time {
	if d.Clock != nil {
		return d.Clock().UTC()
	}
	return time.Now().UTC()
}
