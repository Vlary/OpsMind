package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"aiops/internal/store"
)

type providerStore struct{ q querier }

const providerColumns = `id::text, name, preset, kind, base_url,
	coalesce(api_key_enc, ''), api_key_hint, default_model, models,
	created_at, updated_at`

func scanProvider(row pgx.Row) (store.Provider, error) {
	var p store.Provider
	var modelsRaw []byte
	err := row.Scan(
		&p.ID, &p.Name, &p.Preset, &p.Kind, &p.BaseURL,
		&p.APIKeyEncrypted, &p.APIKeyHint, &p.DefaultModel, &modelsRaw,
		&p.CreatedAt, &p.UpdatedAt,
	)
	if err != nil {
		return store.Provider{}, mapError(err)
	}
	if len(modelsRaw) > 0 {
		if err := json.Unmarshal(modelsRaw, &p.Models); err != nil {
			return store.Provider{}, fmt.Errorf("postgres: decode provider models: %w", err)
		}
	}
	if p.Models == nil {
		p.Models = []string{}
	}
	p.HasAPIKey = p.APIKeyEncrypted != ""
	p.APIKeyMasked = apiKeyMask(p.HasAPIKey, p.APIKeyHint)
	p.CreatedAt = utc(p.CreatedAt)
	p.UpdatedAt = utc(p.UpdatedAt)
	return p, nil
}

// apiKeyMask 只由密文是否存在与末 4 位提示派生，绝不接触明文。
func apiKeyMask(hasKey bool, hint string) string {
	if !hasKey {
		return ""
	}
	if len([]rune(hint)) < 4 {
		return "****"
	}
	return "****" + hint
}

func marshalModels(models []string) ([]byte, error) {
	if models == nil {
		models = []string{}
	}
	raw, err := json.Marshal(models)
	if err != nil {
		return nil, fmt.Errorf("postgres: encode provider models: %w", err)
	}
	return raw, nil
}

func (s providerStore) List(ctx context.Context) ([]store.Provider, error) {
	rows, err := s.q.Query(ctx, `SELECT `+providerColumns+` FROM providers ORDER BY created_at ASC, id ASC`)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()

	items := make([]store.Provider, 0)
	for rows.Next() {
		item, err := scanProvider(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, mapError(rows.Err())
}

func (s providerStore) Get(ctx context.Context, id string) (store.Provider, error) {
	return scanProvider(s.q.QueryRow(ctx, `SELECT `+providerColumns+` FROM providers WHERE id = $1::uuid`, id))
}

func (s providerStore) Create(ctx context.Context, p store.CreateProviderParams) (store.Provider, error) {
	encrypted := ""
	hint := ""
	if p.APIKey != nil {
		encrypted = p.APIKey.Encrypted
		hint = p.APIKey.Hint
	}
	models, err := marshalModels(p.Models)
	if err != nil {
		return store.Provider{}, err
	}
	return scanProvider(s.q.QueryRow(ctx, `
		INSERT INTO providers (name, preset, kind, base_url, api_key_enc, api_key_hint, default_model, models)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb)
		RETURNING `+providerColumns,
		p.Name, p.Preset, p.Kind, p.BaseURL, nullStr(encrypted), hint, p.DefaultModel, models))
}

func (s providerStore) Update(ctx context.Context, id string, p store.UpdateProviderParams) (store.Provider, error) {
	nameSet, name := p.Name != nil, ""
	if nameSet {
		name = *p.Name
	}
	presetSet, preset := p.Preset != nil, store.ProviderPreset("")
	if presetSet {
		preset = *p.Preset
	}
	kindSet, kind := p.Kind != nil, store.ProviderKind("")
	if kindSet {
		kind = *p.Kind
	}
	urlSet, baseURL := p.BaseURL != nil, ""
	if urlSet {
		baseURL = *p.BaseURL
	}
	keySet := p.APIKey != nil
	keyEnc := any(nil)
	keyHint := ""
	if keySet {
		keyEnc = nullStr(p.APIKey.Encrypted)
		keyHint = p.APIKey.Hint
	}
	modelSet, defaultModel := p.DefaultModel != nil, ""
	if modelSet {
		defaultModel = *p.DefaultModel
	}
	modelsSet := p.Models != nil
	var models []byte
	if modelsSet {
		var err error
		models, err = marshalModels(*p.Models)
		if err != nil {
			return store.Provider{}, err
		}
	}

	return scanProvider(s.q.QueryRow(ctx, `
		UPDATE providers SET
			name = CASE WHEN $1::boolean THEN $2::text ELSE name END,
			preset = CASE WHEN $3::boolean THEN $4::text ELSE preset END,
			kind = CASE WHEN $5::boolean THEN $6::text ELSE kind END,
			base_url = CASE WHEN $7::boolean THEN $8::text ELSE base_url END,
			api_key_enc = CASE WHEN $9::boolean THEN $10::text ELSE api_key_enc END,
			api_key_hint = CASE WHEN $9::boolean THEN $11::text ELSE api_key_hint END,
			default_model = CASE WHEN $12::boolean THEN $13::text ELSE default_model END,
			models = CASE WHEN $14::boolean THEN $15::jsonb ELSE models END,
			updated_at = now()
		WHERE id = $16::uuid
		RETURNING `+providerColumns,
		nameSet, name, presetSet, string(preset), kindSet, string(kind), urlSet, baseURL,
		keySet, keyEnc, keyHint, modelSet, defaultModel, modelsSet, models, id))
}

func (s providerStore) Delete(ctx context.Context, id string) error {
	return deleteByID(ctx, s.q, "providers", id)
}
