package llm

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"aiops/internal/store"
)

type registryStore struct {
	store.Store
	mu       sync.Mutex
	provider store.Provider
}

func (s *registryStore) Providers() store.ProviderStore { return &registryProviderStore{s: s} }

type registryProviderStore struct {
	store.ProviderStore
	s *registryStore
}

func (r *registryProviderStore) Get(_ context.Context, id string) (store.Provider, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if r.s.provider.ID != id {
		return store.Provider{}, store.ErrNotFound
	}
	return r.s.provider, nil
}

func TestRegistryCachesAndInvalidates(t *testing.T) {
	st := &registryStore{provider: store.Provider{
		ID: "p1", Name: "one", Kind: store.ProviderOpenAICompatible,
		BaseURL: "http://127.0.0.1:1", DefaultModel: "m",
		UpdatedAt: time.Unix(100, 0),
	}}
	reg := NewRegistry(st, nil, nil, nil)
	defer reg.Close()

	c1, _, err := reg.Client(context.Background(), "p1")
	if err != nil {
		t.Fatalf("Client: %v", err)
	}
	c2, _, err := reg.Client(context.Background(), "p1")
	if err != nil {
		t.Fatalf("Client: %v", err)
	}
	if c1 != c2 {
		t.Fatal("expected cached client instance")
	}

	// updated_at 变化后应重建。
	st.mu.Lock()
	st.provider.Name = "two"
	st.provider.UpdatedAt = time.Unix(200, 0)
	st.mu.Unlock()
	c3, p3, err := reg.Client(context.Background(), "p1")
	if err != nil {
		t.Fatalf("Client: %v", err)
	}
	if c3 == c1 {
		t.Fatal("expected client rebuild after updated_at change")
	}
	if p3.Name != "two" {
		t.Errorf("provider = %+v", p3)
	}

	reg.Invalidate("p1")
	c4, _, err := reg.Client(context.Background(), "p1")
	if err != nil {
		t.Fatalf("Client: %v", err)
	}
	if c4 == c3 {
		t.Fatal("expected client rebuild after Invalidate")
	}
}

func TestRegistryUnknownProvider(t *testing.T) {
	reg := NewRegistry(&registryStore{}, nil, nil, nil)
	_, _, err := reg.Client(context.Background(), "missing")
	if !errors.Is(err, ErrNoProvider) {
		t.Fatalf("error = %v, want ErrNoProvider", err)
	}
}

func TestPresetsAreCompleteAndOrdered(t *testing.T) {
	presets := Presets()
	if len(presets) != 9 {
		t.Fatalf("presets = %d", len(presets))
	}
	want := []store.ProviderPreset{
		store.PresetOpenAI, store.PresetDeepSeek, store.PresetDashScope,
		store.PresetMoonshot, store.PresetZhipu, store.PresetAnthropic,
		store.PresetGemini, store.PresetOllama, store.PresetCustom,
	}
	for i, p := range presets {
		if p.Preset != want[i] {
			t.Errorf("preset[%d] = %q, want %q", i, p.Preset, want[i])
		}
		if p.SuggestedModels == nil {
			t.Errorf("preset %q has nil models", p.Preset)
		}
		if p.Preset == store.PresetAnthropic && p.Kind != store.ProviderAnthropic {
			t.Errorf("anthropic kind = %q", p.Kind)
		}
		if p.Preset != store.PresetAnthropic && p.Kind != store.ProviderOpenAICompatible {
			t.Errorf("%q kind = %q", p.Preset, p.Kind)
		}
	}
	// 值拷贝：修改返回值不得影响下一次调用。
	presets[0].SuggestedModels[0] = "mutated"
	if Presets()[0].SuggestedModels[0] == "mutated" {
		t.Fatal("Presets must return copies")
	}
}
