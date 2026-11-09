package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeBackend 用内存 map 模拟资产列表与创建端点，验证 seed 的幂等逻辑。
func fakeBackend(t *testing.T) *httptest.Server {
	t.Helper()
	servers := map[string]bool{}
	providers := map[string]bool{}
	conversations := map[string]string{} // title -> id
	var seq int
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/auth/login", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Password string `json:"password"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Password != "seed-pass" {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "AUTH_INVALID"}})
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "aiops_session", Value: "token"})
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /api/servers", func(w http.ResponseWriter, r *http.Request) {
		writeItems(t, w, servers)
	})
	mux.HandleFunc("POST /api/servers", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		seq++
		servers[body.Name] = true
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": itoa(seq), "name": body.Name})
	})
	mux.HandleFunc("GET /api/providers", func(w http.ResponseWriter, r *http.Request) {
		writeItems(t, w, providers)
	})
	mux.HandleFunc("POST /api/providers", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		seq++
		providers[body.Name] = true
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": itoa(seq), "name": body.Name})
	})
	mux.HandleFunc("GET /api/conversations", func(w http.ResponseWriter, r *http.Request) {
		items := make([]map[string]string, 0, len(conversations))
		for title, id := range conversations {
			items = append(items, map[string]string{"id": id, "title": title})
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
	})
	mux.HandleFunc("POST /api/conversations", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Title string `json:"title"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		seq++
		conversations[body.Title] = itoa(seq)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": itoa(seq)})
	})
	return httptest.NewServer(mux)
}

func writeItems(t *testing.T, w http.ResponseWriter, names map[string]bool) {
	t.Helper()
	items := make([]map[string]string, 0, len(names))
	for name := range names {
		items = append(items, map[string]string{"id": "id-" + name, "name": name})
	}
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
}

func itoa(n int) string {
	return "00000000-0000-0000-0000-" + strings.Repeat("0", 12-len(intToString(n))) + intToString(n)
}

func intToString(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}

func TestRunSeedIdempotent(t *testing.T) {
	backend := fakeBackend(t)
	defer backend.Close()

	client, err := newSeedClient(backend.URL, "seed-pass")
	if err != nil {
		t.Fatalf("newSeedClient: %v", err)
	}
	opts := seedOptions{llmBaseURL: "http://127.0.0.1:18081"}
	for round := 1; round <= 2; round++ {
		if err := runSeed(client, opts); err != nil {
			t.Fatalf("round %d runSeed: %v", round, err)
		}
	}

	// 第二轮应全部命中「已存在」：资产数量保持 2 服务器 + 1 供应商 + 1 会话。
	client2, err := newSeedClient(backend.URL, "seed-pass")
	if err != nil {
		t.Fatalf("newSeedClient(2): %v", err)
	}
	servers, err := listNamed(client2, "/api/servers")
	if err != nil {
		t.Fatalf("list servers: %v", err)
	}
	if len(servers) != 2 {
		t.Fatalf("want 2 seeded servers, got %d: %v", len(servers), servers)
	}
	providers, err := listNamed(client2, "/api/providers")
	if err != nil {
		t.Fatalf("list providers: %v", err)
	}
	if len(providers) != 1 {
		t.Fatalf("want 1 seeded provider, got %d: %v", len(providers), providers)
	}
}

func TestNewSeedClientWrongPassword(t *testing.T) {
	backend := fakeBackend(t)
	defer backend.Close()
	if _, err := newSeedClient(backend.URL, "wrong"); err == nil {
		t.Fatal("want error for wrong password")
	}
}

func TestSeedChatRejectsErrorEvent(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/auth/login" {
			http.SetCookie(w, &http.Cookie{Name: "aiops_session", Value: "token"})
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: error\ndata: {\"code\":\"LLM_UPSTREAM_ERROR\"}\n\n"))
	}))
	defer backend.Close()
	client, err := newSeedClient(backend.URL, "seed-pass")
	if err != nil {
		t.Fatalf("newSeedClient: %v", err)
	}
	if err := seedChat(client, "conv-1"); err == nil {
		t.Fatal("want error when SSE emits error event")
	}
}
