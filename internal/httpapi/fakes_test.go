package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"

	"aiops/internal/agent"
	"aiops/internal/approvals"
	"aiops/internal/audit"
	"aiops/internal/auth"
	"aiops/internal/config"
	"aiops/internal/cryptox"
	"aiops/internal/dbproxy"
	"aiops/internal/llm"
	"aiops/internal/sshx"
	"aiops/internal/store"
)

// ---------------------------------------------------------------------------
// 内存 store：覆盖全部仓储接口，带错误注入钩子
// ---------------------------------------------------------------------------

type memStore struct {
	mu            sync.Mutex
	pingErr       error
	seq           int
	users         map[string]store.User
	sessions      map[string]store.Session
	settings      map[string]store.Setting
	servers       map[string]store.Server
	databases     map[string]store.Database
	providers     map[string]store.Provider
	conversations map[string]store.Conversation
	messages      []store.Message
	toolCalls     []store.ToolCall
	approvals     map[string]store.Approval
	auditLogs     []store.AuditLog

	// 错误注入（nil 表示成功路径）。
	errPing             error
	errSessionsByHash   error
	errConversationsGet error
	errMessagesList     error
	errServersCreate    error
	errServersUpdate    error
	errServersList      error // 八期：回放 affected_servers 装配失败不阻断
	errDatabasesGet     error
	errProvidersCreate  error
	errApprovalsDecide  error
	errAuditList        error
}

func newMemStore() *memStore {
	return &memStore{
		users:         map[string]store.User{},
		sessions:      map[string]store.Session{},
		settings:      map[string]store.Setting{},
		servers:       map[string]store.Server{},
		databases:     map[string]store.Database{},
		providers:     map[string]store.Provider{},
		conversations: map[string]store.Conversation{},
		approvals:     map[string]store.Approval{},
	}
}

func (m *memStore) Ping(context.Context) error { return m.pingErr }
func (m *memStore) Close()                     {}

func (m *memStore) WithTx(ctx context.Context, fn func(store.Store) error) error {
	return fn(m)
}

func (m *memStore) nextID(prefix string) string {
	m.seq++
	return fmt.Sprintf("%s-%d", prefix, m.seq)
}

func (m *memStore) Users() store.UserStore         { return (*memUsers)(m) }
func (m *memStore) Sessions() store.SessionStore   { return (*memSessions)(m) }
func (m *memStore) Servers() store.ServerStore     { return (*memServers)(m) }
func (m *memStore) Databases() store.DatabaseStore { return (*memDatabases)(m) }
func (m *memStore) Providers() store.ProviderStore { return (*memProviders)(m) }
func (m *memStore) Conversations() store.ConversationStore {
	return (*memConversations)(m)
}
func (m *memStore) Messages() store.MessageStore   { return (*memMessages)(m) }
func (m *memStore) ToolCalls() store.ToolCallStore { return (*memToolCalls)(m) }
func (m *memStore) Approvals() store.ApprovalStore { return (*memApprovals)(m) }
func (m *memStore) AuditLogs() store.AuditStore    { return (*memAudit)(m) }
func (m *memStore) Settings() store.SettingStore   { return (*memSettings)(m) }

// 五期 M2（diagnosis/inspection）向 store.Store 增挂三个子接口后的最小
// stub（embed nil：触达即 panic）——让 memStore 满足接口、既有测试可编译；
// M2 落 diagnosis/inspection handler 测试时替换为真实 fake（同 M3 的
// batchState 模式，见 batch_handlers_test.go）。inspection 的真实替换见
// inspection_handlers_test.go（inspectionState）；diagnosis 尚为占位。
func (m *memStore) DiagnosisSessions() store.DiagnosisSessionStore {
	return stubDiagnosisSessions{}
}

type stubDiagnosisSessions struct{ store.DiagnosisSessionStore }

type memUsers memStore

func (r *memUsers) Count(context.Context) (int64, error) { return int64(len(r.users)), nil }
func (r *memUsers) Create(_ context.Context, p store.CreateUserParams) (store.User, error) {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.users[p.Username]; ok {
		return store.User{}, store.ErrConflict
	}
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	u := store.User{ID: m.nextID("user"), Username: p.Username, PasswordHash: p.PasswordHash, CreatedAt: now, UpdatedAt: now}
	m.users[p.Username] = u
	return u, nil
}
func (r *memUsers) ByUsername(_ context.Context, username string) (store.User, error) {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[username]
	if !ok {
		return store.User{}, store.ErrNotFound
	}
	return u, nil
}
func (r *memUsers) ByID(_ context.Context, id string) (store.User, error) {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, u := range m.users {
		if u.ID == id {
			return u, nil
		}
	}
	return store.User{}, store.ErrNotFound
}
func (r *memUsers) UsernamesByIDs(_ context.Context, ids []string) (map[string]string, error) {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]string, len(ids))
	for _, id := range ids {
		for _, u := range m.users {
			if u.ID == id {
				out[id] = u.Username
				break
			}
		}
	}
	return out, nil
}
func (r *memUsers) UpdatePassword(_ context.Context, id string, p store.UpdatePasswordParams) error {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, u := range m.users {
		if u.ID == id {
			u.PasswordHash = p.PasswordHash
			u.UpdatedAt = p.UpdatedAt
			m.users[name] = u
			return nil
		}
	}
	return store.ErrNotFound
}

type memSessions memStore

func (r *memSessions) Create(_ context.Context, p store.CreateSessionParams) (store.Session, error) {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	s := store.Session{
		ID: m.nextID("sess"), UserID: p.UserID, TokenHash: append([]byte(nil), p.TokenHash...),
		ExpiresAt: p.ExpiresAt, LastSeenAt: p.CreatedAt, IP: p.IP, UserAgent: p.UserAgent, CreatedAt: p.CreatedAt,
	}
	m.sessions[s.ID] = s
	return s, nil
}
func (r *memSessions) ByTokenHash(_ context.Context, hash []byte) (store.Session, error) {
	m := (*memStore)(r)
	if m.errSessionsByHash != nil {
		return store.Session{}, m.errSessionsByHash
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sessions {
		if bytes.Equal(s.TokenHash, hash) {
			return s, nil
		}
	}
	return store.Session{}, store.ErrNotFound
}
func (r *memSessions) Touch(_ context.Context, id string, at time.Time) error {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[id]; ok {
		s.LastSeenAt = at
		m.sessions[id] = s
	}
	return nil
}
func (r *memSessions) Delete(_ context.Context, id string) error {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
	return nil
}
func (r *memSessions) DeleteByUser(_ context.Context, userID string) error {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, s := range m.sessions {
		if s.UserID == userID {
			delete(m.sessions, id)
		}
	}
	return nil
}
func (r *memSessions) DeleteExpired(_ context.Context, before time.Time) (int64, error) {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for id, s := range m.sessions {
		if !s.ExpiresAt.After(before) {
			delete(m.sessions, id)
			n++
		}
	}
	return n, nil
}

type memSettings memStore

func (r *memSettings) Get(_ context.Context, key string) (store.Setting, error) {
	m := (*memStore)(r)
	if v, ok := m.settings[key]; ok {
		return v, nil
	}
	return store.Setting{}, store.ErrNotFound
}
func (r *memSettings) Set(_ context.Context, key string, value json.RawMessage, at time.Time) (store.Setting, error) {
	m := (*memStore)(r)
	s := store.Setting{Key: key, Value: append(json.RawMessage(nil), value...), UpdatedAt: at}
	m.settings[key] = s
	return s, nil
}
func (r *memSettings) List(context.Context) ([]store.Setting, error) {
	out := make([]store.Setting, 0, len(r.settings))
	for _, v := range r.settings {
		out = append(out, v)
	}
	return out, nil
}

type memServers memStore

func (r *memServers) List(context.Context) ([]store.Server, error) {
	m := (*memStore)(r)
	m.mu.Lock()
	err := m.errServersList
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	out := make([]store.Server, 0, len(m.servers))
	for _, s := range m.servers {
		out = append(out, s)
	}
	return out, nil
}
func (r *memServers) Get(_ context.Context, id string) (store.Server, error) {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.servers[id]
	if !ok {
		return store.Server{}, store.ErrNotFound
	}
	return s, nil
}
func (r *memServers) Create(_ context.Context, p store.CreateServerParams) (store.Server, error) {
	m := (*memStore)(r)
	if m.errServersCreate != nil {
		return store.Server{}, m.errServersCreate
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.servers {
		if s.Name == p.Name {
			return store.Server{}, store.ErrConflict
		}
	}
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	osVal := p.OS
	if osVal == "" {
		osVal = store.ServerOSLinux
	}
	kind := p.DeviceKind
	if kind == "" {
		kind = store.DeviceKindServer
	}
	proto := p.AccessProto
	if proto == "" {
		proto = store.AccessProtoSSH
	}
	if kind == store.DeviceKindNetworkDevice {
		osVal = store.ServerOSNetwork
	}
	winrmPort := p.WinRMPort
	if winrmPort == 0 {
		winrmPort = 5985
	}
	s := store.Server{
		ID: m.nextID("srv"), Name: p.Name, OS: osVal, Host: p.Host, Port: p.Port,
		DeviceKind: kind, NetdevBrand: p.NetdevBrand, AccessProto: proto,
		WinRMPort: winrmPort, Username: p.Username,
		AuthMethod: p.Credentials.AuthMethod, PasswordEncrypted: p.Credentials.PasswordEncrypted,
		PrivateKeyEncrypted: p.Credentials.PrivateKeyEncrypted, PassphraseEncrypted: p.Credentials.PassphraseEncrypted,
		EnableSecretEncrypted: p.EnableSecretEncrypted,
		Tags:                  p.Tags, AutoExecEnabled: p.AutoExecEnabled, CreatedAt: now, UpdatedAt: now,
	}
	if s.Tags == nil {
		s.Tags = []string{}
	}
	m.servers[s.ID] = s
	return s, nil
}
func (r *memServers) Update(_ context.Context, id string, p store.UpdateServerParams) (store.Server, error) {
	m := (*memStore)(r)
	if m.errServersUpdate != nil {
		return store.Server{}, m.errServersUpdate
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.servers[id]
	if !ok {
		return store.Server{}, store.ErrNotFound
	}
	if p.Name != nil {
		for oid, other := range m.servers {
			if other.Name == *p.Name && oid != id {
				return store.Server{}, store.ErrConflict
			}
		}
		s.Name = *p.Name
	}
	if p.OS != nil {
		s.OS = *p.OS
	}
	if p.DeviceKind != nil {
		s.DeviceKind = *p.DeviceKind
	}
	if p.NetdevBrand != nil {
		s.NetdevBrand = *p.NetdevBrand
	}
	if p.AccessProto != nil {
		s.AccessProto = *p.AccessProto
	}
	if p.EnableSecretEncrypted != nil {
		s.EnableSecretEncrypted = *p.EnableSecretEncrypted
	}
	if p.Host != nil {
		s.Host = *p.Host
	}
	if p.Port != nil {
		s.Port = *p.Port
	}
	if p.WinRMPort != nil {
		s.WinRMPort = *p.WinRMPort
	}
	if p.Username != nil {
		s.Username = *p.Username
	}
	if p.Tags != nil {
		s.Tags = *p.Tags
	}
	if p.AutoExecEnabled != nil {
		s.AutoExecEnabled = *p.AutoExecEnabled
	}
	if p.Credentials != nil {
		s.AuthMethod = p.Credentials.AuthMethod
		s.PasswordEncrypted = p.Credentials.PasswordEncrypted
		s.PrivateKeyEncrypted = p.Credentials.PrivateKeyEncrypted
		s.PassphraseEncrypted = p.Credentials.PassphraseEncrypted
	}
	m.servers[id] = s
	return s, nil
}
func (r *memServers) Delete(_ context.Context, id string) error {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.servers[id]; !ok {
		return store.ErrNotFound
	}
	delete(m.servers, id)
	return nil
}

// UpdateContainerRuntime 写回容器运行时探测缓存（四期 C1）。
func (r *memServers) UpdateContainerRuntime(_ context.Context, id string, runtime store.ContainerRuntime, _ time.Time) (store.Server, error) {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.servers[id]
	if !ok {
		return store.Server{}, store.ErrNotFound
	}
	if s.DeviceKind == store.DeviceKindNetworkDevice && runtime != store.ContainerRuntimeNone {
		return store.Server{}, store.ErrInvalid
	}
	s.ContainerRuntime = runtime
	m.servers[id] = s
	return s, nil
}

func (r *memServers) UpdateHostKey(_ context.Context, id, fingerprint, publicKey string, at time.Time) (store.Server, error) {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.servers[id]
	if !ok {
		return store.Server{}, store.ErrNotFound
	}
	s.HostKeyFingerprint = fingerprint
	s.HostKeyPublic = publicKey
	s.HostKeyVerifiedAt = &at
	m.servers[id] = s
	return s, nil
}

type memDatabases memStore

func (r *memDatabases) List(context.Context) ([]store.Database, error) {
	m := (*memStore)(r)
	out := make([]store.Database, 0, len(m.databases))
	for _, d := range m.databases {
		out = append(out, d)
	}
	return out, nil
}
func (r *memDatabases) Get(_ context.Context, id string) (store.Database, error) {
	m := (*memStore)(r)
	if m.errDatabasesGet != nil {
		return store.Database{}, m.errDatabasesGet
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.databases[id]
	if !ok {
		return store.Database{}, store.ErrNotFound
	}
	return d, nil
}
func (r *memDatabases) Create(_ context.Context, p store.CreateDatabaseParams) (store.Database, error) {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, d := range m.databases {
		if d.Name == p.Name {
			return store.Database{}, store.ErrConflict
		}
	}
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	d := store.Database{
		ID: m.nextID("db"), Name: p.Name, Engine: p.Engine, Host: p.Host, Port: p.Port,
		InstanceName: p.InstanceName, DatabaseName: p.DatabaseName, Username: p.Username,
		RedisDB:           p.RedisDB,
		PasswordEncrypted: p.Credentials.PasswordEncrypted, SQLitePath: p.SQLitePath,
		SSLMode: p.SSLMode, CreatedAt: now, UpdatedAt: now,
	}
	m.databases[d.ID] = d
	return d, nil
}
func (r *memDatabases) Update(_ context.Context, id string, p store.UpdateDatabaseParams) (store.Database, error) {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.databases[id]
	if !ok {
		return store.Database{}, store.ErrNotFound
	}
	if p.Name != nil {
		// 对齐真实 store 的 name 唯一约束（databases_name_key）与
		// memServers.Update：改名撞已有名称返回 ErrConflict。
		for oid, other := range m.databases {
			if other.Name == *p.Name && oid != id {
				return store.Database{}, store.ErrConflict
			}
		}
		d.Name = *p.Name
	}
	if p.Host != nil {
		d.Host = *p.Host
	}
	if p.Port != nil {
		d.Port = *p.Port
	}
	if p.InstanceName != nil {
		d.InstanceName = *p.InstanceName
	}
	if p.RedisDB != nil {
		d.RedisDB = *p.RedisDB
	}
	if p.DatabaseName != nil {
		d.DatabaseName = *p.DatabaseName
	}
	if p.Username != nil {
		d.Username = *p.Username
	}
	if p.Credentials != nil {
		d.PasswordEncrypted = p.Credentials.PasswordEncrypted
	}
	if p.SQLitePath != nil {
		d.SQLitePath = *p.SQLitePath
	}
	if p.SSLMode != nil {
		d.SSLMode = *p.SSLMode
	}
	m.databases[id] = d
	return d, nil
}
func (r *memDatabases) Delete(_ context.Context, id string) error {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.databases[id]; !ok {
		return store.ErrNotFound
	}
	delete(m.databases, id)
	return nil
}

type memProviders memStore

func (r *memProviders) List(context.Context) ([]store.Provider, error) {
	m := (*memStore)(r)
	out := make([]store.Provider, 0, len(m.providers))
	for _, p := range m.providers {
		out = append(out, p)
	}
	return out, nil
}
func (r *memProviders) Get(_ context.Context, id string) (store.Provider, error) {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.providers[id]
	if !ok {
		return store.Provider{}, store.ErrNotFound
	}
	return p, nil
}
func (r *memProviders) Create(_ context.Context, p store.CreateProviderParams) (store.Provider, error) {
	m := (*memStore)(r)
	if m.errProvidersCreate != nil {
		return store.Provider{}, m.errProvidersCreate
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, existing := range m.providers {
		if existing.Name == p.Name {
			return store.Provider{}, store.ErrConflict
		}
	}
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	prov := store.Provider{
		ID: m.nextID("prov"), Name: p.Name, Preset: p.Preset, Kind: p.Kind, BaseURL: p.BaseURL,
		DefaultModel: p.DefaultModel, Models: p.Models, CreatedAt: now, UpdatedAt: now,
	}
	if p.APIKey != nil {
		prov.APIKeyEncrypted = p.APIKey.Encrypted
		prov.APIKeyHint = p.APIKey.Hint
		prov.HasAPIKey = true
	}
	if prov.Models == nil {
		prov.Models = []string{}
	}
	m.providers[prov.ID] = prov
	return prov, nil
}
func (r *memProviders) Update(_ context.Context, id string, p store.UpdateProviderParams) (store.Provider, error) {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	prov, ok := m.providers[id]
	if !ok {
		return store.Provider{}, store.ErrNotFound
	}
	if p.Name != nil {
		prov.Name = *p.Name
	}
	if p.Preset != nil {
		prov.Preset = *p.Preset
	}
	if p.Kind != nil {
		prov.Kind = *p.Kind
	}
	if p.BaseURL != nil {
		prov.BaseURL = *p.BaseURL
	}
	if p.DefaultModel != nil {
		prov.DefaultModel = *p.DefaultModel
	}
	if p.Models != nil {
		prov.Models = *p.Models
	}
	if p.APIKey != nil {
		if p.APIKey.Encrypted == "" {
			prov.APIKeyEncrypted = ""
			prov.APIKeyHint = ""
			prov.HasAPIKey = false
		} else {
			prov.APIKeyEncrypted = p.APIKey.Encrypted
			prov.APIKeyHint = p.APIKey.Hint
			prov.HasAPIKey = true
		}
	}
	m.providers[id] = prov
	return prov, nil
}
func (r *memProviders) Delete(_ context.Context, id string) error {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.providers[id]; !ok {
		return store.ErrNotFound
	}
	delete(m.providers, id)
	return nil
}

type memConversations memStore

func (r *memConversations) ListPage(_ context.Context, cursor store.ConversationListCursor, limit int) ([]store.Conversation, bool, error) {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]store.Conversation, 0, len(m.conversations))
	for _, c := range m.conversations {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].LastMessageAt.Equal(out[j].LastMessageAt) {
			return out[i].LastMessageAt.After(out[j].LastMessageAt)
		}
		return out[i].ID > out[j].ID
	})
	start := 0
	if cursor.ID != "" {
		for start < len(out) {
			c := out[start]
			if !c.LastMessageAt.Equal(cursor.LastMessageAt) {
				if c.LastMessageAt.Before(cursor.LastMessageAt) {
					break
				}
			} else if c.ID < cursor.ID {
				break
			}
			start++
		}
	}
	end := start + limit
	hasMore := false
	if end < len(out) {
		hasMore = true
	} else {
		end = len(out)
	}
	return out[start:end], hasMore, nil
}
func (r *memConversations) Get(_ context.Context, id string) (store.Conversation, error) {
	m := (*memStore)(r)
	if m.errConversationsGet != nil {
		return store.Conversation{}, m.errConversationsGet
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.conversations[id]
	if !ok {
		return store.Conversation{}, store.ErrNotFound
	}
	return c, nil
}
func (r *memConversations) Create(_ context.Context, p store.CreateConversationParams) (store.Conversation, error) {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	c := store.Conversation{ID: m.nextID("conv"), Title: p.Title, CreatedAt: now, UpdatedAt: now, LastMessageAt: now}
	m.conversations[c.ID] = c
	return c, nil
}
func (r *memConversations) UpdateTitle(_ context.Context, id, title string, at time.Time) (store.Conversation, error) {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.conversations[id]
	if !ok {
		return store.Conversation{}, store.ErrNotFound
	}
	c.Title = title
	c.UpdatedAt = at
	m.conversations[id] = c
	return c, nil
}
func (r *memConversations) BindModel(_ context.Context, id, providerID, model string, at time.Time) (store.Conversation, error) {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.conversations[id]
	if !ok {
		return store.Conversation{}, store.ErrNotFound
	}
	c.ProviderID = &providerID
	c.Model = model
	m.conversations[id] = c
	return c, nil
}
func (r *memConversations) SetApprovalMode(_ context.Context, id string, mode *store.ApprovalMode, at time.Time) (store.Conversation, error) {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.conversations[id]
	if !ok {
		return store.Conversation{}, store.ErrNotFound
	}
	c.ApprovalMode = mode
	c.UpdatedAt = at
	m.conversations[id] = c
	return c, nil
}
func (r *memConversations) Touch(_ context.Context, id string, at time.Time) error { return nil }

// ListReplayCatalog 与生产 pgstore 同语义的内存实现（八期 BE-R）：
// 排序 last_message_at DESC, id DESC；q 大小写不敏感子串（无需转义——
// 内存比较是字面匹配）、has_actions 过滤、游标翻页、多查一条判 has_more。
// 计数与 truncated 预判口径同契约 5.19.1。
func (r *memConversations) ListReplayCatalog(_ context.Context, p store.ListReplayCatalogParams) ([]store.ReplayCatalogItem, bool, error) {
	m := (*memStore)(r)
	if p.Limit < 1 || p.Limit > replayCatalogMaxLimit {
		return nil, false, store.ErrInvalid
	}
	if utf8.RuneCountInString(p.Query) > replayCatalogQueryMaxRunes {
		return nil, false, store.ErrInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	actionTools := map[string]struct{}{}
	for _, name := range p.ActionTools {
		actionTools[name] = struct{}{}
	}
	q := strings.ToLower(p.Query)
	out := make([]store.ReplayCatalogItem, 0, len(m.conversations))
	for _, c := range m.conversations {
		if q != "" && !strings.Contains(strings.ToLower(c.Title), q) {
			continue
		}
		item := store.ReplayCatalogItem{
			ConversationID: c.ID, Title: c.Title, LastMessageAt: c.LastMessageAt,
		}
		for _, tc := range m.toolCalls {
			if tc.ConversationID != c.ID {
				continue
			}
			item.ToolCount++
			if _, ok := actionTools[tc.Name]; ok {
				item.ActionCount++
			}
		}
		for _, ap := range m.approvals {
			if ap.ConversationID != nil && *ap.ConversationID == c.ID {
				item.ApprovalCount++
			}
		}
		item.HasActions = item.ActionCount > 0
		item.Truncated = item.ToolCount+item.ApprovalCount > replayMaxSteps
		if p.HasActions && !item.HasActions {
			continue
		}
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].LastMessageAt.Equal(out[j].LastMessageAt) {
			return out[i].LastMessageAt.After(out[j].LastMessageAt)
		}
		return out[i].ConversationID > out[j].ConversationID
	})
	start := 0
	if p.Cursor.ID != "" {
		for start < len(out) {
			it := out[start]
			if !it.LastMessageAt.Equal(p.Cursor.LastMessageAt) {
				if it.LastMessageAt.Before(p.Cursor.LastMessageAt) {
					break
				}
			} else if it.ConversationID < p.Cursor.ID {
				break
			}
			start++
		}
	}
	end := start + p.Limit
	hasMore := false
	if end < len(out) {
		hasMore = true
	} else {
		end = len(out)
	}
	return out[start:end], hasMore, nil
}
func (r *memConversations) Delete(_ context.Context, id string) error {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.conversations[id]; !ok {
		return store.ErrNotFound
	}
	delete(m.conversations, id)
	return nil
}

type memMessages memStore

func (r *memMessages) Create(_ context.Context, p store.CreateMessageParams) (store.Message, error) {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	msg := store.Message{
		ID: m.nextID("msg"), Seq: int64(len(m.messages) + 1), ConversationID: p.ConversationID,
		TurnID: p.TurnID, Role: p.Role, Content: p.Content, Reasoning: p.Reasoning,
		Status: p.Status, ErrorCode: p.ErrorCode, Model: p.Model,
		PromptTokens: p.PromptTokens, CompletionTokens: p.CompletionTokens, CreatedAt: p.CreatedAt,
	}
	m.messages = append(m.messages, msg)
	return msg, nil
}
func (r *memMessages) ListPage(_ context.Context, conversationID string, beforeSeq int64, limit int) ([]store.Message, bool, error) {
	m := (*memStore)(r)
	if m.errMessagesList != nil {
		return nil, false, m.errMessagesList
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.Message
	for i := len(m.messages) - 1; i >= 0 && len(out) < limit+1; i-- {
		msg := m.messages[i]
		if msg.ConversationID != conversationID {
			continue
		}
		if beforeSeq > 0 && msg.Seq >= beforeSeq {
			continue
		}
		out = append(out, msg)
	}
	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}
	return out, hasMore, nil
}

type memToolCalls memStore

func (r *memToolCalls) Create(_ context.Context, p store.CreateToolCallParams) (store.ToolCall, error) {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	tc := store.ToolCall{
		ID: m.nextID("tc"), Seq: int64(len(m.toolCalls) + 1), ConversationID: p.ConversationID,
		TurnID: p.TurnID, ProviderCallID: p.ProviderCallID, Name: p.Name, Arguments: p.Arguments,
		RiskLevel: p.RiskLevel, Status: p.Status, StartedAt: p.StartedAt,
	}
	m.toolCalls = append(m.toolCalls, tc)
	return tc, nil
}
func (r *memToolCalls) SetStatus(_ context.Context, id string, status store.ToolCallStatus, errorCode, errorMessage string) (store.ToolCall, error) {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.toolCalls {
		if m.toolCalls[i].ID == id {
			m.toolCalls[i].Status = status
			m.toolCalls[i].ErrorCode = errorCode
			m.toolCalls[i].ErrorMessage = errorMessage
			return m.toolCalls[i], nil
		}
	}
	return store.ToolCall{}, store.ErrNotFound
}
func (r *memToolCalls) Finish(_ context.Context, p store.FinishToolCallParams) (store.ToolCall, error) {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.toolCalls {
		if m.toolCalls[i].ID == p.ID {
			m.toolCalls[i].Status = p.Status
			m.toolCalls[i].Result = p.Result
			m.toolCalls[i].ErrorCode = p.ErrorCode
			m.toolCalls[i].ErrorMessage = p.ErrorMessage
			m.toolCalls[i].FinishedAt = &p.FinishedAt
			return m.toolCalls[i], nil
		}
	}
	return store.ToolCall{}, store.ErrNotFound
}
func (r *memToolCalls) ListPage(_ context.Context, conversationID string, beforeSeq int64, limit int) ([]store.ToolCall, bool, error) {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.ToolCall
	for i := len(m.toolCalls) - 1; i >= 0 && len(out) < limit+1; i-- {
		tc := m.toolCalls[i]
		if tc.ConversationID != conversationID {
			continue
		}
		if beforeSeq > 0 && tc.Seq >= beforeSeq {
			continue
		}
		out = append(out, tc)
	}
	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}
	return out, hasMore, nil
}
func (r *memToolCalls) ByID(_ context.Context, id string) (store.ToolCall, error) {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, tc := range m.toolCalls {
		if tc.ID == id {
			return tc, nil
		}
	}
	return store.ToolCall{}, store.ErrNotFound
}

type memApprovals memStore

func (r *memApprovals) Create(_ context.Context, p store.CreateApprovalParams) (store.Approval, error) {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	source := p.Source
	if source == "" {
		source = "chat"
	}
	ap := store.Approval{
		ID: m.nextID("ap"), ConversationID: p.ConversationID, ToolCallID: p.ToolCallID,
		Source:   source,
		ServerID: p.ServerID, ServerName: p.ServerName, Command: p.Command,
		RiskLevel: p.RiskLevel, RuleID: p.RuleID, Reason: p.Reason,
		ApprovalMode: store.ApprovalMode(p.ApprovalMode),
		Status:       store.ApprovalPending, RequestedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	if ap.ApprovalMode == "" {
		ap.ApprovalMode = store.ApprovalModeStandard
	}
	m.approvals[ap.ID] = ap
	return ap, nil
}
func (r *memApprovals) Get(_ context.Context, id string) (store.Approval, error) {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	ap, ok := m.approvals[id]
	if !ok {
		return store.Approval{}, store.ErrNotFound
	}
	return ap, nil
}
func (r *memApprovals) List(_ context.Context, conversationID string, status store.ApprovalStatus, limit int) ([]store.Approval, error) {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]store.Approval, 0, len(m.approvals))
	for _, ap := range m.approvals {
		// 四期 C1：console 审批 ConversationID 为 nil——按 conversation 过滤时排除（契约 3.4）。
		if conversationID != "" && (ap.ConversationID == nil || *ap.ConversationID != conversationID) {
			continue
		}
		if status != "" && ap.Status != status {
			continue
		}
		out = append(out, ap)
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
func (r *memApprovals) Decide(_ context.Context, p store.DecideApprovalParams) (store.Approval, error) {
	m := (*memStore)(r)
	if m.errApprovalsDecide != nil {
		return store.Approval{}, m.errApprovalsDecide
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ap, ok := m.approvals[p.ID]
	if !ok {
		return store.Approval{}, store.ErrNotFound
	}
	if ap.Status != store.ApprovalPending {
		return store.Approval{}, store.ErrConflict
	}
	ap.Status = p.Status
	ap.DecidedAt = &p.DecidedAt
	decidedBy := p.DecidedBy
	ap.DecidedBy = &decidedBy
	ap.Note = p.Note
	ap.EditedCommand = p.EditedCommand
	ap.EditedRiskLevel = p.EditedRiskLevel
	ap.EditedRuleID = p.EditedRuleID
	m.approvals[p.ID] = ap
	return ap, nil
}
func (r *memApprovals) Expire(_ context.Context, now time.Time) (int64, error) { return 0, nil }

type memAudit memStore

func (r *memAudit) Append(_ context.Context, p store.CreateAuditParams) error {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.auditLogs = append(m.auditLogs, store.AuditLog{
		ID: int64(len(m.auditLogs) + 1), CreatedAt: time.Now(), ActorType: p.ActorType,
		ActorID: p.ActorID, Action: p.Action, ResourceType: p.ResourceType,
		ResourceID: p.ResourceID, RequestID: p.RequestID, ConversationID: p.ConversationID,
		ServerID: p.ServerID, IP: p.IP, Detail: p.Detail,
	})
	return nil
}
func (r *memAudit) List(_ context.Context, f store.AuditFilter) ([]store.AuditLog, bool, error) {
	m := (*memStore)(r)
	if m.errAuditList != nil {
		return nil, false, m.errAuditList
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.AuditLog
	for i := len(m.auditLogs) - 1; i >= 0 && len(out) < f.Limit; i-- {
		l := m.auditLogs[i]
		if f.BeforeID > 0 && l.ID >= f.BeforeID {
			continue
		}
		if f.Action != "" && l.Action != f.Action {
			continue
		}
		if f.ResourceType != "" && l.ResourceType != f.ResourceType {
			continue
		}
		if f.ResourceID != "" && l.ResourceID != f.ResourceID {
			continue
		}
		if f.ConversationID != "" && (l.ConversationID == nil || *l.ConversationID != f.ConversationID) {
			continue
		}
		out = append(out, l)
	}
	return out, false, nil
}
func (r *memAudit) LastHash(context.Context) (string, error) { return "", nil }

// ---------------------------------------------------------------------------
// 假 SSH / dbproxy 执行器
// ---------------------------------------------------------------------------

type fakeSSHExec struct {
	sshx.Executor
	mu     sync.Mutex
	testFn func(ctx context.Context, target sshx.Target) (sshx.TestResult, error)
	// execFn 可注入按命令路由的执行结果（四期容器测试）；nil 时回固定输出。
	execFn      func(ctx context.Context, target sshx.Target, req sshx.RunRequest) (sshx.RunResult, error)
	calls       []sshx.RunRequest
	invalidated []string
}

// callCount 线程安全读取当前执行次数（五期起 Dispatch 异步执行，
// 测试断言与执行 goroutine 并发）。
func (f *fakeSSHExec) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeSSHExec) Test(ctx context.Context, target sshx.Target) (sshx.TestResult, error) {
	return f.testFn(ctx, target)
}
func (f *fakeSSHExec) Execute(ctx context.Context, target sshx.Target, req sshx.RunRequest) (sshx.RunResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	f.mu.Unlock()
	if f.execFn != nil {
		return f.execFn(ctx, target, req)
	}
	return sshx.RunResult{ExitCode: 0, Combined: "ok"}, nil
}
func (f *fakeSSHExec) Invalidate(id string) { f.invalidated = append(f.invalidated, id) }
func (f *fakeSSHExec) Close() error         { return nil }

type fakeDBExec struct {
	dbproxy.Executor
	testFn  func(ctx context.Context, db store.Database, password string) (dbproxy.TestResult, error)
	queryFn func(ctx context.Context, db store.Database, password string, req dbproxy.QueryRequest) (dbproxy.QueryResult, error)
	calls   []dbproxy.QueryRequest
}

func (f *fakeDBExec) Test(ctx context.Context, db store.Database, password string) (dbproxy.TestResult, error) {
	return f.testFn(ctx, db, password)
}
func (f *fakeDBExec) Query(ctx context.Context, db store.Database, password string, req dbproxy.QueryRequest) (dbproxy.QueryResult, error) {
	f.calls = append(f.calls, req)
	return f.queryFn(ctx, db, password, req)
}
func (f *fakeDBExec) Invalidate(string) {}
func (f *fakeDBExec) Close() error      { return nil }

// stubLLMClient 给 agent.Runner 注入的假 LLM 客户端。
type stubLLMClient struct {
	fn func(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) error
}

func (c *stubLLMClient) Stream(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
	return c.fn(ctx, req, emit)
}
func (c *stubLLMClient) ListModels(context.Context) ([]string, error) {
	return []string{"test-model"}, nil
}
func (c *stubLLMClient) Test(context.Context) error { return nil }

// finalAnswerLLM 一次性输出固定回答。
func finalAnswerLLM(text string) *stubLLMClient {
	return &stubLLMClient{fn: func(_ context.Context, _ llm.Request, emit func(llm.StreamEvent) error) error {
		if text != "" {
			if err := emit(llm.StreamEvent{Type: llm.EventToken, Delta: text}); err != nil {
				return err
			}
		}
		return emit(llm.StreamEvent{Type: llm.EventDone, FinishReason: "stop", Usage: &llm.Usage{PromptTokens: 2, CompletionTokens: 1, TotalTokens: 3}})
	}}
}

// ---------------------------------------------------------------------------
// 测试环境装配
// ---------------------------------------------------------------------------

const (
	testOrigin   = "http://localhost:5173"
	testPassword = "correct horse battery staple"
)

var (
	adminHashOnce sync.Once
	adminHash     string
)

func getAdminHash() string {
	adminHashOnce.Do(func() {
		hash, err := bcrypt.GenerateFromPassword([]byte(testPassword), bcrypt.MinCost)
		if err != nil {
			panic(err)
		}
		adminHash = string(hash)
	})
	return adminHash
}

type testEnv struct {
	t         *testing.T
	st        *memStore
	cipher    *cryptox.Cipher
	sshExec   *fakeSSHExec
	netExec   *fakeNetCLI
	dbExec    *fakeDBExec
	llmReg    *llm.Registry
	auditW    *audit.Writer
	authSvc   *auth.Service
	apprSvc   *approvals.Service
	streams   *agent.StreamGuard
	router    *gin.Engine
	cookie    string
	llmClient atomicLLMClient
}

// atomicLLMClient 允许在装配后替换注入 agent 的假客户端。
type atomicLLMClient struct {
	mu sync.Mutex
	c  llm.Client
}

func (a *atomicLLMClient) get() llm.Client {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.c
}
func (a *atomicLLMClient) set(c llm.Client) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.c = c
}

func (a *atomicLLMClient) Stream(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) error {
	return a.get().Stream(ctx, req, emit)
}
func (a *atomicLLMClient) ListModels(ctx context.Context) ([]string, error) {
	return a.get().ListModels(ctx)
}
func (a *atomicLLMClient) Test(ctx context.Context) error { return a.get().Test(ctx) }

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)

	st := newMemStore()
	st.users["admin"] = store.User{
		ID: "user-1", Username: "admin", PasswordHash: getAdminHash(),
		CreatedAt: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC),
	}

	cipher, err := cryptox.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	sshExec := &fakeSSHExec{testFn: func(context.Context, sshx.Target) (sshx.TestResult, error) {
		return sshx.TestResult{LatencyMS: 7, ServerVersion: "OpenSSH_9.6"}, nil
	}}
	dbExec := &fakeDBExec{
		testFn: func(context.Context, store.Database, string) (dbproxy.TestResult, error) {
			return dbproxy.TestResult{LatencyMS: 3, ServerVersion: "PostgreSQL 16.2"}, nil
		},
		queryFn: func(_ context.Context, _ store.Database, _ string, _ dbproxy.QueryRequest) (dbproxy.QueryResult, error) {
			return dbproxy.QueryResult{Columns: []dbproxy.Column{{Name: "id", Type: "int8"}}, Rows: [][]any{{int64(7)}}, RowCount: 1}, nil
		},
	}

	auditW := audit.NewWriter(st, log, time.Now)
	auditW.Start(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = auditW.Close(ctx)
	})

	authSvc := auth.NewService(st, auth.Config{SessionTTL: time.Hour})
	apprSvc := approvals.NewService(st, auditW, time.Hour, time.Now, log)
	apprSvc.Start(context.Background())
	t.Cleanup(apprSvc.Stop)

	llmReg := llm.NewRegistry(st, cipher, &http.Client{Timeout: 2 * time.Second}, log)
	streams := agent.NewStreamGuard(20)

	netExec := &fakeNetCLI{}
	env := &testEnv{
		t: t, st: st, cipher: cipher, sshExec: sshExec, netExec: netExec, dbExec: dbExec,
		llmReg: llmReg, auditW: auditW, authSvc: authSvc, apprSvc: apprSvc, streams: streams,
	}
	env.llmClient.set(finalAnswerLLM("assistant reply"))

	runner := agent.NewRunner(st, llmReg, sshExec, dbExec, apprSvc, auditW, cipher,
		agent.RunnerConfig{StreamMaxDuration: time.Minute}, time.Now, log)
	runner.SetLLMClient(&env.llmClient)
	runner.SetNetCLIExecutor(netExec)

	cfg := &config.Config{
		AllowedOrigins: []string{testOrigin},
		PublicOrigin:   testOrigin,
		WebDir:         "",
	}
	env.router = NewRouter(&Deps{
		Config:    cfg,
		Log:       log,
		Store:     st,
		Cipher:    cipher,
		Auth:      authSvc,
		Limiter:   auth.NewLoginLimiter(1000, time.Second, time.Now),
		Approvals: apprSvc,
		LLM:       llmReg,
		Agent:     runner,
		SSH:       sshExec,
		NetCLI:    netExec,
		DB:        dbExec,
		Audit:     auditW,
		Streams:   streams,
		Clock:     time.Now,
		Version:   "test",
	})

	// 登录拿会话 cookie。
	env.cookie = env.login()
	return env
}

func (e *testEnv) login() string {
	e.t.Helper()
	body := fmt.Sprintf(`{"password":%q}`, testPassword)
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", testOrigin)
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		e.t.Fatalf("login status = %d, body = %s", rec.Code, rec.Body.String())
	}
	raw := rec.Header().Get("Set-Cookie")
	if !strings.Contains(raw, sessionCookieName+"=") {
		e.t.Fatalf("login Set-Cookie = %q", raw)
	}
	parts := strings.SplitN(raw, ";", 2)
	return strings.TrimPrefix(parts[0], sessionCookieName+"=")
}

// do 发起带会话与 Origin 的请求。
func (e *testEnv) do(method, path, body string) *httptest.ResponseRecorder {
	e.t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Origin", testOrigin)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: e.cookie})
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), dst); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
}

func errorBody(t *testing.T, rec *httptest.ResponseRecorder) (code, message string) {
	t.Helper()
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	decodeBody(t, rec, &body)
	return body.Error.Code, body.Error.Message
}

// httptestPost 构造直接调用 handler 用的 JSON POST 请求。
func httptestPost(path, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// httptestGet 构造直接调用 handler 用的 GET 请求。
func httptestGet(path string) *http.Request {
	return httptest.NewRequest(http.MethodGet, path, nil)
}

// withParam 构造带路径参数的测试上下文。
func withParam(req *http.Request, key, value string) (*gin.Context, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	c.Params = gin.Params{{Key: key, Value: value}}
	return c, rec
}
