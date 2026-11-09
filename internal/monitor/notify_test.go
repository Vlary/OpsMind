// notify_test.go 覆盖 G32（webhook/飞书/钉钉默认执行；email 投递在
// notify_email_test.go 以 //go:build integration_smtp 标注）：
//   - webhook payload 字段与 X-OpsMind-Signature（HMAC-SHA256 hex）
//   - 飞书 text 卡与签名（timestamp/sign）
//   - 钉钉 markdown 与加签 URL（&timestamp=..&sign=..）
//   - 重试 3 次（1s/5s/15s 退避）耗尽 → notify_status=failed
//   - 队列满丢弃不阻塞 → 直接 failed
//   - 渠道配置 schema 校验（畸形/超限拒绝）
package monitor

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aiops/internal/cryptox"
	"aiops/internal/store"
)

// hmacSHA256Hex 与生产签名算法一致（测试独立重算交叉验证）。
func hmacSHA256Hex(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func testNotifyTask() notifyTask {
	return notifyTask{
		Event: store.AlertEvent{
			ID: 7, RuleID: "r1", ServerID: "s1", Metric: store.AlertMetricCPU,
			Status: store.AlertEventFiring, Severity: store.AlertSeverityWarning,
			Value: 95.5, Message: "cpu_pct 95.5 > 90.0 持续 3 个采样点",
			FiredAt: time.Date(2026, 10, 27, 8, 0, 0, 0, time.UTC),
		},
		RuleName:   "cpu-high",
		ServerName: "web-1",
	}
}

func encryptedConfig(t *testing.T, cipher *cryptox.Cipher, raw string) string {
	t.Helper()
	enc, err := cipher.Encrypt(raw)
	if err != nil {
		t.Fatalf("encrypt config: %v", err)
	}
	return enc
}

func newTestNotifier(t *testing.T, st *fakeMonitorStore, cipher *cryptox.Cipher) *notifier {
	t.Helper()
	now := time.Now()
	return newNotifier(NotifyConfig{}, st, cipher, quietLogger(), func() time.Time { return now }, &fakeAuditSink{})
}

func TestNotifyWebhookPayloadAndSignature(t *testing.T) {
	var gotBody []byte
	var gotSig atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		gotSig.Store(r.Header.Get("X-OpsMind-Signature"))
		w.WriteHeader(200)
	}))
	defer srv.Close()

	st := newFakeMonitorStore()
	cipher := newTestCipher(t)
	cfg := encryptedConfig(t, cipher, `{"url":"`+srv.URL+`","secret":"s3cret"}`)
	ch, err := st.NotificationChannels().Create(context.Background(), store.CreateNotificationChannelParams{
		Name: "hook", Kind: store.NotificationWebhook, ConfigJSON: cfg, ConfigHint: "hint",
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

	var payload map[string]any
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatalf("payload not json: %v", err)
	}
	for key, want := range map[string]any{
		"event": "alert", "rule": "cpu-high", "server": "web-1",
		"metric": "cpu_pct", "severity": "warning",
	} {
		if payload[key] != want {
			t.Fatalf("payload[%s] = %v, want %v", key, payload[key], want)
		}
	}
	if v, _ := payload["value"].(float64); v != 95.5 {
		t.Fatalf("value = %v", payload["value"])
	}
	if payload["fired_at"] != "2026-10-27T08:00:00Z" {
		t.Fatalf("fired_at = %v", payload["fired_at"])
	}
	// HMAC-SHA256(secret, body) hex。
	sig := hmacSHA256Hex("s3cret", gotBody)
	if gotSig.Load() != sig {
		t.Fatalf("signature = %v, want %v", gotSig.Load(), sig)
	}
	// 事件状态回写 sent。
	st.mu.Lock()
	status := st.alerts[0].NotifyStatus
	st.mu.Unlock()
	if status != store.NotifySent {
		t.Fatalf("notify_status = %s, want sent", status)
	}
}

func TestNotifyFeishuTextAndSign(t *testing.T) {
	var gotPayload map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotPayload)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	st := newFakeMonitorStore()
	cipher := newTestCipher(t)
	cfg := encryptedConfig(t, cipher, `{"url":"`+srv.URL+`","secret":"fs"}`)
	ch, _ := st.NotificationChannels().Create(context.Background(), store.CreateNotificationChannelParams{
		Name: "feishu", Kind: store.NotificationFeishu, ConfigJSON: cfg,
	})

	n := newTestNotifier(t, st, cipher)
	task := testNotifyTask()
	task.ChannelID = ch.ID
	n.deliver(context.Background(), task)
	if gotPayload["msg_type"] != "text" {
		t.Fatalf("msg_type = %v", gotPayload["msg_type"])
	}
	content, _ := gotPayload["content"].(map[string]any)
	text, _ := content["text"].(string)
	wantPrefix := "[OpsMind][warning] cpu-high web-1"
	if !strings.HasPrefix(text, wantPrefix) {
		t.Fatalf("text = %q, want prefix %q", text, wantPrefix)
	}
	if _, signed := gotPayload["sign"]; !signed {
		t.Fatal("feishu sign field missing（secret 非空必须签名）")
	}
	if _, stamped := gotPayload["timestamp"]; !stamped {
		t.Fatal("feishu timestamp field missing")
	}
}

func TestNotifyDingtalkSignedURL(t *testing.T) {
	var gotQuery atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery.Store(r.URL.RawQuery)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	st := newFakeMonitorStore()
	cipher := newTestCipher(t)
	cfg := encryptedConfig(t, cipher, `{"url":"`+srv.URL+`/robot/send?access_token=tok","secret":"dt"}`)
	ch, _ := st.NotificationChannels().Create(context.Background(), store.CreateNotificationChannelParams{
		Name: "ding", Kind: store.NotificationDingtalk, ConfigJSON: cfg,
	})

	n := newTestNotifier(t, st, cipher)
	task := testNotifyTask()
	task.ChannelID = ch.ID
	n.deliver(context.Background(), task)
	q, _ := gotQuery.Load().(string)
	if !strings.Contains(q, "access_token=tok") {
		t.Fatalf("original query lost: %q", q)
	}
	if !strings.Contains(q, "&timestamp=") || !strings.Contains(q, "&sign=") {
		t.Fatalf("dingtalk signature params missing: %q", q)
	}
}

func TestNotifyRetryExhaustionMarksFailed(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(500) // 永久失败
	}))
	defer srv.Close()

	st := newFakeMonitorStore()
	cipher := newTestCipher(t)
	cfg := encryptedConfig(t, cipher, `{"url":"`+srv.URL+`"}`)
	ch, _ := st.NotificationChannels().Create(context.Background(), store.CreateNotificationChannelParams{
		Name: "bad", Kind: store.NotificationWebhook, ConfigJSON: cfg,
	})
	ev, err := st.AlertEvents().Create(context.Background(), store.AlertRule{ID: "r1", Severity: "warning"}, "s1", 1, "m", time.Now())
	if err != nil {
		t.Fatal(err)
	}

	n := newTestNotifier(t, st, cipher)
	// 缩短退避：直接替换退避表（重试次数语义不变）。
	notifyBackoffs = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	defer func() { notifyBackoffs = []time.Duration{time.Second, 5 * time.Second, 15 * time.Second} }()

	task := testNotifyTask()
	task.Event.ID = ev.ID
	task.ChannelID = ch.ID
	n.deliver(context.Background(), task)

	if got := atomic.LoadInt32(&calls); got != 4 { // 首发 + 3 次重试
		t.Fatalf("attempts = %d, want 4", got)
	}
	st.mu.Lock()
	status := st.alerts[0].NotifyStatus
	st.mu.Unlock()
	if status != store.NotifyFailed {
		t.Fatalf("notify_status = %s, want failed", status)
	}
}

func TestNotifyQueueFullDropsWithoutBlocking(t *testing.T) {
	st := newFakeMonitorStore()
	cipher := newTestCipher(t)
	n := newTestNotifier(t, st, cipher)

	// 库内先有对应事件（enqueue 丢弃时回写 failed）。
	ev, err := st.AlertEvents().Create(context.Background(),
		store.AlertRule{ID: "r1", Severity: "warning"}, "s1", 1, "m", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	task := testNotifyTask()
	task.Event.ID = ev.ID

	n.cfg.QueueSize = 1
	n.queue = make(chan notifyTask, 1) // 占满
	n.queue <- notifyTask{Event: store.AlertEvent{ID: 99999}}

	done := make(chan struct{})
	go func() {
		n.enqueue(task) // 队列满：丢弃置 failed，不阻塞
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("enqueue blocked on full queue")
	}
	st.mu.Lock()
	status := st.alerts[0].NotifyStatus
	st.mu.Unlock()
	if status != store.NotifyFailed {
		t.Fatalf("notify_status = %s, want failed（队列满丢弃）", status)
	}
}

func TestParseNotificationConfigValidation(t *testing.T) {
	cases := []struct {
		kind store.NotificationKind
		raw  string
		ok   bool
	}{
		{store.NotificationWebhook, `{"url":"https://a.b/hook"}`, true},
		{store.NotificationWebhook, `{"url":"ftp://a.b"}`, false},
		{store.NotificationWebhook, `{"url":""}`, false},
		{store.NotificationWebhook, `not json`, false},
		{store.NotificationWebhook, strings.Repeat("x", 30000), false},
		{store.NotificationEmail, `{"smtp_host":"s","from":"a@b","to":["c@d"]}`, true},
		{store.NotificationEmail, `{"smtp_host":"s","from":"a@b","to":[]}`, false},
		{store.NotificationEmail, `{"smtp_host":"","from":"a@b","to":["c@d"]}`, false},
		{store.NotificationEmail, `{"smtp_host":"s","smtp_port":70000,"from":"a@b","to":["c@d"]}`, false},
		{store.NotificationEmail, `{"smtp_host":"s","from":"a@b","to":["no-at"]}`, false},
		{store.NotificationKind("bogus"), `{"url":"https://a"}`, false},
	}
	for i, tc := range cases {
		_, err := ParseNotificationConfig(tc.kind, tc.raw)
		if tc.ok && err != nil {
			t.Fatalf("case %d: unexpected error %v", i, err)
		}
		if !tc.ok && err == nil {
			t.Fatalf("case %d: expected error", i)
		}
	}
	// email 缺省端口补 465。
	cfg, err := ParseNotificationConfig(store.NotificationEmail, `{"smtp_host":"s","from":"a@b","to":["c@d"]}`)
	if err != nil || cfg.SMTPPort != 465 {
		t.Fatalf("default port: cfg=%+v err=%v", cfg, err)
	}
}

func TestConfigHintNoSecretLeak(t *testing.T) {
	cfg, _ := ParseNotificationConfig(store.NotificationWebhook, `{"url":"https://open.feishu.cn/hook/x?k=1","secret":"verylongsecret"}`)
	hint := ConfigHint(store.NotificationWebhook, cfg)
	if strings.Contains(hint, "verylongsecret") {
		t.Fatalf("hint leaks secret: %q", hint)
	}
	if !strings.Contains(hint, "open.feishu.cn") {
		t.Fatalf("hint should carry host: %q", hint)
	}
	mail, _ := ParseNotificationConfig(store.NotificationEmail, `{"smtp_host":"smtp.x","from":"a@b","to":["c@d","e@f"]}`)
	if h := ConfigHint(store.NotificationEmail, mail); !strings.Contains(h, "2 收件人") {
		t.Fatalf("email hint = %q", h)
	}
}

func TestRenderTemplates(t *testing.T) {
	task := testNotifyTask()
	if s := RenderFeishuText(task); !strings.HasPrefix(s, "[OpsMind][warning] cpu-high web-1") {
		t.Fatalf("feishu text = %q", s)
	}
	if s := RenderDingtalkText(task); !strings.Contains(s, "cpu-high") || !strings.Contains(s, "web-1") {
		t.Fatalf("dingtalk text = %q", s)
	}
	if s := RenderEmailSubject(task); s != "[OpsMind][warning] cpu-high web-1" {
		t.Fatalf("email subject = %q", s)
	}
	if s := RenderEmailBody(task); !strings.Contains(s, "cpu_pct = 95.5") {
		t.Fatalf("email body = %q", s)
	}
	body, err := RenderWebhookBody(task)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil || m["event"] != "alert" {
		t.Fatalf("webhook body = %s", body)
	}
	// RFC5322：CRLF 行尾 + 头部。
	msg := buildRFC5322("a@b", []string{"c@d"}, "subj", "line1\nline2")
	if !strings.Contains(msg, "Subject: subj\r\n") || !strings.Contains(msg, "line1\r\nline2") {
		t.Fatalf("rfc5322 = %q", msg)
	}
}
