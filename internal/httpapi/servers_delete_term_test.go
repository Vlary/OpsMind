package httpapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"aiops/internal/sshx"
	"aiops/internal/store"
)

// hangingPTY 是安静但活着的 PTY：Read 阻塞到 Close，Write/Resize 恒成功。
type hangingPTY struct {
	once   sync.Once
	closed chan struct{}
}

func newHangingPTY() *hangingPTY { return &hangingPTY{closed: make(chan struct{})} }

func (p *hangingPTY) Write(b []byte) (int, error) { return len(b), nil }

func (p *hangingPTY) Read(b []byte) (int, error) {
	<-p.closed
	return 0, io.EOF
}

func (p *hangingPTY) Resize(cols, rows int) error { return nil }

func (p *hangingPTY) Close() error {
	p.once.Do(func() { close(p.closed) })
	return nil
}

func (p *hangingPTY) Wait() error {
	<-p.closed
	return nil
}

// okPTYPool 提供 Session 能力（返回 hangingPTY），使 Attach 形成持续 live 会话。
type okPTYPool struct {
	sshx.Executor
}

func (f *okPTYPool) Session(context.Context, sshx.Target, sshx.SessionRequest) (sshx.PTYSession, error) {
	return newHangingPTY(), nil
}

// TestDeleteServerEndsLiveTerminalSession 契约 2.6：删除服务器前必须先结束
// 其活动终端会话（ended_reason=error）。修复前 handleDeleteServer 直接
// 删行：live PTY 继续以原凭据存活至空闲超时，且收尾 Close 因行已不存在
// 落空、session.end 审计丢失（评审发现）。
func TestDeleteServerEndsLiveTerminalSession(t *testing.T) {
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
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if resp.Header.Get("Sec-WebSocket-Protocol") != termWSSubprotocol {
		t.Fatalf("negotiated subprotocol = %q", resp.Header.Get("Sec-WebSocket-Protocol"))
	}
	// Attach 异步拨号（内存 fake，毫秒级）；留出裕量确保 live 已注册。
	time.Sleep(300 * time.Millisecond)

	rec := env.do(http.MethodDelete, "/api/servers/"+serverID, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete server status = %d body = %s", rec.Code, rec.Body.String())
	}

	// CloseByServer 必须结束 live 会话：WS 收到 closed(error) 帧。
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	sawClosed := false
	for !sawClosed {
		var m map[string]any
		if err := conn.ReadJSON(&m); err != nil {
			t.Fatalf("read frame: %v (closed seen = %v)", err, sawClosed)
		}
		if m["type"] == "closed" {
			sawClosed = true
			if m["reason"] != "error" {
				t.Fatalf("closed reason = %v, want error", m["reason"])
			}
		}
	}

	// 会话行被显式 Close（status=closed, ended_reason=error），而不是留在
	// active 一直挂在已删除的服务器上。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		row, err := env.st.TerminalSessions().Get(context.Background(), session.ID)
		if err == nil && row.Status == store.SessionClosed && row.EndedReason == "error" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("terminal session row was not closed with ended_reason=error after server deletion")
}
