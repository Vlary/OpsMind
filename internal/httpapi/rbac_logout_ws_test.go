// rbac_logout_ws_test.go 覆盖第 1 轮走查 high：登出后已建立的终端
// WebSocket 必须断开（此前登出 35 秒仍可向远端 shell 输入命令）。
package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"aiops/internal/store"
)

// TestLogoutClosesTerminalWebSocket：登出 → 该 auth session 名下的终端 WS
// 收到 closed 帧并断开；同一观测点验证会话行已 closed（不是只删 auth row）。
func TestLogoutClosesTerminalWebSocket(t *testing.T) {
	env := newTermTestEnvWithSSH(t, &okPTYPool{})
	serverID := env.createServer(t, "linux", store.AuthMethodPassword)
	session := env.createSession(t, serverID)

	httpSrv := httptest.NewServer(env.router)
	t.Cleanup(httpSrv.Close)
	wsURL := "ws" + strings.TrimPrefix(httpSrv.URL, "http") + "/api/term/sessions/" + session.ID + "/ws"
	header := http.Header{}
	header.Set("Origin", testOrigin)
	header.Set("Cookie", sessionCookieName+"="+env.cookie)
	header.Set("Sec-WebSocket-Protocol", termWSSubprotocol)
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		body := ""
		if resp != nil {
			b := make([]byte, 0, 512)
			body = string(b)
		}
		t.Fatalf("dial: %v resp=%v body=%s", err, resp, body)
	}
	defer conn.Close()
	// Attach 异步拨号（内存 fake，毫秒级）；留裕量确保 live 已注册。
	time.Sleep(300 * time.Millisecond)

	// 登出：204。
	rec := env.do(http.MethodPost, "/api/auth/logout", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout = %d body = %s", rec.Code, rec.Body.String())
	}

	// WS 必须被关闭：读到 closed 帧（或读错误）即视为断开；持续可读 3s 视为
	// 未断开（回归）。
	deadline := time.Now().Add(3 * time.Second)
	sawClosed := false
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		var m map[string]any
		if err := conn.ReadJSON(&m); err != nil {
			// 读错误等价于连接已断开（服务器 Close 后客户端读到 1006）。
			sawClosed = true
			break
		}
		if m["type"] == "closed" {
			sawClosed = true
			break
		}
	}
	if !sawClosed {
		t.Fatal("terminal WebSocket still open after logout; post-logout input would reach the remote shell")
	}

	// 会话行也应收尾（不是只在进程内关连接）。
	rowDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(rowDeadline) {
		row, err := env.st.TerminalSessions().Get(t.Context(), session.ID)
		if err == nil && row.Status == store.SessionClosed {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("terminal session row was not closed after logout")
}

// TestLogoutKeepsOtherSessionsOpen：登出只关闭本次登录建立的连接；该用户
// 另一台设备上的 auth session 不受影响（不能把「登出」实现成「踢掉全部」）。
func TestLogoutKeepsOtherSessionsOpen(t *testing.T) {
	env := newTermTestEnvWithSSH(t, &okPTYPool{})
	serverID := env.createServer(t, "linux", store.AuthMethodPassword)
	session := env.createSession(t, serverID)

	httpSrv := httptest.NewServer(env.router)
	t.Cleanup(httpSrv.Close)
	wsURL := "ws" + strings.TrimPrefix(httpSrv.URL, "http") + "/api/term/sessions/" + session.ID + "/ws"
	header := http.Header{}
	header.Set("Origin", testOrigin)
	header.Set("Cookie", sessionCookieName+"="+env.cookie)
	header.Set("Sec-WebSocket-Protocol", termWSSubprotocol)
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	time.Sleep(300 * time.Millisecond)

	// 另开一个登录会话（不参与登出），确认它仍可访问 /api/auth/me。
	second := env.login()
	if rec := doAsCookie(env.router, second, http.MethodGet, "/api/auth/me", ""); rec.Code != http.StatusOK {
		t.Fatalf("second session before logout = %d", rec.Code)
	}
	if rec := env.do(http.MethodPost, "/api/auth/logout", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("logout = %d", rec.Code)
	}
	if rec := doAsCookie(env.router, second, http.MethodGet, "/api/auth/me", ""); rec.Code != http.StatusOK {
		t.Fatalf("second session after logout = %d, want 200 (logout must not revoke other devices)", rec.Code)
	}
}

// TestLogoutClosesRdpWebSocket：登出同样必须断开 RDP WebSocket（契约 12.3
// 第 13 条：logout/TTL/revoke 都要 CloseByAuthSession）。
func TestLogoutClosesRdpWebSocket(t *testing.T) {
	addr, connect := startGuacdStub(t)
	env := newRdpTestEnv(t, addr)
	env.connect = connect
	winID := env.createServer(t, "windows", store.AuthMethodPassword, true)
	_, sessionID := env.createSession(t, winID, `{"server_id":"`+winID+`"}`)

	server := httptest.NewServer(env.router)
	t.Cleanup(server.Close)
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/rdp/sessions/" + sessionID + "/ws"
	header := http.Header{}
	header.Set("Origin", testOrigin)
	header.Set("Cookie", sessionCookieName+"="+env.cookie)
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer ws.Close()
	// 消费握手后的 sync 帧，确认 Attach 已完成。
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := ws.ReadMessage(); err != nil {
		t.Fatalf("read sync: %v", err)
	}

	if rec := env.do(http.MethodPost, "/api/auth/logout", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("logout = %d body = %s", rec.Code, rec.Body.String())
	}
	// 登出后连接必须被关闭：下一次读要么读到关闭帧（1000/1001）要么读错误。
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		if _, _, err := ws.ReadMessage(); err != nil {
			return // 连接已断开
		}
	}
}
