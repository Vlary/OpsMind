package llm

import "aiops/internal/store"

// Preset 是内置供应商预设（供 /api/providers/presets 与前端表单使用）。
type Preset struct {
	Preset          store.ProviderPreset `json:"preset"`
	Name            string               `json:"name"`
	Kind            store.ProviderKind   `json:"kind"`
	BaseURL         string               `json:"base_url"`
	SuggestedModels []string             `json:"suggested_models"`
	APIKeyRequired  bool                 `json:"api_key_required"`
}

// Presets 返回固定顺序的 9 个预设，每项都是值拷贝（含独立切片）。
func Presets() []Preset {
	presets := []Preset{
		{
			Preset:          store.PresetOpenAI,
			Name:            "OpenAI",
			Kind:            store.ProviderOpenAICompatible,
			BaseURL:         "https://api.openai.com/v1",
			SuggestedModels: []string{"gpt-4.1-mini", "gpt-4.1"},
			APIKeyRequired:  true,
		},
		{
			Preset:          store.PresetDeepSeek,
			Name:            "DeepSeek",
			Kind:            store.ProviderOpenAICompatible,
			BaseURL:         "https://api.deepseek.com/v1",
			SuggestedModels: []string{"deepseek-chat", "deepseek-reasoner"},
			APIKeyRequired:  true,
		},
		{
			Preset:          store.PresetDashScope,
			Name:            "通义千问（DashScope）",
			Kind:            store.ProviderOpenAICompatible,
			BaseURL:         "https://dashscope.aliyuncs.com/compatible-mode/v1",
			SuggestedModels: []string{"qwen-plus", "qwen-max"},
			APIKeyRequired:  true,
		},
		{
			Preset:          store.PresetMoonshot,
			Name:            "Moonshot / Kimi",
			Kind:            store.ProviderOpenAICompatible,
			BaseURL:         "https://api.moonshot.cn/v1",
			SuggestedModels: []string{"moonshot-v1-8k", "moonshot-v1-32k"},
			APIKeyRequired:  true,
		},
		{
			Preset:          store.PresetZhipu,
			Name:            "智谱 GLM",
			Kind:            store.ProviderOpenAICompatible,
			BaseURL:         "https://open.bigmodel.cn/api/paas/v4",
			SuggestedModels: []string{"glm-4-plus", "glm-4-flash"},
			APIKeyRequired:  true,
		},
		{
			Preset:          store.PresetAnthropic,
			Name:            "Anthropic Claude",
			Kind:            store.ProviderAnthropic,
			BaseURL:         "https://api.anthropic.com",
			SuggestedModels: []string{"claude-sonnet-4-20250514", "claude-3-5-haiku-20241022"},
			APIKeyRequired:  true,
		},
		{
			Preset:          store.PresetGemini,
			Name:            "Google Gemini（OpenAI 兼容）",
			Kind:            store.ProviderOpenAICompatible,
			BaseURL:         "https://generativelanguage.googleapis.com/v1beta/openai",
			SuggestedModels: []string{"gemini-2.0-flash", "gemini-2.5-pro"},
			APIKeyRequired:  true,
		},
		{
			Preset:          store.PresetOllama,
			Name:            "Ollama",
			Kind:            store.ProviderOpenAICompatible,
			BaseURL:         "http://127.0.0.1:11434/v1",
			SuggestedModels: []string{"llama3.2", "qwen2.5"},
			APIKeyRequired:  false,
		},
		{
			Preset:          store.PresetCustom,
			Name:            "自定义 OpenAI 兼容",
			Kind:            store.ProviderOpenAICompatible,
			BaseURL:         "",
			SuggestedModels: []string{},
			APIKeyRequired:  false,
		},
	}
	// 值拷贝，避免调用方修改内置切片。
	out := make([]Preset, len(presets))
	for i, p := range presets {
		models := make([]string, len(p.SuggestedModels))
		copy(models, p.SuggestedModels)
		p.SuggestedModels = models
		out[i] = p
	}
	return out
}

// PresetByValue 查找预设；第二个返回值表示是否存在。
func PresetByValue(v store.ProviderPreset) (Preset, bool) {
	for _, p := range Presets() {
		if p.Preset == v {
			return p, true
		}
	}
	return Preset{}, false
}
