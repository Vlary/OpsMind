// Command smoke 是云枢 OpsMind 的独立 HTTP 冒烟程序（CONTRACT.md 9.3）。
//
// 它不依赖外部已启动的 app：解析 TEST_DATABASE_URL（仅允许本机 65432），
// 在独立数据库 aiops_smoke 上执行迁移、清空业务表、创建固定密码管理员，
// 启动本地假 OpenAI 兼容供应商，用 httptest 暴露真实路由，然后按 S01–S18
// 全部通过 HTTP 观察应用行为。结束时关闭资源并删除 aiops_smoke。
//
// 测试库与主库分离：冒烟库固定为 <host>:<port>/aiops_smoke，绝不触碰
// aiops（主库）或生产库；TEST_DATABASE_URL 只用于取连接参数与主机白名单校验。
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"aiops/internal/agent"
	"aiops/internal/approvals"
	"aiops/internal/audit"
	"aiops/internal/auth"
	"aiops/internal/config"
	"aiops/internal/cryptox"
	"aiops/internal/dbproxy"
	"aiops/internal/httpapi"
	"aiops/internal/llm"
	"aiops/internal/logx"
	"aiops/internal/sshx"
	"aiops/internal/store/postgres"
)

const (
	// defaultTestDSN 与 CONTRACT.md 9.1 一致；数据库名会被替换为 smokeDBName。
	defaultTestDSN = "postgres://aiops:aiops@127.0.0.1:65432/aiops?sslmode=disable"
	// smokeDBName 是冒烟专用独立数据库，每次运行结束删除。
	smokeDBName = "aiops_smoke"
	// testDBPort 是契约固定的测试库端口，其他端口一律拒绝（防误清生产库）。
	testDBPort = "65432"

	serverPassword = "smoke-ssh-password"
	providerAPIKey = "sk-smoke-abcd1234"

	healthSamples = 100
	p95Budget     = 500 * time.Millisecond
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "SMOKE_FAILED: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("SMOKE_OK")
}

func run() (err error) {
	adminDSN, smokeDSN, err := resolveSmokeDSNs()
	if err != nil {
		return err
	}

	ctx := context.Background()
	if err := ensureDatabase(ctx, adminDSN, smokeDBName); err != nil {
		return err
	}

	env, err := setupEnv(ctx, smokeDSN, adminDSN)
	if err != nil {
		// setup 可能只完成了一部分，仍尝试删除本次创建的测试库。
		_ = dropDatabase(adminDSN, smokeDBName)
		return err
	}
	defer func() {
		if cerr := env.Close(); cerr != nil {
			fmt.Fprintf(os.Stderr, "SMOKE_CLEANUP_WARNING: %v\n", cerr)
		}
	}()

	s := &smokeRun{h: newSmokeHTTP(env.server.URL), mock: env.mock}
	defer func() {
		if s.sqlitePath != "" {
			_ = os.Remove(s.sqlitePath)
		}
	}()
	return s.runAll()
}

// resolveSmokeDSNs 校验 TEST_DATABASE_URL 的 host/port，并把库名固定为 aiops_smoke。
func resolveSmokeDSNs() (adminDSN, smokeDSN string, err error) {
	raw := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if raw == "" {
		raw = defaultTestDSN
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", fmt.Errorf("TEST_DATABASE_URL is invalid: %w", err)
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return "", "", fmt.Errorf("TEST_DATABASE_URL scheme must be postgres, got %q", u.Scheme)
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	if host != "127.0.0.1" && host != "localhost" {
		return "", "", fmt.Errorf("refusing to use non-local test database host %q", host)
	}
	if port != testDBPort {
		return "", "", fmt.Errorf("refusing to use test database port %q (must be %s)", port, testDBPort)
	}
	if u.User == nil || u.User.Username() == "" {
		return "", "", errors.New("TEST_DATABASE_URL must include a username")
	}

	admin := *u
	admin.Path = "/postgres"
	smoke := *u
	smoke.Path = "/" + smokeDBName
	return admin.String(), smoke.String(), nil
}

func ensureDatabase(ctx context.Context, adminDSN, name string) error {
	conn, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return fmt.Errorf("connect maintenance database: %w", err)
	}
	defer conn.Close(context.Background())

	var exists bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&exists); err != nil {
		return fmt.Errorf("check database %s: %w", name, err)
	}
	if exists {
		return nil
	}
	if _, err := conn.Exec(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		return fmt.Errorf("create database %s: %w", name, err)
	}
	return nil
}

func dropDatabase(adminDSN, name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return fmt.Errorf("connect maintenance database for cleanup: %w", err)
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, `DROP DATABASE IF EXISTS "`+name+`" WITH (FORCE)`); err != nil {
		return fmt.Errorf("drop database %s: %w", name, err)
	}
	return nil
}

// smokeEnv 持有一次冒烟运行的全部资源，Close 按依赖逆序释放并删除测试库。
type smokeEnv struct {
	adminDSN string
	dataDir  string

	store     *postgres.Store
	audit     *audit.Writer
	approvals *approvals.Service
	ssh       *sshx.Pool
	db        *dbproxy.ExecutorPool
	registry  *llm.Registry
	mock      *mockLLM
	server    *httptest.Server
}

func setupEnv(ctx context.Context, smokeDSN, adminDSN string) (*smokeEnv, error) {
	dataDir, err := os.MkdirTemp("", "aiops-smoke-*")
	if err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	env := &smokeEnv{dataDir: dataDir, adminDSN: adminDSN}
	fail := func(err error) (*smokeEnv, error) {
		_ = env.Close()
		return nil, err
	}

	log := logx.New(slog.LevelError)
	cfg := &config.Config{
		Addr:              ":0",
		DatabaseURL:       smokeDSN,
		DataDir:           dataDir,
		AdminPassword:     smokeAdminPassword,
		AllowedOrigins:    []string{smokeOrigin},
		PublicOrigin:      smokeOrigin,
		SecureCookies:     false,
		TrustProxy:        false,
		WebDir:            "",
		LogLevel:          slog.LevelError,
		SessionTTL:        time.Hour,
		ApprovalTTL:       120 * time.Second,
		StreamMaxDuration: 30 * time.Minute,
		MaxStreams:        20,
	}

	masterKey, err := cryptox.LoadMasterKey(cfg.DataDir, cfg.MasterKeyB64)
	if err != nil {
		return fail(fmt.Errorf("load master key: %w", err))
	}
	cipher, err := cryptox.New(masterKey)
	if err != nil {
		return fail(fmt.Errorf("build cipher: %w", err))
	}

	startupCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	st, err := postgres.Open(startupCtx, cfg.DatabaseURL, log)
	if err != nil {
		return fail(fmt.Errorf("open test database: %w", err))
	}
	env.store = st

	if err := truncateBusinessTables(startupCtx, smokeDSN); err != nil {
		return fail(err)
	}

	clock := time.Now
	authSvc := auth.NewService(st, auth.Config{SessionTTL: cfg.SessionTTL, Clock: clock})
	if err := authSvc.EnsureAdmin(startupCtx, cfg.AdminPassword, io.Discard); err != nil {
		return fail(fmt.Errorf("create admin: %w", err))
	}

	env.mock = newMockLLM()
	env.audit = audit.NewWriter(st, log, clock)
	env.approvals = approvals.NewService(st, env.audit, cfg.ApprovalTTL, clock, log)
	env.registry = llm.NewRegistry(st, cipher, newLLMHTTPClient(), log)
	env.ssh = sshx.New(sshx.Config{}, log)
	env.db = dbproxy.New(dbproxy.Config{}, log)
	streams := agent.NewStreamGuard(cfg.MaxStreams)
	runner := agent.NewRunner(
		st, env.registry, env.ssh, env.db, env.approvals, env.audit, cipher,
		agent.RunnerConfig{StreamMaxDuration: cfg.StreamMaxDuration}, clock, log,
	)

	deps := &httpapi.Deps{
		Config:    cfg,
		Log:       log,
		Store:     st,
		Cipher:    cipher,
		Auth:      authSvc,
		Limiter:   auth.NewLoginLimiter(5, 12*time.Second, clock),
		Approvals: env.approvals,
		LLM:       env.registry,
		Agent:     runner,
		SSH:       env.ssh,
		DB:        env.db,
		Audit:     env.audit,
		Streams:   streams,
		Clock:     clock,
		Version:   "smoke",
	}

	env.server = httptest.NewServer(httpapi.NewRouter(deps))
	env.audit.Start(ctx)
	env.approvals.Start(ctx)
	return env, nil
}

// truncateBusinessTables 清空测试库业务表（仅 aiops_smoke）。
func truncateBusinessTables(ctx context.Context, smokeDSN string) error {
	conn, err := pgx.Connect(ctx, smokeDSN)
	if err != nil {
		return fmt.Errorf("connect test database for truncate: %w", err)
	}
	defer conn.Close(context.Background())
	const stmt = `TRUNCATE users,sessions,servers,databases,providers,conversations,` +
		`messages,tool_calls,approvals,audit_logs,settings RESTART IDENTITY CASCADE`
	if _, err := conn.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("truncate test database: %w", err)
	}
	return nil
}

func (e *smokeEnv) Close() error {
	var errs []error
	if e.server != nil {
		e.server.Close()
	}
	if e.approvals != nil {
		e.approvals.Stop()
	}
	if e.audit != nil {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := e.audit.Close(closeCtx); err != nil {
			errs = append(errs, err)
		}
		cancel()
	}
	if e.ssh != nil {
		_ = e.ssh.Close()
	}
	if e.db != nil {
		_ = e.db.Close()
	}
	if e.registry != nil {
		e.registry.Close()
	}
	if e.mock != nil {
		e.mock.Close()
	}
	if e.store != nil {
		e.store.Close()
	}
	if e.adminDSN != "" {
		if err := dropDatabase(e.adminDSN, smokeDBName); err != nil {
			errs = append(errs, err)
		}
	}
	if e.dataDir != "" {
		if err := os.RemoveAll(e.dataDir); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func newLLMHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 120 * time.Second,
			MaxIdleConns:          50,
			MaxIdleConnsPerHost:   20,
			IdleConnTimeout:       90 * time.Second,
		},
	}
}

// ---------------------------------------------------------------------------
// S01–S18
// ---------------------------------------------------------------------------

type smokeRun struct {
	h    *smokeHTTP
	mock *mockLLM

	serverID    string
	databaseID  string
	providerID  string
	providerID2 string
	alphaID     string
	bravoID     string
	noProvider  string
	sqlitePath  string
}

func (s *smokeRun) runAll() error {
	steps := []struct {
		name string
		fn   func() error
	}{
		{"S01 health warmup", s.step01Warmup},
		{"S02 health concurrency", s.step02ConcurrentHealth},
		{"S03 login invalid", s.step03LoginInvalid},
		{"S04 login success", s.step04LoginSuccess},
		{"S05 origin required", s.step05OriginRequired},
		{"S06 create server", s.step06CreateServer},
		{"S07 server read/update", s.step07ServerReadUpdate},
		{"S08 sqlite database", s.step08SQLiteDatabase},
		{"S09 create provider", s.step09CreateProvider},
		{"S10 provider test/models", s.step10ProviderTest},
		{"S11 risk preview", s.step11RiskPreview},
		{"S12 chat without provider", s.step12ChatNoProvider},
		{"S13 chat sse", s.step13ChatSSE},
		{"S14 conversation isolation", s.step14ConversationIsolation},
		{"S15 approval not found", s.step15ApprovalNotFound},
		{"S16 audit chain", s.step16AuditChain},
		{"S17 delete assets", s.step17DeleteAssets},
		{"S18 logout", s.step18Logout},
	}
	for _, step := range steps {
		if err := step.fn(); err != nil {
			return fmt.Errorf("%s: %w", step.name, err)
		}
	}
	return nil
}

type healthBody struct {
	Status   string `json:"status"`
	Version  string `json:"version"`
	Database string `json:"database"`
}

func (s *smokeRun) step01Warmup() error {
	for i := 0; i < 5; i++ {
		resp, err := s.h.do("S01", http.MethodGet, "/healthz", nil, false)
		if err != nil {
			return err
		}
		if err := expectStatus("S01", resp, http.StatusOK); err != nil {
			return err
		}
		var health healthBody
		if err := decodeJSON("S01", resp, &health); err != nil {
			return err
		}
		if health.Status != "ok" || health.Database != "ok" {
			return resp.fail("S01", "want status=ok database=ok, got status=%q database=%q", health.Status, health.Database)
		}
	}
	return nil
}

func (s *smokeRun) step02ConcurrentHealth() error {
	type sample struct {
		duration time.Duration
		status   int
		err      error
	}
	samples := make([]sample, healthSamples)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < healthSamples; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			begin := time.Now()
			resp, err := s.h.do("S02", http.MethodGet, "/healthz", nil, false)
			elapsed := time.Since(begin)
			if err != nil {
				samples[i] = sample{duration: elapsed, err: err}
				return
			}
			samples[i] = sample{duration: elapsed, status: resp.Status}
		}(i)
	}
	close(start)
	wg.Wait()

	durations := make([]time.Duration, 0, healthSamples)
	for i, sample := range samples {
		if sample.err != nil {
			return fmt.Errorf("S02: request %d failed: %w", i, sample.err)
		}
		if sample.status != http.StatusOK {
			return fmt.Errorf("S02: request %d returned status %d", i, sample.status)
		}
		durations = append(durations, sample.duration)
	}
	p := p95(durations)
	if p >= p95Budget {
		return fmt.Errorf("S02: p95 latency %s exceeds %s", p, p95Budget)
	}
	fmt.Printf("S02 health concurrency ok (p95=%s)\n", p)
	return nil
}

func (s *smokeRun) step03LoginInvalid() error {
	resp, err := s.h.do("S03", http.MethodPost, "/api/auth/login", map[string]any{"password": "wrong-password"}, false)
	if err != nil {
		return err
	}
	if err := expectErrorCode("S03", resp, http.StatusUnauthorized, "AUTH_INVALID"); err != nil {
		return err
	}
	e, _ := resp.decodeError()
	if e.Error.RequestID == "" {
		return resp.fail("S03", "error body has no request_id")
	}
	return nil
}

func (s *smokeRun) step04LoginSuccess() error {
	resp, err := s.h.do("S04", http.MethodPost, "/api/auth/login", map[string]any{"password": smokeAdminPassword}, false)
	if err != nil {
		return err
	}
	if err := expectStatus("S04", resp, http.StatusOK); err != nil {
		return err
	}
	cookies := strings.Join(resp.Header.Values("Set-Cookie"), "\n")
	for _, want := range []string{"HttpOnly", "SameSite=Lax", "Path=/"} {
		if !strings.Contains(cookies, want) {
			return resp.fail("S04", "Set-Cookie missing %q", want)
		}
	}

	me, err := s.h.do("S04", http.MethodGet, "/api/auth/me", nil, false)
	if err != nil {
		return err
	}
	if err := expectStatus("S04", me, http.StatusOK); err != nil {
		return err
	}
	var payload struct {
		User struct {
			Username string `json:"username"`
		} `json:"user"`
	}
	if err := decodeJSON("S04", me, &payload); err != nil {
		return err
	}
	if payload.User.Username != "admin" {
		return me.fail("S04", "want admin user, got %q", payload.User.Username)
	}
	return nil
}

func (s *smokeRun) step05OriginRequired() error {
	body := map[string]any{
		"name": "no-origin", "host": "192.0.2.11", "port": 22, "username": "root",
		"auth_method": "password", "password": "x",
	}
	resp, err := s.h.do("S05", http.MethodPost, "/api/servers", body, true)
	if err != nil {
		return err
	}
	return expectErrorCode("S05", resp, http.StatusForbidden, "ORIGIN_FORBIDDEN")
}

type serverBody struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Host            string   `json:"host"`
	Port            int      `json:"port"`
	Username        string   `json:"username"`
	AuthMethod      string   `json:"auth_method"`
	HasPassword     bool     `json:"has_password"`
	HasPrivateKey   bool     `json:"has_private_key"`
	Tags            []string `json:"tags"`
	AutoExecEnabled bool     `json:"auto_exec_enabled"`
}

func (s *smokeRun) step06CreateServer() error {
	body := map[string]any{
		"name":              "smoke-ssh",
		"host":              "192.0.2.10",
		"port":              22,
		"username":          "root",
		"auth_method":       "password",
		"password":          serverPassword,
		"tags":              []string{"prod", "web", "prod", " web "},
		"auto_exec_enabled": false,
	}
	resp, err := s.h.do("S06", http.MethodPost, "/api/servers", body, false)
	if err != nil {
		return err
	}
	if err := expectStatus("S06", resp, http.StatusCreated); err != nil {
		return err
	}
	if err := assertNoSecrets("S06", resp.Body, serverPassword); err != nil {
		return err
	}
	var srv serverBody
	if err := decodeJSON("S06", resp, &srv); err != nil {
		return err
	}
	if srv.ID == "" || !srv.HasPassword || srv.HasPrivateKey {
		return resp.fail("S06", "want id and has_password=true, has_private_key=false; got id=%q has_password=%v has_private_key=%v", srv.ID, srv.HasPassword, srv.HasPrivateKey)
	}
	if len(srv.Tags) != 2 || srv.Tags[0] != "prod" || srv.Tags[1] != "web" {
		return resp.fail("S06", "tags must be trimmed, order-preserving and deduplicated; got %v", srv.Tags)
	}
	s.serverID = srv.ID
	return nil
}

func (s *smokeRun) step07ServerReadUpdate() error {
	list, err := s.h.do("S07", http.MethodGet, "/api/servers", nil, false)
	if err != nil {
		return err
	}
	if err := expectStatus("S07", list, http.StatusOK); err != nil {
		return err
	}
	if err := assertNoSecrets("S07", list.Body, serverPassword); err != nil {
		return err
	}
	var page struct {
		Items []serverBody `json:"items"`
	}
	if err := decodeJSON("S07", list, &page); err != nil {
		return err
	}
	found := false
	for _, item := range page.Items {
		if item.ID == s.serverID {
			found = true
		}
	}
	if !found {
		return list.fail("S07", "created server %s not present in list", s.serverID)
	}

	detail, err := s.h.do("S07", http.MethodGet, "/api/servers/"+s.serverID, nil, false)
	if err != nil {
		return err
	}
	if err := expectStatus("S07", detail, http.StatusOK); err != nil {
		return err
	}
	if err := assertNoSecrets("S07", detail.Body, serverPassword); err != nil {
		return err
	}

	update := map[string]any{
		"name":              "smoke-ssh-renamed",
		"tags":              []string{"db", "prod"},
		"auto_exec_enabled": true,
	}
	updated, err := s.h.do("S07", http.MethodPatch, "/api/servers/"+s.serverID, update, false)
	if err != nil {
		return err
	}
	if err := expectStatus("S07", updated, http.StatusOK); err != nil {
		return err
	}
	if err := assertNoSecrets("S07", updated.Body, serverPassword); err != nil {
		return err
	}
	var srv serverBody
	if err := decodeJSON("S07", updated, &srv); err != nil {
		return err
	}
	if srv.Name != "smoke-ssh-renamed" || !srv.AutoExecEnabled ||
		len(srv.Tags) != 2 || srv.Tags[0] != "db" || srv.Tags[1] != "prod" {
		return updated.fail("S07", "update not applied: name=%q tags=%v auto_exec=%v", srv.Name, srv.Tags, srv.AutoExecEnabled)
	}
	return nil
}

func (s *smokeRun) step08SQLiteDatabase() error {
	f, err := os.CreateTemp("", "aiops-smoke-*.db")
	if err != nil {
		return fmt.Errorf("S08: create sqlite file: %w", err)
	}
	path := f.Name()
	f.Close()
	s.sqlitePath = path

	body := map[string]any{
		"name":        "smoke-sqlite",
		"engine":      "sqlite",
		"sqlite_path": path,
	}
	resp, err := s.h.do("S08", http.MethodPost, "/api/databases", body, false)
	if err != nil {
		return err
	}
	if err := expectStatus("S08", resp, http.StatusCreated); err != nil {
		return err
	}
	if err := assertNoSecrets("S08", resp.Body); err != nil {
		return err
	}
	var db struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Engine      string `json:"engine"`
		SQLitePath  string `json:"sqlite_path"`
		HasPassword bool   `json:"has_password"`
	}
	if err := decodeJSON("S08", resp, &db); err != nil {
		return err
	}
	if db.ID == "" || db.Engine != "sqlite" || db.SQLitePath != path || db.HasPassword {
		return resp.fail("S08", "unexpected database payload id=%q engine=%q path=%q has_password=%v", db.ID, db.Engine, db.SQLitePath, db.HasPassword)
	}
	s.databaseID = db.ID

	tested, err := s.h.do("S08", http.MethodPost, "/api/databases/"+s.databaseID+"/test", nil, false)
	if err != nil {
		return err
	}
	if err := expectStatus("S08", tested, http.StatusOK); err != nil {
		return err
	}
	if err := assertNoSecrets("S08", tested.Body); err != nil {
		return err
	}
	var test struct {
		OK bool `json:"ok"`
	}
	if err := decodeJSON("S08", tested, &test); err != nil {
		return err
	}
	if !test.OK {
		return tested.fail("S08", "database test did not return ok=true")
	}
	return nil
}

type providerBody struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Preset       string   `json:"preset"`
	Kind         string   `json:"kind"`
	BaseURL      string   `json:"base_url"`
	HasAPIKey    bool     `json:"has_api_key"`
	APIKeyMasked string   `json:"api_key_masked"`
	DefaultModel string   `json:"default_model"`
	Models       []string `json:"models"`
}

func (s *smokeRun) createProvider() (*providerBody, *apiResponse, error) {
	body := map[string]any{
		"name":          "smoke-provider",
		"preset":        "custom",
		"kind":          "openai_compatible",
		"base_url":      s.mock.URL(),
		"api_key":       providerAPIKey,
		"default_model": mockModel,
		"models":        []string{mockModel},
	}
	resp, err := s.h.do("S09", http.MethodPost, "/api/providers", body, false)
	if err != nil {
		return nil, nil, err
	}
	if err := expectStatus("S09", resp, http.StatusCreated); err != nil {
		return nil, resp, err
	}
	var provider providerBody
	if err := decodeJSON("S09", resp, &provider); err != nil {
		return nil, resp, err
	}
	return &provider, resp, nil
}

func (s *smokeRun) step09CreateProvider() error {
	provider, resp, err := s.createProvider()
	if err != nil {
		return err
	}
	if err := assertNoSecrets("S09", resp.Body, providerAPIKey); err != nil {
		return err
	}
	if provider.ID == "" || !provider.HasAPIKey {
		return resp.fail("S09", "want id and has_api_key=true; got id=%q has_api_key=%v", provider.ID, provider.HasAPIKey)
	}
	if provider.APIKeyMasked != "****1234" {
		return resp.fail("S09", "api_key_masked must be ****1234, got %q", provider.APIKeyMasked)
	}
	s.providerID = provider.ID
	return nil
}

func (s *smokeRun) step10ProviderTest() error {
	tested, err := s.h.do("S10", http.MethodPost, "/api/providers/"+s.providerID+"/test", nil, false)
	if err != nil {
		return err
	}
	if err := expectStatus("S10", tested, http.StatusOK); err != nil {
		return err
	}
	var test struct {
		OK         bool `json:"ok"`
		ModelCount int  `json:"model_count"`
	}
	if err := decodeJSON("S10", tested, &test); err != nil {
		return err
	}
	if !test.OK || test.ModelCount < 1 {
		return tested.fail("S10", "want ok=true model_count>=1, got ok=%v model_count=%d", test.OK, test.ModelCount)
	}

	refreshed, err := s.h.do("S10", http.MethodPost, "/api/providers/"+s.providerID+"/models/refresh", nil, false)
	if err != nil {
		return err
	}
	if err := expectStatus("S10", refreshed, http.StatusOK); err != nil {
		return err
	}
	var models struct {
		Models []string `json:"models"`
	}
	if err := decodeJSON("S10", refreshed, &models); err != nil {
		return err
	}
	if !containsString(models.Models, mockModel) {
		return refreshed.fail("S10", "refreshed models do not contain %q: %v", mockModel, models.Models)
	}
	return nil
}

func (s *smokeRun) step11RiskPreview() error {
	cases := []struct {
		command string
		want    string
	}{
		{"rm -rf /", "forbidden"},
		{"systemctl stop sshd", "forbidden"},
		{"ls -la", "safe"},
		{"rm -rf /tmp/x", "needs_approval"},
	}
	for _, tc := range cases {
		resp, err := s.h.do("S11", http.MethodPost, "/api/ops/preview-risk", map[string]any{"command": tc.command}, false)
		if err != nil {
			return err
		}
		if err := expectStatus("S11", resp, http.StatusOK); err != nil {
			return err
		}
		var result struct {
			RiskLevel string `json:"risk_level"`
			RuleID    string `json:"rule_id"`
		}
		if err := decodeJSON("S11", resp, &result); err != nil {
			return err
		}
		if result.RiskLevel != tc.want {
			return resp.fail("S11", "command %q: want risk_level %q, got %q", tc.command, tc.want, result.RiskLevel)
		}
		if result.RuleID == "" {
			return resp.fail("S11", "command %q: rule_id is empty", tc.command)
		}
	}
	return nil
}

func (s *smokeRun) step12ChatNoProvider() error {
	deleted, err := s.h.do("S12", http.MethodDelete, "/api/providers/"+s.providerID, nil, false)
	if err != nil {
		return err
	}
	if err := expectStatus("S12", deleted, http.StatusNoContent); err != nil {
		return err
	}
	s.providerID = ""

	conv, err := s.createConversation("S12", "conv-no-provider")
	if err != nil {
		return err
	}
	s.noProvider = conv

	resp, err := s.h.do("S12", http.MethodPost, "/api/chat", map[string]any{
		"conversation_id": s.noProvider,
		"content":         "hello without provider",
	}, false)
	if err != nil {
		return err
	}
	if err := expectErrorCode("S12", resp, http.StatusUnprocessableEntity, "NO_PROVIDER_CONFIGURED"); err != nil {
		return err
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		return resp.fail("S12", "want JSON error content type, got %q", ct)
	}
	return nil
}

func (s *smokeRun) step13ChatSSE() error {
	provider, resp, err := s.createProvider()
	if err != nil {
		return err
	}
	if err := assertNoSecrets("S13", resp.Body, providerAPIKey); err != nil {
		return err
	}
	s.providerID2 = provider.ID

	alpha, err := s.createConversation("S13", "conv-alpha")
	if err != nil {
		return err
	}
	bravo, err := s.createConversation("S13", "conv-bravo")
	if err != nil {
		return err
	}
	s.alphaID = alpha
	s.bravoID = bravo

	if err := s.chat("S13", alpha, "alpha"); err != nil {
		return err
	}
	return s.chat("S13", bravo, "bravo")
}

// chat 读取完整 SSE 到 done，并断言 token 内容与事件顺序。
func (s *smokeRun) chat(step, conversationID, content string) error {
	resp, err := s.h.open(step, http.MethodPost, "/api/chat", map[string]any{
		"conversation_id": conversationID,
		"content":         content,
	}, false)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, bodySnippet))
		return fmt.Errorf("%s: chat status=%d body=%s", step, resp.StatusCode, string(body))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		return fmt.Errorf("%s: want text/event-stream, got %q (mock streams=%d)", step, ct, s.mock.Streams())
	}
	events, err := readSSE(step, resp.Body)
	if err != nil {
		return err
	}
	return validateSSE(step, events, "echo:"+content)
}

func (s *smokeRun) step14ConversationIsolation() error {
	alpha, err := s.messages("S14", s.alphaID)
	if err != nil {
		return err
	}
	if err := checkIsolation("S14", alpha, "alpha", "bravo"); err != nil {
		return err
	}
	bravo, err := s.messages("S14", s.bravoID)
	if err != nil {
		return err
	}
	return checkIsolation("S14", bravo, "bravo", "alpha")
}

type messageBody struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func (s *smokeRun) messages(step, conversationID string) ([]messageBody, error) {
	resp, err := s.h.do(step, http.MethodGet, "/api/conversations/"+conversationID+"/messages", nil, false)
	if err != nil {
		return nil, err
	}
	if err := expectStatus(step, resp, http.StatusOK); err != nil {
		return nil, err
	}
	var page struct {
		Items []messageBody `json:"items"`
	}
	if err := decodeJSON(step, resp, &page); err != nil {
		return nil, err
	}
	return page.Items, nil
}

func checkIsolation(step string, items []messageBody, want, other string) error {
	if len(items) != 2 {
		return fmt.Errorf("%s: want 2 messages (user+assistant), got %d", step, len(items))
	}
	if items[0].Role != "user" || items[0].Content != want {
		return fmt.Errorf("%s: want user message %q, got role=%q content=%q", step, want, items[0].Role, items[0].Content)
	}
	if items[1].Role != "assistant" || items[1].Content != "echo:"+want {
		return fmt.Errorf("%s: want assistant message %q, got role=%q content=%q", step, "echo:"+want, items[1].Role, items[1].Content)
	}
	for _, item := range items {
		if strings.Contains(item.Content, other) {
			return fmt.Errorf("%s: cross-conversation leak: found %q in %q", step, other, item.Content)
		}
	}
	return nil
}

func (s *smokeRun) step15ApprovalNotFound() error {
	resp, err := s.h.do("S15", http.MethodPost, "/api/approvals/"+uuid.NewString()+"/decide", map[string]any{
		"decision": "deny",
	}, false)
	if err != nil {
		return err
	}
	return expectErrorCode("S15", resp, http.StatusNotFound, "APPROVAL_NOT_FOUND")
}

type auditBody struct {
	Action   string `json:"action"`
	PrevHash string `json:"prev_hash"`
	Hash     string `json:"hash"`
}

func (s *smokeRun) step16AuditChain() error {
	deadline := time.Now().Add(2 * time.Second)
	var lastMissing []string
	for {
		resp, err := s.h.do("S16", http.MethodGet, "/api/audit?limit=200", nil, false)
		if err != nil {
			return err
		}
		if err := expectStatus("S16", resp, http.StatusOK); err != nil {
			return err
		}
		var page struct {
			Items []auditBody `json:"items"`
		}
		if err := decodeJSON("S16", resp, &page); err != nil {
			return err
		}
		if err := assertNoSecrets("S16", resp.Body, serverPassword, providerAPIKey); err != nil {
			return err
		}
		for _, item := range page.Items {
			if len(item.Hash) != 64 || len(item.PrevHash) != 64 {
				return resp.fail("S16", "audit %q hash lengths must be 64, got prev=%d hash=%d", item.Action, len(item.PrevHash), len(item.Hash))
			}
		}

		if missing := missingAuditActions(page.Items); len(missing) == 0 {
			return nil
		} else {
			lastMissing = missing
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("S16: audit actions missing after 2s: %v", lastMissing)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func missingAuditActions(items []auditBody) []string {
	seen := map[string]bool{}
	for _, item := range items {
		seen[item.Action] = true
	}
	var missing []string
	loginFound := false
	chatFound := false
	for _, item := range items {
		if strings.Contains(item.Action, "login") {
			loginFound = true
		}
		if strings.HasPrefix(item.Action, "chat.") {
			chatFound = true
		}
	}
	if !loginFound {
		missing = append(missing, "login")
	}
	if !seen["server.create"] {
		missing = append(missing, "server.create")
	}
	if !seen["provider.create"] {
		missing = append(missing, "provider.create")
	}
	if !chatFound {
		missing = append(missing, "chat")
	}
	return missing
}

func (s *smokeRun) step17DeleteAssets() error {
	for _, conv := range []string{s.alphaID, s.bravoID} {
		resp, err := s.h.do("S17", http.MethodDelete, "/api/conversations/"+conv, nil, false)
		if err != nil {
			return err
		}
		if err := expectStatus("S17", resp, http.StatusNoContent); err != nil {
			return err
		}
		for _, path := range []string{
			"/api/conversations/" + conv,
			"/api/conversations/" + conv + "/messages",
			"/api/conversations/" + conv + "/tool-calls",
		} {
			gone, err := s.h.do("S17", http.MethodGet, path, nil, false)
			if err != nil {
				return err
			}
			if err := expectStatus("S17", gone, http.StatusNotFound); err != nil {
				return err
			}
		}
	}

	deletions := []struct {
		step string
		path string
		get  string
	}{
		{"S17", "/api/providers/" + s.providerID2, "/api/providers/" + s.providerID2},
		{"S17", "/api/databases/" + s.databaseID, "/api/databases/" + s.databaseID},
		{"S17", "/api/servers/" + s.serverID, "/api/servers/" + s.serverID},
	}
	for _, item := range deletions {
		resp, err := s.h.do(item.step, http.MethodDelete, item.path, nil, false)
		if err != nil {
			return err
		}
		if err := expectStatus(item.step, resp, http.StatusNoContent); err != nil {
			return err
		}
		gone, err := s.h.do(item.step, http.MethodGet, item.get, nil, false)
		if err != nil {
			return err
		}
		if err := expectStatus(item.step, gone, http.StatusNotFound); err != nil {
			return err
		}
	}
	return nil
}

func (s *smokeRun) step18Logout() error {
	resp, err := s.h.do("S18", http.MethodPost, "/api/auth/logout", nil, false)
	if err != nil {
		return err
	}
	if err := expectStatus("S18", resp, http.StatusNoContent); err != nil {
		return err
	}
	me, err := s.h.do("S18", http.MethodGet, "/api/auth/me", nil, false)
	if err != nil {
		return err
	}
	return expectErrorCode("S18", me, http.StatusUnauthorized, "AUTH_REQUIRED")
}

func (s *smokeRun) createConversation(step, title string) (string, error) {
	resp, err := s.h.do(step, http.MethodPost, "/api/conversations", map[string]any{"title": title}, false)
	if err != nil {
		return "", err
	}
	if err := expectStatus(step, resp, http.StatusCreated); err != nil {
		return "", err
	}
	var conv struct {
		ID string `json:"id"`
	}
	if err := decodeJSON(step, resp, &conv); err != nil {
		return "", err
	}
	if conv.ID == "" {
		return "", resp.fail(step, "conversation id is empty")
	}
	return conv.ID, nil
}

func containsString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}
