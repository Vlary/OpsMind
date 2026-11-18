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
	"github.com/google/uuid"
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
	diagnosis     map[string]store.DiagnosisSession
	auditLogs     []store.AuditLog
	// 一次性 invite/reset token（user_tokens）。放在 store 内而不是包级
	// 全局变量，才能随 WithTx 快照回滚、不在测试间串味。
	tokens      map[string]store.UserToken
	tokenHashes map[string]string

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
	errApprovalsGet     error
	errApprovalsDecide  error
	errAuditList        error

	// audit_outbox 测试通道（EnqueueAuditOutbox/Pending/Process）。
	outbox    []store.AuditOutboxEntry
	outboxSeq int64
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
		diagnosis:     map[string]store.DiagnosisSession{},
		tokens:        map[string]store.UserToken{},
		tokenHashes:   map[string]string{},
	}
}

func (m *memStore) Ping(context.Context) error { return m.pingErr }
func (m *memStore) Close()                     {}

// ---- audit_outbox（store.AuditOutboxStore） ----

func (m *memStore) EnqueueAuditOutbox(_ context.Context, payload []byte, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.outboxSeq++
	m.outbox = append(m.outbox, store.AuditOutboxEntry{
		ID: m.outboxSeq, Payload: append([]byte(nil), payload...), CreatedAt: at,
	})
	return nil
}

func (m *memStore) PendingAuditOutbox(_ context.Context, limit int) ([]store.AuditOutboxEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	out := make([]store.AuditOutboxEntry, 0, limit)
	for _, e := range m.outbox {
		if e.Attempts > 0 {
			continue
		}
		out = append(out, e)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (m *memStore) ProcessAuditOutbox(_ context.Context, ids []int64, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	want := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		want[id] = struct{}{}
	}
	for i := range m.outbox {
		if _, ok := want[m.outbox[i].ID]; ok {
			m.outbox[i].Attempts++
		}
	}
	return nil
}

func (m *memStore) LockAuditChain(context.Context) error { return nil }

// memRevoke 实现 store.RevokeStore（stub）：把该用户 pending 审批收敛为
// expired，并同步 awaiting_approval tool_call。测试用内存实现不做 SQL 联接。
// 返回受影响的行（与 postgres 实现同契约，调用方逐行写审计）。
func (r *memApprovals) ExpirePendingByUser(_ context.Context, userID, note string, now time.Time) ([]store.Approval, error) {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	expired := make([]store.Approval, 0, len(m.approvals))
	for id, ap := range m.approvals {
		if ap.Status != store.ApprovalPending || ap.RequestedBy != userID {
			continue
		}
		ap.Status = store.ApprovalExpired
		ap.Note = note
		at := now
		ap.DecidedAt = &at
		m.approvals[id] = ap
		expired = append(expired, ap)
		if ap.ToolCallID == nil {
			continue
		}
		for i := range m.toolCalls {
			tc := &m.toolCalls[i]
			if tc.ID == *ap.ToolCallID && tc.Status == store.ToolAwaitingApproval {
				tc.Status = store.ToolExpired
				tc.ErrorCode = "APPROVAL_CANCELLED"
				tc.ErrorMessage = "approval cancelled: requester account revoked"
			}
		}
	}
	return expired, nil
}

// ExpireApprovedGrantsByUser 收敛已批准未消费的 terminal grant（stub）。
func (r *memApprovals) ExpireApprovedGrantsByUser(_ context.Context, userID, note string, _ time.Time) ([]store.Approval, error) {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	expired := make([]store.Approval, 0, len(m.approvals))
	for id, ap := range m.approvals {
		if ap.Source == "terminal" && ap.Status == store.ApprovalApproved &&
			ap.ConsumedAt == nil && ap.RequestedBy == userID {
			ap.Status = store.ApprovalExpired
			ap.Note = note
			m.approvals[id] = ap
			expired = append(expired, ap)
		}
	}
	return expired, nil
}

// ExpireStaleGrants 收敛已批准但已过 grant_expires_at 的 grant（stub）。
func (r *memApprovals) ExpireStaleGrants(_ context.Context, now time.Time, limit int) ([]store.Approval, error) {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit <= 0 {
		limit = 200
	}
	expired := make([]store.Approval, 0, limit)
	for id, ap := range m.approvals {
		if len(expired) >= limit {
			break
		}
		if ap.Source != "terminal" || ap.Status != store.ApprovalApproved || ap.ConsumedAt != nil {
			continue
		}
		if ap.GrantExpiresAt == nil || ap.GrantExpiresAt.After(now) {
			continue
		}
		ap.Status = store.ApprovalExpired
		ap.Note = "grant_expired"
		at := now
		ap.DecidedAt = &at
		m.approvals[id] = ap
		expired = append(expired, ap)
	}
	return expired, nil
}

// WithTx 模拟事务：先在内存里做快照，fn 返回错误即整体回滚。测试用它
// 覆盖「投递失败 → 用户行/token/审计一并撤销」这类真实事务语义（内存
// fake 不做快照的话，回滚路径无处验证）。
func (m *memStore) WithTx(ctx context.Context, fn func(store.Store) error) error {
	m.mu.Lock()
	snap := m.snapshotLocked()
	m.mu.Unlock()
	if err := fn(m); err != nil {
		m.mu.Lock()
		m.restoreLocked(snap)
		m.mu.Unlock()
		return err
	}
	return nil
}

// memSnapshot 是 WithTx 的回滚快照（浅拷贝值类型 map/slice）。
type memSnapshot struct {
	users, sessions, settings, servers, databases, providers, conversations map[string]any
	toolCalls                                                               []store.ToolCall
	approvals                                                               map[string]store.Approval
	auditLogs                                                               []store.AuditLog
	tokens                                                                  map[string]store.UserToken
	tokenHashes                                                             map[string]string
	outbox                                                                  []store.AuditOutboxEntry
	outboxSeq                                                               int64
}

func (m *memStore) snapshotLocked() memSnapshot {
	snap := memSnapshot{outboxSeq: m.outboxSeq}
	snap.users = make(map[string]any, len(m.users))
	for k, v := range m.users {
		snap.users[k] = v
	}
	snap.sessions = make(map[string]any, len(m.sessions))
	for k, v := range m.sessions {
		snap.sessions[k] = v
	}
	snap.settings = make(map[string]any, len(m.settings))
	for k, v := range m.settings {
		snap.settings[k] = v
	}
	snap.servers = make(map[string]any, len(m.servers))
	for k, v := range m.servers {
		snap.servers[k] = v
	}
	snap.databases = make(map[string]any, len(m.databases))
	for k, v := range m.databases {
		snap.databases[k] = v
	}
	snap.providers = make(map[string]any, len(m.providers))
	for k, v := range m.providers {
		snap.providers[k] = v
	}
	snap.conversations = make(map[string]any, len(m.conversations))
	for k, v := range m.conversations {
		snap.conversations[k] = v
	}
	snap.toolCalls = append([]store.ToolCall(nil), m.toolCalls...)
	snap.approvals = make(map[string]store.Approval, len(m.approvals))
	for k, v := range m.approvals {
		snap.approvals[k] = v
	}
	snap.auditLogs = append([]store.AuditLog(nil), m.auditLogs...)
	snap.tokens = make(map[string]store.UserToken, len(m.tokens))
	for k, v := range m.tokens {
		snap.tokens[k] = v
	}
	snap.tokenHashes = make(map[string]string, len(m.tokenHashes))
	for k, v := range m.tokenHashes {
		snap.tokenHashes[k] = v
	}
	snap.outbox = append([]store.AuditOutboxEntry(nil), m.outbox...)
	return snap
}

func (m *memStore) restoreLocked(snap memSnapshot) {
	m.users = make(map[string]store.User, len(snap.users))
	for k, v := range snap.users {
		m.users[k] = v.(store.User)
	}
	m.sessions = make(map[string]store.Session, len(snap.sessions))
	for k, v := range snap.sessions {
		m.sessions[k] = v.(store.Session)
	}
	m.settings = make(map[string]store.Setting, len(snap.settings))
	for k, v := range snap.settings {
		m.settings[k] = v.(store.Setting)
	}
	m.servers = make(map[string]store.Server, len(snap.servers))
	for k, v := range snap.servers {
		m.servers[k] = v.(store.Server)
	}
	m.databases = make(map[string]store.Database, len(snap.databases))
	for k, v := range snap.databases {
		m.databases[k] = v.(store.Database)
	}
	m.providers = make(map[string]store.Provider, len(snap.providers))
	for k, v := range snap.providers {
		m.providers[k] = v.(store.Provider)
	}
	m.conversations = make(map[string]store.Conversation, len(snap.conversations))
	for k, v := range snap.conversations {
		m.conversations[k] = v.(store.Conversation)
	}
	m.toolCalls = snap.toolCalls
	m.approvals = snap.approvals
	m.auditLogs = snap.auditLogs
	m.tokens = snap.tokens
	m.tokenHashes = snap.tokenHashes
	m.outbox = snap.outbox
	m.outboxSeq = snap.outboxSeq
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

// 五期 M2（diagnosis/inspection）向 store.Store 增挂子接口：inspection 的
// 真实替换见 inspection_handlers_test.go（inspectionState）；diagnosis 在
// 第 11 轮走查 medium 补成内存实现——列表归属过滤与 GET 的 owner-or-admin
// 必须能在 handler 层用真实行验证（此前是触达即 panic 的占位）。
func (m *memStore) DiagnosisSessions() store.DiagnosisSessionStore {
	return (*memDiagnosisSessions)(m)
}

type memDiagnosisSessions memStore

func (r *memDiagnosisSessions) Create(_ context.Context, p store.CreateDiagnosisParams) (store.DiagnosisSession, error) {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	if p.ExpiresAt.IsZero() {
		return store.DiagnosisSession{}, store.ErrInvalid
	}
	name := ""
	if srv, ok := m.servers[p.ServerID]; ok {
		name = srv.Name
	}
	d := store.DiagnosisSession{
		ID: m.nextID("diag"), AlertEventID: p.AlertEventID, ServerID: p.ServerID,
		ServerName: name, ConversationID: p.ConversationID, Trigger: p.Trigger,
		Status: store.DiagnosisPending, ExpiresAt: p.ExpiresAt.UTC(),
		CreatedBy: p.CreatedBy, CreatedAt: now, UpdatedAt: now,
	}
	m.diagnosis[d.ID] = d
	return d, nil
}

func (r *memDiagnosisSessions) Get(_ context.Context, id string) (store.DiagnosisSession, error) {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.diagnosis[id]
	if !ok {
		return store.DiagnosisSession{}, store.ErrNotFound
	}
	return d, nil
}

func (r *memDiagnosisSessions) List(_ context.Context, status store.DiagnosisStatus, serverID, createdBy string, limit int) ([]store.DiagnosisSession, error) {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]store.DiagnosisSession, 0, len(m.diagnosis))
	for _, d := range m.diagnosis {
		if status != "" && d.Status != status {
			continue
		}
		if serverID != "" && d.ServerID != serverID {
			continue
		}
		// 与 pgstore 同语义：created_by IS NULL 的 system 行不满足等值过滤。
		if createdBy != "" && (d.CreatedBy == nil || *d.CreatedBy != createdBy) {
			continue
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *memDiagnosisSessions) UpdateStatus(_ context.Context, p store.UpdateDiagnosisStatusParams) (store.DiagnosisSession, error) {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.diagnosis[p.ID]
	if !ok {
		return store.DiagnosisSession{}, store.ErrNotFound
	}
	if p.From != "" && d.Status != p.From {
		return store.DiagnosisSession{}, store.ErrConflict
	}
	d.Status = p.To
	d.UpdatedAt = p.Timestamp.UTC()
	if p.Report != nil {
		d.Report = append(json.RawMessage(nil), p.Report...)
	}
	if p.ErrorCode != "" {
		d.ErrorCode = p.ErrorCode
	}
	if p.To == store.DiagnosisDiagnosing {
		at := p.Timestamp.UTC()
		d.StartedAt = &at
	}
	if p.To == store.DiagnosisAwaitingFix {
		// 与 postgres 同口径：进入 awaiting_fix 重置 expires_at=timestamp+30m。
		d.ExpiresAt = p.Timestamp.UTC().Add(30 * time.Minute)
	}
	switch p.To {
	case store.DiagnosisDone, store.DiagnosisExpired, store.DiagnosisFailed:
		at := p.Timestamp.UTC()
		d.FinishedAt = &at
	}
	m.diagnosis[p.ID] = d
	return d, nil
}

func (r *memDiagnosisSessions) ExpireStale(_ context.Context, now time.Time) (int64, error) {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for id, d := range m.diagnosis {
		if d.Status != store.DiagnosisAwaitingFix || d.ExpiresAt.After(now) {
			continue
		}
		d.Status = store.DiagnosisExpired
		at := now.UTC()
		d.FinishedAt = &at
		d.UpdatedAt = at
		m.diagnosis[id] = d
		n++
	}
	return n, nil
}

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
	role := p.Role
	if role == "" {
		role = store.RoleOperator
	}
	u := store.User{
		ID: m.nextID("user"), Username: p.Username, PasswordHash: p.PasswordHash,
		Role: role, Enabled: p.Enabled, DisplayName: p.DisplayName, Email: p.Email,
		MustChangePassword: p.MustChangePassword, CreatedBy: p.CreatedBy,
		CreatedAt: now, UpdatedAt: now,
	}
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
func (r *memSessions) Get(_ context.Context, id string) (store.Session, error) {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return store.Session{}, store.ErrNotFound
	}
	return s, nil
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

func (r *memConversations) ListPage(_ context.Context, userID string, cursor store.ConversationListCursor, limit int) ([]store.Conversation, bool, error) {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]store.Conversation, 0, len(m.conversations))
	for _, c := range m.conversations {
		// 与生产 pgstore 同语义：user_id 过滤先于排序/分页（等价于 SQL 的
		// WHERE 在 LIMIT 之前），否则他人会话会占满窗口导致翻页卡死。
		if userID != "" && c.UserID != userID {
			continue
		}
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
	c := store.Conversation{ID: m.nextID("conv"), Title: p.Title, UserID: p.UserID, CreatedAt: now, UpdatedAt: now, LastMessageAt: now}
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
		// 与生产 pgstore / 契约 12.3 第 3 条同语义：非 audit:read 的目录在
		// SQL 侧强制 user_id=自己（fake 在收集阶段先过滤，等价于 WHERE）。
		if p.UserID != "" && c.UserID != p.UserID {
			continue
		}
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
		ID: uuid.NewString(), ConversationID: p.ConversationID, ToolCallID: p.ToolCallID,
		Source:   source,
		ServerID: p.ServerID, ServerName: p.ServerName, Command: p.Command,
		RiskLevel: p.RiskLevel, RuleID: p.RuleID, Reason: p.Reason,
		ApprovalMode: store.ApprovalMode(p.ApprovalMode),
		RequestedBy:  p.RequestedBy,
		Status:       store.ApprovalPending, RequestedAt: now, ExpiresAt: now.Add(time.Hour),
		GrantKind: p.GrantKind, GrantExpiresAt: p.GrantExpiresAt,
		PolicyFrom: p.PolicyFrom, PolicyTo: p.PolicyTo,
	}
	if !p.ExpiresAt.IsZero() {
		ap.ExpiresAt = p.ExpiresAt
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
	if m.errApprovalsGet != nil {
		return store.Approval{}, m.errApprovalsGet
	}
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

// Expire 与生产 pgstore 同语义：pending 且 expires_at<=now → expired，
// 返回受影响行数（第 4 轮走查 medium 的 outbox 同事务测试需要真实计数）。
func (r *memApprovals) Expire(_ context.Context, now time.Time) (int64, error) {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for id, ap := range m.approvals {
		if ap.Status == store.ApprovalPending && !ap.ExpiresAt.After(now) {
			ap.Status = store.ApprovalExpired
			m.approvals[id] = ap
			n++
		}
	}
	return n, nil
}

func (r *memApprovals) ConsumeGrant(_ context.Context, p store.ConsumeGrantParams) (store.Approval, error) {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	match := func(ap store.Approval) error {
		if ap.Source != "terminal" || ap.GrantKind == nil || *ap.GrantKind != p.GrantKind {
			return store.ErrGrantKindMismatch
		}
		if ap.Status != store.ApprovalApproved || ap.ConsumedAt != nil ||
			ap.GrantExpiresAt == nil || !ap.GrantExpiresAt.After(p.Now) ||
			ap.RequestedBy != p.UserID || ap.ServerID == nil || *ap.ServerID != p.ServerID {
			return store.ErrGrantUnavailable
		}
		return nil
	}
	if p.ApprovalID != "" {
		ap, ok := m.approvals[p.ApprovalID]
		if !ok {
			return store.Approval{}, store.ErrGrantUnavailable
		}
		if err := match(ap); err != nil {
			return store.Approval{}, err
		}
		now := p.Now
		ap.ConsumedAt = &now
		m.approvals[ap.ID] = ap
		return ap, nil
	}
	// 空 id：消费最新一条符合条件（与 postgres ORDER BY decided_at DESC 同义）。
	var best *store.Approval
	for _, ap := range m.approvals {
		if match(ap) != nil {
			continue
		}
		if best == nil || (ap.DecidedAt != nil && (best.DecidedAt == nil || ap.DecidedAt.After(*best.DecidedAt))) {
			copied := ap
			best = &copied
		}
	}
	if best == nil {
		return store.Approval{}, store.ErrGrantUnavailable
	}
	now := p.Now
	best.ConsumedAt = &now
	m.approvals[best.ID] = *best
	return *best, nil
}

// ReleaseGrant 与生产 pgstore 同语义：仅回滚本次消费（consumed_at 精确
// 匹配）且 requested_by=调用者，否则 ErrGrantUnavailable。
func (r *memApprovals) ReleaseGrant(_ context.Context, p store.ReleaseGrantParams) (store.Approval, error) {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	ap, ok := m.approvals[p.ApprovalID]
	if !ok || ap.Status != store.ApprovalApproved || ap.ConsumedAt == nil ||
		!ap.ConsumedAt.Equal(p.ConsumedAt) || ap.RequestedBy != p.UserID {
		return store.Approval{}, store.ErrGrantUnavailable
	}
	ap.ConsumedAt = nil
	m.approvals[ap.ID] = ap
	return ap, nil
}

func (r *memApprovals) ExpirePendingGrantsByUser(_ context.Context, userID string, now time.Time) ([]store.Approval, error) {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	expired := make([]store.Approval, 0, len(m.approvals))
	for id, ap := range m.approvals {
		if ap.Source == "terminal" && ap.Status == store.ApprovalPending && ap.RequestedBy == userID {
			ap.Status = store.ApprovalExpired
			ap.Note = "rbac:user_revoked"
			at := now
			ap.DecidedAt = &at
			m.approvals[id] = ap
			expired = append(expired, ap)
		}
	}
	return expired, nil
}

// ListAvailableGrants 与生产 pgstore 同语义：只返回本人 approved、未消费、
// 未过期（grant_expires_at > now）的 terminal grant，按最先到期排序。
func (r *memApprovals) ListAvailableGrants(_ context.Context, p store.ListAvailableGrantsParams) ([]store.Approval, error) {
	if p.UserID == "" {
		return nil, store.ErrInvalid
	}
	if p.GrantKind != "" && p.GrantKind != "term" && p.GrantKind != "rdp" {
		return nil, store.ErrInvalid
	}
	limit := p.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		return nil, store.ErrInvalid
	}
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]store.Approval, 0, len(m.approvals))
	for _, ap := range m.approvals {
		if ap.Source != "terminal" || ap.Status != store.ApprovalApproved ||
			ap.ConsumedAt != nil || ap.GrantExpiresAt == nil ||
			!ap.GrantExpiresAt.After(p.Now) || ap.RequestedBy != p.UserID {
			continue
		}
		if p.GrantKind != "" && (ap.GrantKind == nil || *ap.GrantKind != p.GrantKind) {
			continue
		}
		out = append(out, ap)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].GrantExpiresAt.Before(*out[j].GrantExpiresAt)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

type memAudit memStore

func (r *memAudit) Append(_ context.Context, p store.CreateAuditParams) error {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	createdAt := p.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	m.auditLogs = append(m.auditLogs, store.AuditLog{
		ID: int64(len(m.auditLogs) + 1), CreatedAt: createdAt, ActorType: p.ActorType,
		ActorID: p.ActorID, Action: p.Action, ResourceType: p.ResourceType,
		ResourceID: p.ResourceID, RequestID: p.RequestID, ConversationID: p.ConversationID,
		ServerID: p.ServerID, IP: p.IP, Detail: p.Detail, PrevHash: p.PrevHash, Hash: p.Hash,
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
func (r *memAudit) LastHash(context.Context) (string, error) {
	m := (*memStore)(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.auditLogs) == 0 {
		return audit.ZeroHash, nil
	}
	return m.auditLogs[len(m.auditLogs)-1].Hash, nil
}

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
	t       *testing.T
	st      *memStore
	cipher  *cryptox.Cipher
	sshExec *fakeSSHExec
	netExec *fakeNetCLI
	dbExec  *fakeDBExec
	llmReg  *llm.Registry
	auditW  *audit.Writer
	authSvc *auth.Service
	apprSvc *approvals.Service
	streams *agent.StreamGuard
	router  *gin.Engine
	cookie  string
	// reviewerCookie 是 approver 角色的独立会话：审批职责分离下，
	// admin 发起的审批不能由 admin 自己批准。懒创建，避免污染用户列表用例。
	reviewerCookie string
	llmClient      atomicLLMClient
}

// seedSecondAdmin 预置第二个启用管理员：默认测试环境代表「非降级部署」
// （契约 12.3 第 11 条：仅一个启用管理员时不得审批/开终端/放宽 policy）。
// 单管理员降级模式的用例显式停用它。
func seedSecondAdmin(st *memStore) {
	st.users[testSecondAdminUsername] = store.User{
		ID: testSecondAdminID, Username: testSecondAdminUsername, PasswordHash: getAdminHash(),
		Role: store.RoleAdmin, Enabled: true,
		CreatedAt: time.Date(2026, 10, 24, 11, 0, 0, 0, time.UTC),
	}
}

// RBAC 测试身份。
const (
	testSecondAdminUsername = "admin-second"
	testSecondAdminID       = "user-second-admin"
	testReviewerUsername    = "reviewer"
	testRequesterID         = "requester-1"
	testRequesterUsername   = "requester"
)

// ensureRequester 插入固定 ID 的发起人（历史用例以 "requester-1" 造审批）。
func (e *testEnv) ensureRequester(t *testing.T) {
	t.Helper()
	e.st.mu.Lock()
	defer e.st.mu.Unlock()
	if _, ok := e.st.users[testRequesterUsername]; ok {
		return
	}
	e.st.users[testRequesterUsername] = store.User{
		ID: testRequesterID, Username: testRequesterUsername, PasswordHash: getAdminHash(),
		Role: store.RoleOperator, Enabled: true,
		CreatedAt: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC),
	}
}

// approverCookie 返回 approver 角色的会话（懒创建：先加用户再登录）。
func (e *testEnv) approverCookie(t *testing.T) string {
	t.Helper()
	if e.reviewerCookie != "" {
		return e.reviewerCookie
	}
	addTestUser(t, e, testReviewerUsername, testPassword, store.RoleApprover, true)
	e.reviewerCookie = loginAs(t, e, testReviewerUsername, testPassword)
	return e.reviewerCookie
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
		Role: store.RoleAdmin, Enabled: true,
		CreatedAt: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC),
	}
	seedSecondAdmin(st)

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
	body := fmt.Sprintf(`{"username":"admin","password":%q}`, testPassword)
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
	return e.doAs(e.cookie, method, path, body)
}

// doAs 以指定会话发起请求（审批人用 reviewerCookie）。
func (e *testEnv) doAs(cookie, method, path, body string) *httptest.ResponseRecorder {
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
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookie})
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

// withTestAdminIdentity 在只测 handler 的中间件链里注入已认证 admin 身份。
//
// RBAC（00012）后，Register*Routes 注册的端点都带 d.guard 权限中间件；
// 这些单元测试直接挂 handler，不经过 NewRouter 的 requireAuth，因此必须
// 显式提供 user/user_id 上下文，否则守卫按未认证返回 401。这不是放宽
// 生产鉴权：真实路由仍由 requireAuth 从会话注入身份。
func withTestAdminIdentity() gin.HandlerFunc {
	return func(c *gin.Context) {
		user := store.User{
			ID: "user-1", Username: "admin", Role: store.RoleAdmin, Enabled: true,
		}
		// 职责分离：审批决定必须由另一用户发起；单元测试用请求头切换
		// 身份（真实路由仍是 requireAuth 从会话注入，不受影响）。
		if c.GetHeader(testIdentityHeader) == string(store.RoleApprover) {
			user = store.User{
				ID: "user-2", Username: "reviewer", Role: store.RoleApprover, Enabled: true,
			}
		}
		c.Set("user", user)
		c.Set("user_id", user.ID)
		c.Next()
	}
}

// testIdentityHeader 仅在 handler 单元测试的假中间件里使用。
const testIdentityHeader = "X-Test-Identity"

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
