//go:build integration_smtp

// notify_email_test.go 覆盖 G32 的 email 分支（契约 9.2：容器/fake SMTP +
// //go:build integration_smtp 标注，CI 不强制）。fake SMTP 实现 HELO→
// （可选 AUTH）→MAIL→RCPT→DATA 序列断言，use_tls=false 走明文通道。
package monitor

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"aiops/internal/store"
)

// fakeSMTPD 是最小 SMTP 服务器：记录会话命令序列。
type fakeSMTPD struct {
	ln       net.Listener
	commands []string
	data     string
}

func newFakeSMTPD(t *testing.T) *fakeSMTPD {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d := &fakeSMTPD{ln: ln}
	go d.serve(t)
	t.Cleanup(func() { _ = ln.Close() })
	return d
}

func (d *fakeSMTPD) serve(t *testing.T) {
	conn, err := d.ln.Accept()
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	write := func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }
	write("220 fake smtp")
	r := bufio.NewReader(conn)
	inData := false
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if inData {
			d.data += line + "\n"
			if line == "." {
				inData = false
				write("250 ok")
			}
			continue
		}
		d.commands = append(d.commands, line)
		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
			write("250-fake")
			write("250 AUTH PLAIN")
		case strings.HasPrefix(upper, "AUTH"):
			write("235 ok")
		case strings.HasPrefix(upper, "MAIL FROM:"):
			write("250 ok")
		case strings.HasPrefix(upper, "RCPT TO:"):
			write("250 ok")
		case strings.HasPrefix(upper, "DATA"):
			write("354 go")
			inData = true
		case upper == "QUIT":
			write("221 bye")
			return
		default:
			write("250 ok")
		}
	}
}

func TestNotifyEmailDelivery(t *testing.T) {
	d := newFakeSMTPD(t)
	addr := d.ln.Addr().String()
	host, portStr, _ := net.SplitHostPort(addr)
	var port int
	_ = json.Unmarshal([]byte(portStr), &port)

	st := newFakeMonitorStore()
	cipher := newTestCipher(t)
	cfgJSON := `{"smtp_host":"` + host + `","smtp_port":` + portStr + `,"username":"u","password":"p","from":"ops@example.com","to":["a@example.com","b@example.com"],"use_tls":false}`
	ch, err := st.NotificationChannels().Create(context.Background(), store.CreateNotificationChannelParams{
		Name: "mail", Kind: store.NotificationEmail, ConfigJSON: encryptedConfig(t, cipher, cfgJSON),
	})
	if err != nil {
		t.Fatal(err)
	}
	ev, err := st.AlertEvents().Create(context.Background(),
		store.AlertRule{ID: "r1", Severity: "warning"}, "s1", 1, "m", time.Now())
	if err != nil {
		t.Fatal(err)
	}

	n := newTestNotifier(t, st, cipher)
	task := testNotifyTask()
	task.Event.ID = ev.ID
	task.ChannelID = ch.ID
	n.deliver(context.Background(), task)

	joined := strings.Join(d.commands, "\n")
	for _, want := range []string{"MAIL FROM:<ops@example.com>", "RCPT TO:<a@example.com>", "RCPT TO:<b@example.com>"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("session missing %s:\n%s", want, joined)
		}
	}
	if !strings.Contains(d.data, "Subject: [OpsMind][warning] cpu-high web-1") {
		t.Fatalf("data missing subject:\n%s", d.data)
	}
	st.mu.Lock()
	status := st.alerts[0].NotifyStatus
	st.mu.Unlock()
	if status != store.NotifySent {
		t.Fatalf("notify_status = %s, want sent", status)
	}
}
